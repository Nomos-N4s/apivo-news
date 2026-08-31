// Moving an entry, and the two writes that carry every move (T069, D7).
//
// D7 in one line: no state is ever recorded without its posting, and no
// posting is ever written without a state. entry_transition.ledger_transfer_ref
// is not null, so the schema enforces one direction of that; this file
// enforces the other by never posting without recording.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoEntryStore reports a machine built with nowhere to record what it
	// moved. Refused at construction, because discovered later it is
	// discovered with a transfer already posted.
	ErrNoEntryStore = errors.New("earnings: an entry machine needs a store to write to")
	// ErrNoLedger reports a machine built with nothing to move money in,
	// refused for the same reason and at the same moment.
	ErrNoLedger = errors.New("earnings: an entry machine needs a ledger to post in")
	// ErrNoReceivable reports a machine with no account to pay earnings out
	// of. Refused rather than defaulted: a house account nobody named is an
	// account nobody meant to open, and money credited from one would be
	// money the business cannot find.
	ErrNoReceivable = errors.New("earnings: an entry machine needs the account earnings are paid out of")
	// ErrEntryMoved reports a move whose entry was no longer in the state the
	// caller read it in. Ordinary under concurrency and never a defect: two
	// pollers, or a poller and an operator, can decide different futures for
	// one entry at the same instant, and this is the second one being told
	// so - before the ledger has been asked to move anything.
	ErrEntryMoved = errors.New("earnings: the entry is no longer in the state it was read in")
	// ErrNotPosted reports a transfer the ledger refused. The entry is
	// unchanged when this is returned: nothing is recorded for a posting that
	// did not happen.
	ErrNotPosted = errors.New("earnings: the transfer could not be posted")
	// ErrNotRecorded reports a transition the database refused after the
	// ledger accepted the transfer. It wraps the refusal unchanged, and a
	// caller must not swallow it: the money HAS moved and the retry that
	// records it is the only thing that can make the two agree again.
	ErrNotRecorded = errors.New("earnings: the transfer posted but the transition could not be recorded")
)

// EntryStore is the five statements this file needs, named here rather than
// taken as *store.Queries so the caller keeps the transaction boundary. That
// is load-bearing: the transition, the link and the entry's new state are one
// commit or none.
type EntryStore interface {
	CreateEntry(ctx context.Context, arg store.CreateEntryParams) (store.CashbackEntry, error)
	MoveEntry(ctx context.Context, arg store.MoveEntryParams) (store.CashbackEntry, error)
	RecordTransition(ctx context.Context, arg store.RecordTransitionParams) (store.CashbackEntryTransition, error)
	LinkLedgerTransfer(ctx context.Context, arg store.LinkLedgerTransferParams) (store.CashbackLedgerLink, error)
}

// Entries moves entries through their lifecycle, writing a ledger transfer
// and an entry_transition for every move.
type Entries struct {
	store      EntryStore
	ledger     wallet.Ledger
	receivable string
}

// NewEntries builds the machine, refusing one that is missing a part.
func NewEntries(entries EntryStore, ledger wallet.Ledger, receivable string) (*Entries, error) {
	switch {
	case entries == nil:
		return nil, ErrNoEntryStore
	case ledger == nil:
		return nil, ErrNoLedger
	case receivable == "":
		return nil, ErrNoReceivable
	}
	return &Entries{store: entries, ledger: ledger, receivable: receivable}, nil
}

// Move is one requested transition, and everything the two writes need.
type Move struct {
	// Entry is the entry to move, and From is the state it was read in.
	// Both, because the state is what makes the write conditional: passing
	// the id alone would let a caller move an entry it never looked at.
	Entry uuid.UUID
	From  State
	To    State
	// Member owns the entry, and Amount is what it holds. Read from the
	// entry rather than supplied would be one fewer argument and one more
	// read inside a transaction the caller is holding open.
	Member uuid.UUID
	Amount money.Amount
	// Cause is the domain fact that made this move happen: the superseding
	// report a network sent, or the operator action a human took. It is what
	// the idempotency key is derived from (D8), so it must be the same value
	// on a retry - a key that cannot be re-derived is a key a retry cannot
	// reuse, which is how a member gets paid twice.
	Cause uuid.UUID
	// Reason is free text recorded with the transition, and Actor the
	// operator who caused it where a human did.
	Reason string
	Actor  uuid.UUID
	// HoldRule names the rule holding the entry when To is held, and is
	// ignored otherwise. The schema requires a held entry to name one and a
	// non-held entry to name none, in the same row.
	HoldRule string
}

// Apply moves the entry, posting the transfer first and recording it second.
//
// THAT ORDER IS FORCED and it is worth saying why, because the other one
// looks safer. entry_transition.ledger_transfer_ref is not null, so a
// transition cannot be written before the transfer exists to be named. What
// that leaves is the window between a posted transfer and an unrecorded
// transition, and it is survivable precisely because the transfer is posted
// under a key derived from the cause: the retry re-posts, the ledger returns
// the original reference and records nothing, and the transition is written
// against the same transfer. The reverse order has no such recovery - a
// recorded transition naming a transfer that was never posted is a state with
// no money behind it, which is the disagreement C-1 exists to prevent.
//
// The legality check runs before either write. It is cheap, it is the one
// refusal that costs nothing, and a transition the design forbids should
// never reach the ledger even in a form that would be rolled back.
func (e *Entries) Apply(ctx context.Context, move Move) (Entry, error) {
	if !CanFollow(move.From, move.To) {
		return Entry{}, ErrIllegalTransition{From: move.From, To: move.To}
	}
	where, err := postingsFor(move.Member, move.From, move.To, e.receivable)
	if err != nil {
		return Entry{}, err
	}

	ref, err := e.post(ctx, where, move.Amount, idempotencyKey(move.Entry, move.Cause, move.To))
	if err != nil {
		return Entry{}, err
	}

	moved, err := e.store.MoveEntry(ctx, store.MoveEntryParams{
		ID:        pgUUID(move.Entry),
		FromState: string(move.From),
		ToState:   string(move.To),
		HoldRule:  pgTextOrNull(holdRuleFor(move)),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The entry left the state the caller read it in. The transfer is
		// posted and this returns without recording it, which is the one
		// place that is right: recording a transition the entry did not make
		// would be worse than a posting whose retry will find its transition.
		return Entry{}, fmt.Errorf("%w: %s was not %s", ErrEntryMoved, move.Entry, move.From)
	case err != nil:
		return Entry{}, fmt.Errorf("%w: moving %s: %w", ErrNotRecorded, move.Entry, err)
	}

	if err := e.record(ctx, move.Entry, move.From, move.To, ref, move.Reason, move.Actor); err != nil {
		return Entry{}, err
	}
	return entryFrom(moved)
}

// post moves the amount between the two accounts under the given key.
func (e *Entries) post(ctx context.Context, where movement, amount money.Amount, key string) (wallet.TransferRef, error) {
	source, err := e.ledger.EnsureAccount(ctx, where.from, amount.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNotPosted, where.from, err)
	}
	target, err := e.ledger.EnsureAccount(ctx, where.to, amount.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNotPosted, where.to, err)
	}
	debit, err := amount.Neg()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotPosted, err)
	}

	ref, err := e.ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: key,
		Postings: []wallet.Posting{
			{Account: source, Amount: debit},
			{Account: target, Amount: amount},
		},
		Reference: key,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotPosted, err)
	}
	return ref, nil
}

// record writes the transition and the link that joins it to its posting.
//
// Both, always: the link is what C-7 is answered from, and a transition
// without one is a posting nobody can find from the domain side.
func (e *Entries) record(ctx context.Context, entry uuid.UUID, from, to State, ref wallet.TransferRef, reason string, actor uuid.UUID) error {
	transition, err := e.store.RecordTransition(ctx, store.RecordTransitionParams{
		EntryID:           pgUUID(entry),
		FromState:         pgTextOrNull(string(from)),
		ToState:           string(to),
		LedgerTransferRef: string(ref),
		Reason:            pgTextOrNull(reason),
		ActorID:           pgUUIDOrNull(actor),
	})
	if err != nil {
		return fmt.Errorf("%w: recording %s becoming %s: %w", ErrNotRecorded, entry, to, err)
	}
	if _, err := e.store.LinkLedgerTransfer(ctx, store.LinkLedgerTransferParams{
		TransitionID:      transition.ID,
		EntryID:           pgUUID(entry),
		LedgerTransferRef: string(ref),
	}); err != nil {
		return fmt.Errorf("%w: linking %s to transfer %s: %w", ErrNotRecorded, entry, ref, err)
	}
	return nil
}

// holdRuleFor answers what the moved row's hold_rule must be.
//
// The schema requires a held entry to name a rule and every other entry to
// name none, checked on the row as a whole - so releasing has to clear it in
// the same statement that changes the state.
func holdRuleFor(move Move) string {
	if move.To == StateHeld {
		return move.HoldRule
	}
	return ""
}

// idempotencyKey derives the key for one transition from the facts that
// caused it (D8).
//
// Derived rather than random, because a random key per attempt defeats the
// purpose: a retry would mint a new one and post the money a second time.
// The cause is in it because an entry can make the same move twice - held to
// pending, back to held, and out again - and a key of entry and states alone
// would make the second move a replay of the first, which the ledger would
// answer by recording nothing at all.
func idempotencyKey(entry, cause uuid.UUID, to State) string {
	return fmt.Sprintf("entry:%s:cause:%s:to:%s", entry, cause, to)
}
