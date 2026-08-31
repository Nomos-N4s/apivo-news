// Confirming an entry, which takes two facts and not one (T070, FR-043).
//
// Confirmed is the only state that counts toward the withdrawal threshold
// (FR-050), so it is the state a member can turn into money leaving the
// business. FR-043 therefore asks for more than the network's word: the
// network must have approved the commission AND the commission must have
// been reconciled as received.
//
// The two are separate because they fail separately and for different
// reasons - the network has not decided yet, or Apivo has not imported the
// statement - and an operator looking at money that has not confirmed needs
// to know which, because only one of them is theirs to act on.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

var (
	// ErrNoReconciliation reports a confirmer with nothing to ask about
	// receipt. Refused at construction, because one that discovered it later
	// would already have read entries it cannot decide.
	ErrNoReconciliation = errors.New("earnings: confirming needs somewhere to ask whether the money arrived")
	// ErrNoEntries reports a confirmer with no machine to move entries with.
	ErrNoEntries = errors.New("earnings: confirming needs the entry machine to move through")
	// ErrNotApproved reports an entry whose network has not confirmed the
	// commission. Ordinary and temporary: most reports sit pending for weeks
	// while a return window runs, and this is what that looks like.
	ErrNotApproved = errors.New("earnings: the network has not approved the commission")
	// ErrNotReconciled reports a commission the network approved and no
	// statement has yet accounted for.
	//
	// Also ordinary, and it is the half FR-043 exists for. A network's
	// approval is its intention to pay; the statement is the money. Between
	// them sits every commission that was approved and then withdrawn, and a
	// balance that counted those would let a member withdraw money the
	// business never received.
	ErrNotReconciled = errors.New("earnings: no statement has accounted for the commission yet")
	// ErrReconciliationUnknown reports a report the reconciliation read could
	// not answer about. It is never treated as "not reconciled": a read that
	// failed is not a statement that disagreed, and confirming or refusing on
	// it would be deciding money on an error.
	ErrReconciliationUnknown = errors.New("earnings: whether the commission arrived could not be read")
)

// Reconciliation is the one read this file makes, named here rather than
// taken as *store.Queries so the caller keeps the transaction boundary.
type Reconciliation interface {
	ReportIsReconciled(ctx context.Context, networkTransactionID pgtype.UUID) (pgtype.Bool, error)
}

// Confirmations moves an entry to confirmed when both halves of FR-043 hold.
type Confirmations struct {
	entries    *Entries
	reconciled Reconciliation
}

// NewConfirmations builds the confirmer, refusing one that is missing a part.
func NewConfirmations(entries *Entries, reconciled Reconciliation) (*Confirmations, error) {
	switch {
	case entries == nil:
		return nil, ErrNoEntries
	case reconciled == nil:
		return nil, ErrNoReconciliation
	}
	return &Confirmations{entries: entries, reconciled: reconciled}, nil
}

// Confirm moves the entry to confirmed if the network approved the commission
// and a statement has accounted for it.
//
// Both gates are read BEFORE the machine is asked to move anything, so a
// refusal costs no ledger call and leaves the entry exactly where it was.
// They are reported as distinct errors rather than one, because the two are
// somebody's to act on and somebody else's to wait for.
//
// The status is the report's own, passed in rather than read here: the caller
// holding an entry is the caller that just read the report it cites, and a
// second read would be a second answer - one that could differ from the one
// the decision was made against.
func (c *Confirmations) Confirm(ctx context.Context, entry Entry, reported networks.Status, cause uuid.UUID) (Entry, error) {
	if reported != networks.StatusConfirmed {
		return Entry{}, fmt.Errorf("%w: %s reports %s", ErrNotApproved, entry.Report, reported)
	}

	answer, err := c.reconciled.ReportIsReconciled(ctx, pgtype.UUID{Bytes: entry.Report, Valid: true})
	switch {
	case err != nil:
		return Entry{}, fmt.Errorf("%w: report %s: %w", ErrReconciliationUnknown, entry.Report, err)
	case !answer.Valid:
		// `exists` cannot be null, so this is the statement's shape having
		// changed rather than the database declining to say. Refusing is the
		// safe direction: nothing confirms on an answer nobody can read.
		return Entry{}, fmt.Errorf("%w: report %s answered null", ErrReconciliationUnknown, entry.Report)
	case !answer.Bool:
		return Entry{}, fmt.Errorf("%w: report %s", ErrNotReconciled, entry.Report)
	}

	return c.entries.Apply(ctx, Move{
		Entry:  entry.ID,
		From:   entry.State,
		To:     StateConfirmed,
		Member: entry.Member,
		Amount: entry.Amount,
		Cause:  cause,
		Reason: "the network approved the commission and a statement accounted for it",
	})
}
