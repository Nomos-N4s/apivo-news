package earnings_test

// What the machine does in what order, and what it refuses to do (T069, D7).
//
// The properties worth proving here are all about ORDER and about what does
// NOT happen: that nothing is recorded for a posting that failed, that
// nothing is posted for a move the design forbids, and that a move whose
// entry has already changed hands is refused before anything is written.

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const receivable = "network-receivable"

// fakeLedger records what it was asked to post so a case can assert the
// accounts and the key, and can refuse on demand.
type fakeLedger struct {
	posted    []wallet.Transfer
	ensured   []wallet.AccountRef
	postErr   error
	ensureErr error
}

func (f *fakeLedger) EnsureAccount(_ context.Context, ref wallet.AccountRef, _ money.Currency) (wallet.LedgerAccountID, error) {
	f.ensured = append(f.ensured, ref)
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return wallet.LedgerAccountID("acct:" + ref.String()), nil
}

// The reference is derived from the key, which is the ledger's own
// idempotency contract in one line: the same key answers the same transfer,
// and a different key answers a different one. A constant here would hide
// exactly the difference D8's key exists to make.
func (f *fakeLedger) Post(_ context.Context, transfer wallet.Transfer) (wallet.TransferRef, error) {
	if f.postErr != nil {
		return "", f.postErr
	}
	f.posted = append(f.posted, transfer)
	return wallet.TransferRef("transfer:" + transfer.IdempotencyKey), nil
}

// The two reads the port declares and this file never makes. They are here
// because the interface is the interface; a machine that moved money would be
// wrong to read a balance and decide on it, since the ledger is the authority
// and the decision belongs to whatever asked for the move.
func (f *fakeLedger) Balance(context.Context, wallet.LedgerAccountID, money.Currency) (money.Amount, error) {
	return money.Amount{}, nil
}

func (f *fakeLedger) History(context.Context, wallet.LedgerAccountID, wallet.Window) (iter.Seq2[wallet.Posting, error], error) {
	return nil, nil
}

// fakeEntries records the writes, and can report the entry as having moved.
type fakeEntries struct {
	row         store.CashbackEntry
	moved       bool
	moveErr     error
	transitions []store.RecordTransitionParams
	links       []store.LinkLedgerTransferParams
	moves       []store.MoveEntryParams
	created     store.CreateEntryParams
	creations   int
}

func (f *fakeEntries) CreateEntry(_ context.Context, arg store.CreateEntryParams) (store.CashbackEntry, error) {
	f.created = arg
	f.creations++
	row := f.row
	row.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	return row, nil
}

func (f *fakeEntries) MoveEntry(_ context.Context, arg store.MoveEntryParams) (store.CashbackEntry, error) {
	f.moves = append(f.moves, arg)
	switch {
	case f.moved:
		return store.CashbackEntry{}, pgx.ErrNoRows
	case f.moveErr != nil:
		return store.CashbackEntry{}, f.moveErr
	}
	row := f.row
	row.State = arg.ToState
	row.HoldRule = arg.HoldRule
	return row, nil
}

// The stored row echoed back, because the statement returns the row and what
// is announced is read from it. A fake that returned a bare id would let this
// package announce a move naming no entry and no transfer, and nothing here
// would notice.
func (f *fakeEntries) RecordTransition(_ context.Context, arg store.RecordTransitionParams) (store.CashbackEntryTransition, error) {
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

// recordedAt is the instant the fake's transition rows carry, fixed so a case
// can assert what was announced.
var recordedAt = time.Date(2026, time.March, 1, 9, 30, 0, 0, time.UTC)

func (f *fakeEntries) LinkLedgerTransfer(_ context.Context, arg store.LinkLedgerTransferParams) (store.CashbackLedgerLink, error) {
	f.links = append(f.links, arg)
	return store.CashbackLedgerLink{}, nil
}

// anEntry is a stored row a move can be applied to.
func anEntry(member uuid.UUID, state earnings.State) store.CashbackEntry {
	return store.CashbackEntry{
		ID:                   pgtype.UUID{Bytes: uuid.New(), Valid: true},
		AccountID:            pgtype.UUID{Bytes: member, Valid: true},
		BrandID:              "apivo-de",
		NetworkTransactionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		State:                string(state),
		AmountMinor:          3000,
		Currency:             "EUR",
	}
}

// machine builds the state machine over the given parts.
func machine(t *testing.T, entries earnings.EntryStore, ledger wallet.Ledger) *earnings.Entries {
	t.Helper()
	m, err := earnings.NewEntries(entries, ledger, receivable)
	if err != nil {
		t.Fatalf("NewEntries(): %v", err)
	}
	return m
}

// aMove is a lawful pending-to-confirmed move of the given entry.
func aMove(t *testing.T, row store.CashbackEntry) earnings.Move {
	t.Helper()
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return earnings.Move{
		Entry:  uuid.UUID(row.ID.Bytes),
		From:   earnings.StatePending,
		To:     earnings.StateConfirmed,
		Member: uuid.UUID(row.AccountID.Bytes),
		Amount: amount,
		Cause:  uuid.New(),
	}
}

// TestAConfirmationMovesTheMembersOwnMoneyBetweenTheirOwnBuckets is the
// property that makes a confirmation not a second credit: both sides of the
// transfer are the member's, so their total does not change - only which
// bucket counts toward the withdrawal threshold.
func TestAConfirmationMovesTheMembersOwnMoneyBetweenTheirOwnBuckets(t *testing.T) {
	t.Parallel()

	member := uuid.New()
	row := anEntry(member, earnings.StatePending)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}

	entry, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, aMove(t, row))
	if err != nil {
		t.Fatalf("Apply(): %v", err)
	}
	if entry.State != earnings.StateConfirmed {
		t.Errorf("the entry is %s, want confirmed", entry.State)
	}

	if len(ledger.posted) != 1 {
		t.Fatalf("posted %d transfer(s), want one", len(ledger.posted))
	}
	transfer := ledger.posted[0]
	if len(transfer.Postings) != 2 {
		t.Fatalf("the transfer has %d posting(s), want two", len(transfer.Postings))
	}
	// Zero-sum, at the only layer that assembles the postings (C-1).
	total, err := transfer.Postings[0].Amount.Add(transfer.Postings[1].Amount)
	if err != nil {
		t.Fatalf("summing the postings: %v", err)
	}
	if !total.IsZero() {
		t.Errorf("the transfer sums to %s, want zero", total)
	}
	for _, ref := range ledger.ensured {
		if _, _, ok := ref.Member(); !ok {
			t.Errorf("a confirmation touched %s, which is not the member's own account", ref)
		}
	}
}

// TestNothingIsRecordedWhenTheTransferIsRefused is D7's other direction. A
// state recorded with no posting behind it is exactly the disagreement
// between the wallet and the ledger that C-1 exists to prevent.
func TestNothingIsRecordedWhenTheTransferIsRefused(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StatePending)
	entries := &fakeEntries{row: row}
	ledger := &fakeLedger{postErr: errors.New("ledger unreachable")}

	_, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, aMove(t, row))

	if !errors.Is(err, earnings.ErrNotPosted) {
		t.Fatalf("Apply() error = %v, want one wrapping %v", err, earnings.ErrNotPosted)
	}
	if len(entries.moves) != 0 || len(entries.transitions) != 0 || len(entries.links) != 0 {
		t.Errorf("a refused transfer wrote %d move(s), %d transition(s) and %d link(s), want none",
			len(entries.moves), len(entries.transitions), len(entries.links))
	}
}

// TestAnIllegalMoveNeverReachesTheLedger. The legality check is the one
// refusal that costs nothing, and a transition the design forbids should not
// reach the ledger even in a form that would be rolled back.
func TestAnIllegalMoveNeverReachesTheLedger(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StateConfirmed)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}
	move := aMove(t, row)
	move.From, move.To = earnings.StateConfirmed, earnings.StatePending

	_, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, move)

	var illegal earnings.ErrIllegalTransition
	if !errors.As(err, &illegal) {
		t.Fatalf("Apply() error = %v, want an illegal-transition error", err)
	}
	if len(ledger.ensured) != 0 || len(ledger.posted) != 0 {
		t.Errorf("an illegal move ensured %d account(s) and posted %d transfer(s), want none",
			len(ledger.ensured), len(ledger.posted))
	}
}

// TestAnEntryThatMovedFirstIsRefusedWithoutRecording is the concurrency
// guard. Two pollers, or a poller and an operator, can decide different
// futures for one entry at the same instant; the second is told so, and no
// transition is recorded for a move the entry did not make.
func TestAnEntryThatMovedFirstIsRefusedWithoutRecording(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StatePending)
	entries := &fakeEntries{row: row, moved: true}
	ledger := &fakeLedger{}

	_, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, aMove(t, row))

	if !errors.Is(err, earnings.ErrEntryMoved) {
		t.Fatalf("Apply() error = %v, want one wrapping %v", err, earnings.ErrEntryMoved)
	}
	if len(entries.transitions) != 0 || len(entries.links) != 0 {
		t.Errorf("an entry that had already moved recorded %d transition(s) and %d link(s), want none",
			len(entries.transitions), len(entries.links))
	}
}

// TestEveryTransitionIsLinkedToItsPosting covers C-7's seam. A transition
// with no link is a posting nobody can find from the domain side.
func TestEveryTransitionIsLinkedToItsPosting(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StatePending)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}

	if _, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, aMove(t, row)); err != nil {
		t.Fatalf("Apply(): %v", err)
	}
	if len(entries.transitions) != 1 || len(entries.links) != 1 {
		t.Fatalf("recorded %d transition(s) and %d link(s), want one of each",
			len(entries.transitions), len(entries.links))
	}
	if entries.transitions[0].LedgerTransferRef != entries.links[0].LedgerTransferRef {
		t.Errorf("the transition names transfer %q and its link names %q",
			entries.transitions[0].LedgerTransferRef, entries.links[0].LedgerTransferRef)
	}
	if entries.transitions[0].LedgerTransferRef == "" {
		t.Error("the transition names no transfer, which the schema refuses")
	}
}

// TestTheKeyDistinguishesARepeatedMove is D8 at this layer. An entry can go
// held to pending, back to held, and out again; a key of entry and states
// alone would make the second move a replay of the first, which the ledger
// answers by recording nothing at all - so the member's money would not move.
func TestTheKeyDistinguishesARepeatedMove(t *testing.T) {
	t.Parallel()

	member := uuid.New()
	row := anEntry(member, earnings.StateHeld)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}
	m := machine(t, entries, ledger)

	out := &fakeOutbox{}
	release := aMove(t, row)
	release.From, release.To = earnings.StateHeld, earnings.StatePending
	if _, err := m.Apply(t.Context(), out, release); err != nil {
		t.Fatalf("the first release: %v", err)
	}
	// The same move again, caused by a different fact - which is what a
	// second hold and release actually is.
	release.Cause = uuid.New()
	if _, err := m.Apply(t.Context(), out, release); err != nil {
		t.Fatalf("the second release: %v", err)
	}

	if len(ledger.posted) != 2 {
		t.Fatalf("posted %d transfer(s), want two", len(ledger.posted))
	}
	if ledger.posted[0].IdempotencyKey == ledger.posted[1].IdempotencyKey {
		t.Errorf("both moves share the key %q, so the ledger would treat the second as a replay",
			ledger.posted[0].IdempotencyKey)
	}
}

// TestPayingOutIsRefusedByName. It is a lawful transition, but the
// destination is chosen by the withdrawal that claimed the entry and an entry
// does not know which one that is. Refusing keeps this package from inventing
// an account name to fill a gap it cannot see.
func TestPayingOutIsRefusedByName(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StateReserved)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}
	move := aMove(t, row)
	move.From, move.To = earnings.StateReserved, earnings.StatePaid

	_, err := machine(t, entries, ledger).Apply(t.Context(), &fakeOutbox{}, move)

	if !errors.Is(err, earnings.ErrNotThisPackagesToPost) {
		t.Fatalf("Apply() error = %v, want one wrapping %v", err, earnings.ErrNotThisPackagesToPost)
	}
	if len(ledger.posted) != 0 {
		t.Errorf("a payout posted %d transfer(s) here, want none", len(ledger.posted))
	}
}

// TestAMachineIsRefusedWithoutItsParts covers the construction refusals.
// Either part discovered later is discovered with a transfer already posted
// or an entry already created.
func TestAMachineIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	if _, err := earnings.NewEntries(nil, &fakeLedger{}, receivable); !errors.Is(err, earnings.ErrNoEntryStore) {
		t.Errorf("NewEntries(nil, ...) error = %v, want %v", err, earnings.ErrNoEntryStore)
	}
	if _, err := earnings.NewEntries(&fakeEntries{}, nil, receivable); !errors.Is(err, earnings.ErrNoLedger) {
		t.Errorf("NewEntries(_, nil, _) error = %v, want %v", err, earnings.ErrNoLedger)
	}
	if _, err := earnings.NewEntries(&fakeEntries{}, &fakeLedger{}, ""); !errors.Is(err, earnings.ErrNoReceivable) {
		t.Errorf("NewEntries(_, _, \"\") error = %v, want %v", err, earnings.ErrNoReceivable)
	}
}

// TestAMoveIsAnnouncedBesideItself is the atomicity guarantee at this layer:
// the event is appended through the same handle the move was written with,
// so there is no code path that commits a state change without its event
// (contracts/events.md, T076).
func TestAMoveIsAnnouncedBesideItself(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StatePending)
	entries, ledger, out := &fakeEntries{row: row}, &fakeLedger{}, &fakeOutbox{}
	move := aMove(t, row)

	if _, err := machine(t, entries, ledger).Apply(t.Context(), out, move); err != nil {
		t.Fatalf("Apply(): %v", err)
	}

	announced := out.only(t, earnings.TypeEntryStateChanged)
	if announced.Subject != move.Entry.String() {
		t.Errorf("the event is about %q, want the entry %s", announced.Subject, move.Entry)
	}
	// The transfer the ledger answered, carried through to the stream: the
	// key and the payload both name it, which is what lets a consumer follow
	// the money without reading this module's tables.
	posted := ledger.posted[0].IdempotencyKey
	if want := "transfer:" + posted; announced.Payload["ledger_transfer_ref"] != want {
		t.Errorf("the event names transfer %v, want %q", announced.Payload["ledger_transfer_ref"], want)
	}
	if announced.Key != earnings.TypeEntryStateChanged+":transfer:"+posted {
		t.Errorf("the event is keyed %q, want the type and the transfer", announced.Key)
	}
	if announced.Payload["from"] != string(earnings.StatePending) || announced.Payload["to"] != string(earnings.StateConfirmed) {
		t.Errorf("the event says %v to %v, want pending to confirmed",
			announced.Payload["from"], announced.Payload["to"])
	}
}

// TestAMoveThatCannotBeAnnouncedFails. Swallowing the refusal would commit a
// moved entry with no event - the one thing the contract says no code path
// may do - and the append shares the caller's transaction, so a caller that
// carried on would commit nothing while reporting success.
func TestAMoveThatCannotBeAnnouncedFails(t *testing.T) {
	t.Parallel()

	row := anEntry(uuid.New(), earnings.StatePending)
	entries, ledger := &fakeEntries{row: row}, &fakeLedger{}
	out := &fakeOutbox{err: errOutboxRefused}

	_, err := machine(t, entries, ledger).Apply(t.Context(), out, aMove(t, row))

	if !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Fatalf("Apply() error = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
}
