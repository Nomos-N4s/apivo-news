package earnings_test

// The multi-entry stand-in a reservation needs, and the cases about what one
// transfer over many entries must do (T091, D9).
//
// fakeEntries in statemachine_test.go holds ONE row, which is right for a
// machine that moves one entry at a time. A reservation moves a set, and the
// property under test is what happens ACROSS the set - one transfer, one
// reference on every transition - so it needs a store that can hold one.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakeReservationStore holds a member's confirmed rows and moves them.
type fakeReservationStore struct {
	rows        []store.CashbackEntry
	readErr     error
	reads       []store.ConfirmedEntriesForParams
	moves       []store.MoveEntryParams
	transitions []store.RecordTransitionParams
	links       []store.LinkLedgerTransferParams
	// gone are entries the store reports as having left the state the caller
	// read them in, standing in for a row that moved between the read and
	// the write.
	gone map[uuid.UUID]bool
}

func (f *fakeReservationStore) ConfirmedEntriesFor(_ context.Context, arg store.ConfirmedEntriesForParams) ([]store.CashbackEntry, error) {
	f.reads = append(f.reads, arg)
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.rows, nil
}

func (f *fakeReservationStore) MoveEntry(_ context.Context, arg store.MoveEntryParams) (store.CashbackEntry, error) {
	f.moves = append(f.moves, arg)
	if f.gone[uuid.UUID(arg.ID.Bytes)] {
		return store.CashbackEntry{}, pgx.ErrNoRows
	}
	for _, row := range f.rows {
		if row.ID != arg.ID {
			continue
		}
		if row.State != arg.FromState {
			return store.CashbackEntry{}, pgx.ErrNoRows
		}
		row.State = arg.ToState
		row.HoldRule = arg.HoldRule
		return row, nil
	}
	return store.CashbackEntry{}, pgx.ErrNoRows
}

func (f *fakeReservationStore) RecordTransition(_ context.Context, arg store.RecordTransitionParams) (store.CashbackEntryTransition, error) {
	f.transitions = append(f.transitions, arg)
	return store.CashbackEntryTransition{
		ID:                pgtype.UUID{Bytes: uuid.New(), Valid: true},
		EntryID:           arg.EntryID,
		FromState:         arg.FromState,
		ToState:           arg.ToState,
		LedgerTransferRef: arg.LedgerTransferRef,
		Reason:            arg.Reason,
		ActorID:           arg.ActorID,
		OccurredAt:        pgtype.Timestamptz{Time: recordedAt, Valid: true},
	}, nil
}

func (f *fakeReservationStore) LinkLedgerTransfer(_ context.Context, arg store.LinkLedgerTransferParams) (store.CashbackLedgerLink, error) {
	f.links = append(f.links, arg)
	return store.CashbackLedgerLink{}, nil
}

// CreateEntry is here because EntryStore declares it; reserving never opens
// an entry, and a case that reached it is a case that went wrong.
func (f *fakeReservationStore) CreateEntry(context.Context, store.CreateEntryParams) (store.CashbackEntry, error) {
	return store.CashbackEntry{}, errors.New("reserving must not open an entry")
}

// confirmedRows builds a member's confirmed rows in the order the locked read
// returns them: oldest first.
func confirmedRows(member uuid.UUID, minors ...int64) []store.CashbackEntry {
	rows := make([]store.CashbackEntry, 0, len(minors))
	for _, minor := range minors {
		row := anEntry(member, earnings.StateConfirmed)
		row.AmountMinor = minor
		rows = append(rows, row)
	}
	return rows
}

// asEntries reads the rows back the way Confirmed would, so a case can hand
// Reserve exactly what the machine would have handed it.
func asEntries(t *testing.T, rows []store.CashbackEntry) []earnings.Entry {
	t.Helper()
	entries := make([]earnings.Entry, 0, len(rows))
	for _, row := range rows {
		amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
		if err != nil {
			t.Fatalf("money.New(%d, %s): %v", row.AmountMinor, row.Currency, err)
		}
		entries = append(entries, earnings.Entry{
			ID:     uuid.UUID(row.ID.Bytes),
			Member: uuid.UUID(row.AccountID.Bytes),
			Brand:  row.BrandID,
			State:  earnings.State(row.State),
			Amount: amount,
		})
	}
	return entries
}

// TestAReservationPostsOneTransferForEveryEntry is C-7's requirement stated
// as a test: migration 0016 finds the entries a payout paid by matching them
// against the request's single reserved_transfer_ref, so every transition
// must carry that one reference.
func TestAReservationPostsOneTransferForEveryEntry(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 1000, 2000, 3000)
	entries := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}
	out := &fakeOutbox{}
	request := uuid.New()

	reserved, err := machine(t, entries, ledger).Reserve(t.Context(), out, entries, asEntries(t, rows), request)
	if err != nil {
		t.Fatalf("Reserve(): %v", err)
	}

	if len(ledger.posted) != 1 {
		t.Fatalf("posted %d transfer(s) for three entries, want one", len(ledger.posted))
	}
	if len(entries.transitions) != 3 {
		t.Fatalf("recorded %d transition(s), want one per entry", len(entries.transitions))
	}
	for _, transition := range entries.transitions {
		if transition.LedgerTransferRef != string(reserved.Transfer) {
			t.Errorf("a transition names transfer %q, want the reservation's %q",
				transition.LedgerTransferRef, reserved.Transfer)
		}
	}
	if want := int64(6000); reserved.Amount.Minor != want {
		t.Errorf("reserved %s, want the sum of the three entries %d", reserved.Amount, want)
	}
	// One event per entry, not one per transfer: the key carries both, which
	// is what keeps the second entry from colliding with the first.
	if announced := out.of(t, earnings.TypeEntryStateChanged); len(announced) != 3 {
		t.Errorf("announced %d move(s), want one per entry", len(announced))
	}
}

// TestAReservationIsKeyedOnTheRequest. D8: a retry must re-derive the same
// key, and the request id is the one fact that cannot move under a retry -
// the member's confirmed set can.
func TestAReservationIsKeyedOnTheRequest(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 1000)
	entries := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}
	request := uuid.New()

	if _, err := machine(t, entries, ledger).Reserve(t.Context(), &fakeOutbox{}, entries, asEntries(t, rows), request); err != nil {
		t.Fatalf("Reserve(): %v", err)
	}

	if len(ledger.posted) != 1 {
		t.Fatalf("posted %d transfer(s), want one", len(ledger.posted))
	}
	if key := ledger.posted[0].IdempotencyKey; key != "withdrawal:"+request.String()+":reserve" {
		t.Errorf("posted under key %q, want one derived from the request %s", key, request)
	}
}

// TestAReservationMovesConfirmedToReserved. The accounts are the ones
// postingsFor gives a confirmed-to-reserved move, and both are the member's:
// a reservation changes which bucket money sits in, not how much they have.
func TestAReservationMovesConfirmedToReserved(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 2500)
	entries := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}

	if _, err := machine(t, entries, ledger).Reserve(t.Context(), &fakeOutbox{}, entries, asEntries(t, rows), uuid.New()); err != nil {
		t.Fatalf("Reserve(): %v", err)
	}

	if len(ledger.ensured) != 2 {
		t.Fatalf("ensured %d account(s), want the two stages", len(ledger.ensured))
	}
	from, to := ledger.ensured[0], ledger.ensured[1]
	if got, stage, ok := from.Member(); !ok || got != member || stage != wallet.StageConfirmed {
		t.Errorf("took from %s, want %s's confirmed", from, member)
	}
	if got, stage, ok := to.Member(); !ok || got != member || stage != wallet.StageReserved {
		t.Errorf("moved to %s, want %s's reserved", to, member)
	}
	for _, move := range entries.moves {
		if move.FromState != string(earnings.StateConfirmed) || move.ToState != string(earnings.StateReserved) {
			t.Errorf("moved %s to %s, want confirmed to reserved", move.FromState, move.ToState)
		}
	}
}

// TestAReservationOfAnEntryThatIsNotConfirmedIsRefused. Checked before the
// ledger is asked for anything, so a caller that passed a pending entry gets
// a refusal rather than a posted transfer it has to undo.
func TestAReservationOfAnEntryThatIsNotConfirmedIsRefused(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 1000, 2000)
	entries := asEntries(t, rows)
	entries[1].State = earnings.StatePending
	store := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}

	_, err := machine(t, store, ledger).Reserve(t.Context(), &fakeOutbox{}, store, entries, uuid.New())
	var illegal earnings.ErrIllegalTransition
	if !errors.As(err, &illegal) {
		t.Fatalf("Reserve() = %v, want a %T", err, illegal)
	}
	if len(ledger.posted) != 0 {
		t.Errorf("posted %d transfer(s) for a refused reservation, want none", len(ledger.posted))
	}
}

// TestAReservationIsOneMembersMoney. One transfer moves one member's balance
// between one pair of stage accounts, so a set spanning two members would
// post somebody else's money into this member's reserved account.
func TestAReservationIsOneMembersMoney(t *testing.T) {
	rows := confirmedRows(uuid.New(), 1000)
	entries := asEntries(t, rows)
	entries = append(entries, asEntries(t, confirmedRows(uuid.New(), 2000))...)
	store := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}

	if _, err := machine(t, store, ledger).Reserve(t.Context(), &fakeOutbox{}, store, entries, uuid.New()); err == nil {
		t.Fatal("Reserve() across two members succeeded, want a refusal")
	}
	if len(ledger.posted) != 0 {
		t.Errorf("posted %d transfer(s) for a refused reservation, want none", len(ledger.posted))
	}
}

// TestAReservationWithoutACauseIsRefused. The cause is what the key is
// derived from, so a reservation without one is a reservation whose retry
// would post the money a second time.
func TestAReservationWithoutACauseIsRefused(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 1000)
	store := &fakeReservationStore{rows: rows}
	ledger := &fakeLedger{}

	_, err := machine(t, store, ledger).Reserve(t.Context(), &fakeOutbox{}, store, asEntries(t, rows), uuid.Nil)
	if !errors.Is(err, earnings.ErrNoReservationCause) {
		t.Fatalf("Reserve() = %v, want one wrapping %v", err, earnings.ErrNoReservationCause)
	}
	if len(ledger.posted) != 0 {
		t.Errorf("posted %d transfer(s), want none", len(ledger.posted))
	}
}

// TestAnEntryThatLeftConfirmedStopsTheReservation. ConfirmedEntriesFor locks
// the rows, so this should not happen - and if it does, the conditional move
// says so rather than recording a transition from a state the entry was not
// in.
func TestAnEntryThatLeftConfirmedStopsTheReservation(t *testing.T) {
	member := uuid.New()
	rows := confirmedRows(member, 1000, 2000)
	entries := asEntries(t, rows)
	store := &fakeReservationStore{rows: rows, gone: map[uuid.UUID]bool{entries[1].ID: true}}
	ledger := &fakeLedger{}

	_, err := machine(t, store, ledger).Reserve(t.Context(), &fakeOutbox{}, store, entries, uuid.New())
	if !errors.Is(err, earnings.ErrEntryMoved) {
		t.Fatalf("Reserve() = %v, want one wrapping %v", err, earnings.ErrEntryMoved)
	}
}

// TestConfirmedReadsTheMembersOwnMoney. The locked read is narrowed on the
// member and the currency, and a case that let either drift would let a
// withdrawal reserve somebody else's balance.
func TestConfirmedReadsTheMembersOwnMoney(t *testing.T) {
	member := uuid.New()
	store := &fakeReservationStore{rows: confirmedRows(member, 1000, 2000)}

	held, err := machine(t, store, &fakeLedger{}).Confirmed(t.Context(), store, member, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("Confirmed(): %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("read %d entries, want two", len(held))
	}
	if len(store.reads) != 1 {
		t.Fatalf("made %d read(s), want one", len(store.reads))
	}
	read := store.reads[0]
	if uuid.UUID(read.AccountID.Bytes) != member {
		t.Errorf("read %s's confirmed money, want %s's", uuid.UUID(read.AccountID.Bytes), member)
	}
	if read.Currency != "EUR" {
		t.Errorf("read in %s, want EUR", read.Currency)
	}
	total, err := earnings.Total(held, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("Total(): %v", err)
	}
	if total.Minor != 3000 {
		t.Errorf("confirmed total = %s, want 30.00", total)
	}
}
