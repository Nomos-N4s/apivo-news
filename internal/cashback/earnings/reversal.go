// Undoing a credit the network took back, or an operator refused (T071,
// T119, SC-010, C-3).
//
// A reversal never edits the entry it undoes. It is a second entry, citing
// the superseding report the network sent, carrying reversal_of_id and born
// reversed, with its own posting moving the money back. The pair - the
// original credit and this - is what makes the correction auditable: both
// facts stay readable, and the balance is the sum of what happened rather
// than the last thing anybody wrote.
//
// An operator rejecting a held credit (US7 scenario 3) undoes it the same
// way, with one difference: the network has restated nothing, so there is
// no superseding report, and the reversing entry rests on the credit's own
// report - the only evidence there is (C-2) - with the operator as its cause
// and their reason on the transition (FR-061). Migration 0032 admits that
// row; entry_reversed_at_most_once still keeps a credit undone once.
//
// The schema says the same thing three times, which is how much it matters:
// entry_guard freezes the original's evidence and makes reversed terminal,
// entry_reversal_is_reversed refuses a reversing entry in any other state,
// and entry_reversed_at_most_once refuses a second reversal of one credit -
// two debits for one clawback, each looking correct on its own.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

var (
	// ErrNoReversingReport reports a reversal with no evidence of its own.
	// A status change is a new superseding row (C-3), never an edit, so
	// when the NETWORK reverses, the reversing report always exists by the
	// time this is called, and a reversal citing the original's report with
	// nobody named as its cause is a caller that lost track of which row it
	// read. Only a named operator's rejection may rest on the credit's own
	// report, because only then is there no other row to cite.
	ErrNoReversingReport = errors.New("earnings: a reversal cites the report that reversed it, not the one it undoes")
	// ErrNotReversible reports an attempt to reverse an entry that is
	// already reversed, or that never held money.
	ErrNotReversible = errors.New("earnings: the entry holds nothing to reverse")
	// ErrNotReversed reports a reversal the database refused. It wraps the
	// refusal unchanged: the transfer has posted by then, and a caller that
	// swallowed this would leave money moved with no entry saying so.
	ErrNotReversed = errors.New("earnings: the transfer posted but the reversal could not be recorded")
)

// Reversals writes the second half of an auditable pair.
type Reversals struct {
	entries *Entries
}

// NewReversals builds it over the entry machine, refusing a nil one.
func NewReversals(entries *Entries) (*Reversals, error) {
	if entries == nil {
		return nil, ErrNoEntries
	}
	return &Reversals{entries: entries}, nil
}

// Reversal is one clawback: the credit being undone and the report that
// undid it.
type Reversal struct {
	// Original is the entry the network took back. It is read, never
	// written: this is the fact the pair exists to preserve.
	Original Entry
	// Report is the SUPERSEDING report carrying the reversal, and becomes
	// the new entry's own evidence (C-2). For an operator's rejection it is
	// the original's own report, and Actor must then be named.
	Report uuid.UUID
	// Reason is recorded with the opening transition, and Actor names the
	// operator where a human caused it rather than a poll.
	Reason string
	Actor  uuid.UUID
}

// Reverse writes the reversing entry and posts the money back.
//
// The posting is computed from the ORIGINAL'S state, not the reversal's:
// money sits in whichever stage account the credit reached, and that is
// where it has to come out of. A reversal of a confirmed credit and one of a
// pending credit debit different accounts, and getting that wrong leaves a
// member's confirmed balance untouched while some other bucket goes negative.
//
// Post first, then record - the same order and the same reason as every
// other move: entry_transition cannot name a transfer that does not exist
// yet, and the key is derived from the reversing report so a retry re-posts
// to the same transfer rather than moving the money twice.
// db is the transaction the store was bound to, for the reason
// [Entries.Apply] takes one: the reversing entry and the announcement that
// it exists commit together or neither does.
func (r *Reversals) Reverse(ctx context.Context, db events.RowQuerier, reversal Reversal) (Entry, error) {
	switch reversal.Report {
	case uuid.Nil:
		return Entry{}, fmt.Errorf("%w: entry %s", ErrNoReversingReport, reversal.Original.ID)
	case reversal.Original.Report:
		if reversal.Actor == uuid.Nil {
			return Entry{}, fmt.Errorf("%w: entry %s cites %s already", ErrNoReversingReport,
				reversal.Original.ID, reversal.Report)
		}
	}
	if _, holds := stages[reversal.Original.State]; !holds {
		return Entry{}, fmt.Errorf("%w: %s is %s", ErrNotReversible,
			reversal.Original.ID, reversal.Original.State)
	}

	where, err := postingsFor(reversal.Original.Member, reversal.Original.State, StateReversed, r.entries.receivable)
	if err != nil {
		return Entry{}, err
	}
	ref, err := r.entries.post(ctx, where, reversal.Original.Amount,
		idempotencyKey(reversal.Original.ID, reversal.Report, StateReversed))
	if err != nil {
		return Entry{}, err
	}

	// The reversing entry carries the original's member, brand, click and
	// amount, because it undoes exactly that credit and nothing else. The
	// evidence is the network's superseding report where the network caused
	// it, and the credit's own where an operator did; entry_one_per_report
	// (0032) refuses a second CREDIT on a report and admits the reversal.
	created, err := r.entries.store.CreateEntry(ctx, store.CreateEntryParams{
		AccountID:            pgUUID(reversal.Original.Member),
		BrandID:              reversal.Original.Brand,
		NetworkTransactionID: pgUUID(reversal.Report),
		ClickID:              pgUUIDOrNull(reversal.Original.Click),
		State:                string(StateReversed),
		AmountMinor:          reversal.Original.Amount.Minor,
		Currency:             string(reversal.Original.Amount.Currency),
		ReversalOfID:         pgUUID(reversal.Original.ID),
	})
	if err != nil {
		return Entry{}, fmt.Errorf("%w: reversing %s: %w", ErrNotReversed, reversal.Original.ID, err)
	}

	// The opening transition, from nothing. Recorded like any other, because
	// a reversal that moved money without a transition would be the same
	// disagreement D7 forbids anywhere else.
	recorded, err := r.entries.record(ctx, uuid.UUID(created.ID.Bytes), "", StateReversed,
		ref, reversal.Reason, reversal.Actor)
	if err != nil {
		return Entry{}, err
	}
	reversing, err := entryFrom(created)
	if err != nil {
		return Entry{}, err
	}
	// Two facts, because two things happened: an entry that did not exist
	// now does, and it moved into the state it was born in. Announcing only
	// the creation would leave a consumer following state changes with an
	// entry that appeared already reversed and no transfer to trace it by;
	// announcing only the move would leave one following creations blind to
	// an entry that owes a member's money back.
	if err := r.entries.announcer.Created(ctx, db, reversing); err != nil {
		return Entry{}, err
	}
	if err := r.entries.announcer.StateChanged(ctx, db, recorded); err != nil {
		return Entry{}, err
	}
	return reversing, nil
}
