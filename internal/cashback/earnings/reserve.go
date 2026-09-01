// Reserving a member's confirmed money for a withdrawal (T091, D9, FR-050).
//
// D9 says the amount moves from confirmed to reserved when the request is
// MADE, before any human reviews it, because the double-spend window is
// between request and approval. This file is that move.
//
// It is not [Entries.Apply] repeated, and the reason is a join in migration
// 0016:
//
//	join cashback.entry_transition et
//	  on et.ledger_transfer_ref = wr.reserved_transfer_ref
//
// C-7 answers "which entries did this payout pay" by matching every reserving
// transition against the ONE transfer reference the withdrawal request
// stores. Apply posts a transfer per entry, so five entries reserved through
// it would carry five references, the request could name only one of them,
// and the provenance of a payout would be four fifths missing. So a
// reservation is one transfer covering every entry it claims, and this file
// exists to post it that way.
//
// The other half of the design is which entries a withdrawal takes, and it is
// [Covering]: oldest first, whole entries, stopping at the first one that
// covers the amount asked for. An entry is the unit money is owed in - it
// cites the network report that evidences it (C-2) - and half an entry cites
// half a report, so there is nothing to split. That means a member who asks
// for slightly less than a whole entry is reserved slightly more, which is
// why the contract returns reserved_amount as its own field rather than
// echoing what was asked.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNothingToReserve reports a reservation asked for nothing: a
	// non-positive amount, or one whose currency is not a currency. Refused
	// before any read, because "reserve nothing" has no answer that is not a
	// withdrawal request for zero.
	ErrNothingToReserve = errors.New("earnings: a reservation must ask for a positive amount")
	// ErrNoReservationCause reports a reservation with no withdrawal request
	// behind it. The cause is what the transfer's idempotency key is derived
	// from (D8), so a reservation without one is a reservation whose retry
	// would post the money a second time.
	ErrNoReservationCause = errors.New("earnings: a reservation must name the withdrawal request that caused it")
)

// ShortConfirmedBalance reports that a member's confirmed money does not
// reach what they asked to withdraw.
//
// It carries the arithmetic rather than a sentence about it, because the
// contract requires the shortfall to reach the member (US4 scenario 1) and a
// caller that had to re-derive it would be re-deriving it from a balance that
// may have moved since.
type ShortConfirmedBalance struct {
	// Want is what was asked for, Have what the member's confirmed entries
	// actually hold, and Short the difference. All three, because "you are
	// 4.00 short" and "you have 6.00 of the 10.00" answer different questions
	// and a member asks both.
	Want, Have, Short money.Amount
}

func (e ShortConfirmedBalance) Error() string {
	return fmt.Sprintf("earnings: confirmed balance is %s and %s was asked for, %s short",
		e.Have, e.Want, e.Short)
}

// Covering answers which entries a withdrawal of want takes.
//
// The rule is one sentence: oldest first, whole entries, stop at the first
// entry that brings the running total to want or past it. What comes back is
// therefore never less than want and never more than want plus the last
// entry, and it is exactly the set whose sum the reservation posts.
//
// Entries must arrive oldest first - ConfirmedEntriesFor orders them - and
// every one must be in want's currency; an entry of another currency is a
// different balance and is refused rather than skipped, because skipping it
// would silently answer about a set the caller did not pass.
//
// A pure function over a slice, deliberately: which money leaves a member's
// balance first is a rule a person should be able to read and a test should
// be able to exercise without a database or a ledger anywhere near it.
func Covering(entries []Entry, want money.Amount) ([]Entry, money.Amount, error) {
	if err := want.Validate(); err != nil {
		return nil, money.Amount{}, fmt.Errorf("%w: %w", ErrNothingToReserve, err)
	}
	if !want.IsPositive() {
		return nil, money.Amount{}, fmt.Errorf("%w: %s", ErrNothingToReserve, want)
	}
	zero, err := money.New(0, want.Currency)
	if err != nil {
		return nil, money.Amount{}, err
	}

	running := zero
	for i, entry := range entries {
		if entry.Amount.Currency != want.Currency {
			return nil, money.Amount{}, fmt.Errorf(
				"earnings: entry %s holds %s and %s was asked for: a withdrawal reserves one currency (C-6)",
				entry.ID, entry.Amount.Currency, want.Currency)
		}
		running, err = running.Add(entry.Amount)
		if err != nil {
			return nil, money.Amount{}, err
		}
		// At or past what was asked for: this entry is the last one taken,
		// and everything after it stays confirmed.
		if running.Minor >= want.Minor {
			return entries[:i+1], running, nil
		}
	}

	short, err := want.Sub(running)
	if err != nil {
		return nil, money.Amount{}, err
	}
	return nil, money.Amount{}, ShortConfirmedBalance{Want: want, Have: running, Short: short}
}

// ReservationStore is what reserving needs on top of [EntryStore]: the read
// that locks a member's confirmed entries. Named separately so a caller that
// only moves entries does not have to supply it.
type ReservationStore interface {
	EntryStore
	ConfirmedEntriesFor(ctx context.Context, arg store.ConfirmedEntriesForParams) ([]store.CashbackEntry, error)
}

// Reservation is what one withdrawal claimed.
type Reservation struct {
	// Transfer is the single ledger reference every reserved entry's
	// transition carries, and the one the withdrawal request stores as
	// reserved_transfer_ref. C-7 is answered by matching them.
	Transfer wallet.TransferRef
	// Entries are the entries reserved, oldest first, as they stand after
	// the move.
	Entries []Entry
	// Amount is what was actually reserved - the sum of Entries - which is
	// at least what was asked for and may exceed it by less than the last
	// entry. It is what the withdrawal request records and what the payout
	// pays.
	Amount money.Amount
}

// Reserve moves a member's oldest confirmed entries into reserved under one
// ledger transfer (D9).
//
// The order of the writes is [Entries.Apply]'s order and for its reason: the
// transfer is posted first because entry_transition.ledger_transfer_ref is not
// null, and the window between a posted transfer and an unrecorded transition
// is survivable because the key is derived from the cause, so a retry
// re-posts to the same reference rather than moving the money again.
//
// What this adds is that the transfer is posted ONCE for the whole set. Every
// entry is then moved and recorded against that one reference, which is what
// makes the C-7 join in 0016 return the whole payment rather than a fifth of
// it.
//
// db is the transaction the store was bound to. Every entry moved, every
// transition recorded and every event appended commit together or none of
// them do; a caller that passed a pool instead would be able to commit a
// reservation half-recorded.
func (e *Entries) Reserve(ctx context.Context, db events.RowQuerier, entries ReservationStore, member uuid.UUID, want money.Amount, cause uuid.UUID) (Reservation, error) {
	switch {
	case entries == nil:
		return Reservation{}, ErrNoEntryStore
	case cause == uuid.Nil:
		return Reservation{}, ErrNoReservationCause
	}
	if err := want.Validate(); err != nil {
		return Reservation{}, fmt.Errorf("%w: %w", ErrNothingToReserve, err)
	}

	confirmed, err := e.confirmed(ctx, entries, member, want.Currency)
	if err != nil {
		return Reservation{}, err
	}
	taking, total, err := Covering(confirmed, want)
	if err != nil {
		return Reservation{}, err
	}

	where, err := postingsFor(member, StateConfirmed, StateReserved, e.receivable)
	if err != nil {
		return Reservation{}, err
	}
	ref, err := e.post(ctx, where, total, reservationKey(cause))
	if err != nil {
		return Reservation{}, err
	}

	reserved := make([]Entry, 0, len(taking))
	for _, entry := range taking {
		moved, err := e.reserveOne(ctx, db, entries, entry, ref)
		if err != nil {
			return Reservation{}, err
		}
		reserved = append(reserved, moved)
	}
	return Reservation{Transfer: ref, Entries: reserved, Amount: total}, nil
}

// confirmed reads and locks the member's confirmed entries in one currency.
func (e *Entries) confirmed(ctx context.Context, entries ReservationStore, member uuid.UUID, currency money.Currency) ([]Entry, error) {
	rows, err := entries.ConfirmedEntriesFor(ctx, store.ConfirmedEntriesForParams{
		AccountID: pgUUID(member),
		Currency:  string(currency),
	})
	if err != nil {
		return nil, fmt.Errorf("earnings: reading what %s has confirmed in %s: %w", member, currency, err)
	}
	held := make([]Entry, 0, len(rows))
	for _, row := range rows {
		entry, err := entryFrom(row)
		if err != nil {
			return nil, err
		}
		held = append(held, entry)
	}
	return held, nil
}

// reserveOne moves one entry into reserved against the transfer already
// posted for the whole set.
//
// The conditional move is kept even though ConfirmedEntriesFor locked the row:
// the lock makes a concurrent withdrawal wait, and this makes a row that
// somehow left confirmed anyway say so rather than be recorded as having
// moved from a state it was not in.
func (e *Entries) reserveOne(ctx context.Context, db events.RowQuerier, entries ReservationStore, entry Entry, ref wallet.TransferRef) (Entry, error) {
	moved, err := entries.MoveEntry(ctx, store.MoveEntryParams{
		ID:        pgUUID(entry.ID),
		FromState: string(StateConfirmed),
		ToState:   string(StateReserved),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Entry{}, fmt.Errorf("%w: %s was not %s", ErrEntryMoved, entry.ID, StateConfirmed)
	case err != nil:
		return Entry{}, fmt.Errorf("%w: reserving %s: %w", ErrNotRecorded, entry.ID, err)
	}
	recorded, err := e.record(ctx, entry.ID, StateConfirmed, StateReserved, ref, "", uuid.Nil)
	if err != nil {
		return Entry{}, err
	}
	if err := e.announcer.StateChanged(ctx, db, recorded); err != nil {
		return Entry{}, err
	}
	return entryFrom(moved)
}

// reservationKey derives the transfer's idempotency key from the withdrawal
// request that caused it (D8).
//
// The request, and nothing else. Not the entries: a retry that re-read the
// member's confirmed set could find it changed - an entry confirmed in the
// meantime, one reversed - and a key derived from the set would then be a
// different key, which is a second reservation of the same withdrawal. The
// request id is the one fact that cannot move under a retry.
func reservationKey(cause uuid.UUID) string {
	return fmt.Sprintf("withdrawal:%s:reserve", cause)
}
