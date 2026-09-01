// Refusing a withdrawal, which puts the money back (T093, FR-053, US4
// scenario 5).
//
// ONE TRANSACTION, unlike approving, and the difference is worth stating: an
// approval has to reach a payout rail, and a rail is not in any transaction,
// so it commits before it submits. A rejection touches nothing outside this
// database except the ledger, and the ledger is reached through the same
// post-then-record ordering every other entry move uses.
//
// The reason is not optional and not an afterthought.
// withdrawal_request_rejection_has_reason refuses a rejected row without one,
// FR-061 asks for a named human and a reason on every operator action, and a
// member is owed an explanation for money they asked for and did not get. So
// it travels with the decision rather than being written afterwards.

package payout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoDecisionReason reports a rejection with nothing said. The schema
	// refuses it too; this refuses it before anything is read, so the answer
	// names the rule rather than a constraint.
	ErrNoDecisionReason = errors.New("payout: a rejection records why (FR-061): supply a non-blank reason")
	// ErrNotRejected reports a rejection the database refused. It wraps the
	// refusal unchanged.
	ErrNotRejected = errors.New("payout: the rejection could not be recorded")
	// ErrNothingReserved reports a request whose reservation transfer moved
	// no entries.
	//
	// Alarming rather than ordinary: withdrawal_request.reserved_transfer_ref
	// is NOT NULL precisely so that every request has entries behind it, and
	// a request with none is one whose money cannot be given back because
	// nothing can be found to give. Refused rather than treated as an empty
	// release, which would mark the request rejected and leave the balance
	// wherever it actually is.
	ErrNothingReserved = errors.New("payout: this withdrawal's reservation moved no entries, so there is nothing to release")
)

// Rejection is one operator refusing one withdrawal.
type Rejection struct {
	// Request is the withdrawal being refused.
	Request uuid.UUID
	// Operator is the named human refusing it, taken from the token subject
	// and never from the body.
	Operator uuid.UUID
	// Reason is why, recorded on the request and shown to the member.
	// Required and non-blank (US7 scenario 3, FR-061).
	Reason string
}

// Rejected is what a refusal did.
type Rejected struct {
	// Request is the refused request as it now stands, carrying the
	// decision.
	Request Withdrawal
	// Released is what went back to confirmed - the sum of the entries the
	// reservation held, which is the request's own amount.
	Released money.Amount
	// ReleaseTransfer is the ledger transfer that carried it back. Distinct
	// from the request's reserved_transfer_ref: one movement out, one
	// movement back, each with its own reference.
	ReleaseTransfer string
}

// Rejections refuses withdrawals and puts their money back.
type Rejections struct {
	db         Beginner
	ledger     wallet.Ledger
	receivable string
	announcer  *Announcer
}

// NewRejections builds the service. The receivable may be blank - see
// [ErrNoReceivable] for why an incomplete deployment answers on the endpoint
// rather than failing to start, which would take the whole operator surface
// down over a deployment that simply cannot pay out yet.
func NewRejections(db Beginner, ledger wallet.Ledger, receivable string) (*Rejections, error) {
	switch {
	case db == nil:
		return nil, ErrNoWithdrawalStore
	case ledger == nil:
		return nil, ErrNoLedger
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &Rejections{db: db, ledger: ledger, receivable: receivable, announcer: announcer}, nil
}

// Reject refuses the request and releases its reservation.
//
// The order inside the transaction is the same shape approving uses and for
// the same reason: everything that can refuse runs before the ledger is asked
// for anything, because a posted transfer is not rolled back by a failing
// statement afterwards.
//
// What follows the post is recoverable if it fails. The release is keyed on
// the request (D8), so a retry re-posts to the same reference, the ledger
// records nothing a second time, and the decision is written against the
// release that already happened.
func (r *Rejections) Reject(ctx context.Context, rejection Rejection) (Rejected, error) {
	reason := strings.TrimSpace(rejection.Reason)
	switch {
	case rejection.Request == uuid.Nil:
		return Rejected{}, fmt.Errorf("%w: no withdrawal to refuse", ErrNotRejected)
	case rejection.Operator == uuid.Nil:
		return Rejected{}, fmt.Errorf("%w: no operator is refusing it", ErrNotRejected)
	case reason == "":
		return Rejected{}, ErrNoDecisionReason
	}

	if r.receivable == "" {
		return Rejected{}, ErrNoReceivable
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Rejected{}, fmt.Errorf("%w: %w", ErrNotRejected, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	request, err := awaitingDecision(ctx, queries, rejection.Request)
	if err != nil {
		return Rejected{}, err
	}

	entries := earningsstore.New(tx)
	machine, err := earnings.NewEntries(entries, r.ledger, r.receivable)
	if err != nil {
		return Rejected{}, fmt.Errorf("%w: %w", ErrNotRejected, err)
	}
	held, err := machine.Reserved(ctx, entries, request.ReservedTransfer)
	if err != nil {
		return Rejected{}, err
	}
	if len(held) == 0 {
		return Rejected{}, fmt.Errorf("%w: %s", ErrNothingReserved, request.ID)
	}

	// Past this line the ledger has been touched.
	released, err := machine.Release(ctx, tx, entries, held, request.ID)
	if err != nil {
		return Rejected{}, err
	}

	row, err := queries.RecordWithdrawalDecision(ctx, store.RecordWithdrawalDecisionParams{
		ID:             pgtype.UUID{Bytes: request.ID, Valid: true},
		FromState:      string(StateAwaitingApproval),
		ToState:        string(StateRejected),
		DecidedBy:      pgtype.UUID{Bytes: rejection.Operator, Valid: true},
		DecisionReason: pgtype.Text{String: reason, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Rejected{}, fmt.Errorf("%w: %s", ErrNotAwaitingApproval, request.ID)
		}
		return Rejected{}, fmt.Errorf("%w: refusing %s: %w", ErrNotRejected, request.ID, err)
	}
	refused, err := withdrawalFrom(row)
	if err != nil {
		return Rejected{}, err
	}
	// Beside the release and the decision, so a member's money going back
	// and the stream saying so are one commit.
	if err := r.announcer.Rejected(ctx, tx, refused); err != nil {
		return Rejected{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Rejected{}, fmt.Errorf("%w: %s: %w", ErrNotRejected, request.ID, err)
	}

	return Rejected{
		Request:         refused,
		Released:        released.Amount,
		ReleaseTransfer: string(released.Transfer),
	}, nil
}

// awaitingDecision reads and locks a request an operator may still decide.
//
// Shared by approving and refusing, because they are two answers to one
// question and the read that finds the question must be one read. A second
// spelling of "still in the queue" would eventually disagree with the first.
func awaitingDecision(ctx context.Context, queries *store.Queries, id uuid.UUID) (Withdrawal, error) {
	row, err := queries.LockWithdrawalRequestForDecision(ctx, pgtype.UUID{Bytes: id, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Withdrawal{}, fmt.Errorf("%w: %s", ErrNoSuchWithdrawal, id)
	case err != nil:
		return Withdrawal{}, fmt.Errorf("%w: reading %s: %w", ErrNotApproved, id, err)
	}
	request, err := withdrawalFrom(row)
	if err != nil {
		return Withdrawal{}, err
	}
	if request.State != StateAwaitingApproval {
		return Withdrawal{}, fmt.Errorf("%w: %s is %s", ErrNotAwaitingApproval, id, request.State)
	}
	return request, nil
}
