// What to do when a rail does not answer the way it was hoped (T094,
// FR-053).
//
// A submission that failed leaves exactly one of two situations, and telling
// them apart is the whole of this file:
//
//   - RETRYABLE. The payment may already be in flight. Re-send it, with the
//     SAME key, because that is what makes the second attempt a question
//     about the first rather than a second payment. The reservation stays
//     exactly where it is: releasing it would let the member spend a balance
//     that is about to leave.
//   - TERMINAL. The payment will never happen. The payout is marked failed,
//     the request is marked failed, and the reservation goes back to
//     confirmed so the member has their money again (US4 scenario 5).
//
// Getting this backwards costs real money in both directions, which is why
// [Rail] makes every failure carry its classification and why an
// unclassified one is a bug rather than a case to guess at.
//
// The key is never re-derived here. It is read from the payout row, which
// generated it (C-5, D8), so a retry cannot mint a new one even by accident -
// and minting one is exactly how a retry stops being a retry.

package payout

import (
	"context"
	"errors"
	"fmt"

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
	// ErrNoPayout reports a request that has never been approved, so there
	// is no submission to retry. Distinct from a payout that failed: one is
	// a request still waiting for a decision, the other is a payment that
	// went wrong.
	ErrNoPayout = errors.New("payout: this withdrawal has no payout to retry")
	// ErrNothingToRetry reports a payout that has already finished. A
	// settled payment is terminal (payout_guard says so) and a failed one
	// has had its reservation released; re-sending either would be a second
	// payment or a second release.
	ErrNothingToRetry = errors.New("payout: this payout has already finished")
	// ErrNotRetried reports a retry the database refused. It wraps the
	// refusal unchanged.
	ErrNotRetried = errors.New("payout: the retry could not be recorded")
)

// Outcome is what a retry concluded.
type Outcome struct {
	// Payout is the payment as it now stands.
	Payout Payout
	// Released is what went back to confirmed, and is zero unless the
	// failure was terminal. A retryable failure releases nothing: the money
	// may be in flight.
	Released money.Amount
	// ReleaseTransfer is the ledger movement that carried it back, empty
	// unless something was released.
	ReleaseTransfer string
}

// Retries re-sends payments the rail did not take, and gives up on the ones
// it never will.
type Retries struct {
	db         Beginner
	rail       Rail
	ledger     wallet.Ledger
	receivable string
	descriptor string
	announcer  *Announcer
}

// NewRetries builds the service. The receivable may be blank and the
// descriptor may not: a retry re-sends a payment, and a payment a member
// cannot recognise on their statement is one they charge back (FR-070,
// FR-073).
func NewRetries(db Beginner, rail Rail, ledger wallet.Ledger, receivable, descriptor string) (*Retries, error) {
	switch {
	case db == nil:
		return nil, ErrNoWithdrawalStore
	case rail == nil:
		return nil, ErrNoRail
	case ledger == nil:
		return nil, ErrNoLedger
	case descriptor == "":
		return nil, ErrNoDescriptor
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &Retries{
		db: db, rail: rail, ledger: ledger, receivable: receivable,
		descriptor: descriptor, announcer: announcer,
	}, nil
}

// Retry re-sends the payment for one request, or gives up on it.
//
// The rail is asked OUTSIDE the transaction, exactly as approving asks it,
// and for the same reason: a rollback cannot un-send a payment. So the
// sequence is read-and-release-the-lock, ask, then write what came back.
//
// That leaves a window in which two retries could both ask the rail. It is
// harmless and it is the point: both send the same key, so the rail answers
// the same payment twice rather than making two. This is the one place where
// leaning on the key instead of on a lock is not laziness - a lock held
// across a network call to a payment rail is a lock held for as long as that
// rail feels like taking.
func (r *Retries) Retry(ctx context.Context, request uuid.UUID) (Outcome, error) {
	if request == uuid.Nil {
		return Outcome{}, fmt.Errorf("%w: no withdrawal to retry", ErrNotRetried)
	}

	owed, sending, err := r.pending(ctx, request)
	if err != nil {
		return Outcome{}, err
	}

	reference, err := r.rail.Submit(ctx, sending)
	switch {
	case errors.Is(err, ErrRailTerminal):
		// The payment will never happen. Give the member their money back.
		return r.giveUp(ctx, request, err)
	case err != nil:
		// Retryable, or a rail that did not classify - and an unclassified
		// failure is treated as retryable, because the safe reading of "I do
		// not know" is that the money might be in flight. Releasing on it
		// would be the expensive mistake.
		return Outcome{}, fmt.Errorf("payout: re-sending %s to the %s rail: %w",
			sending.IdempotencyKey, r.rail.Kind(), err)
	}
	return r.recordSent(ctx, owed, reference)
}

// pending reads the payout still owed to the rail and builds the instruction
// to re-send, under the key the row already carries.
func (r *Retries) pending(ctx context.Context, request uuid.UUID) (Payout, Instruction, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Payout{}, Instruction{}, fmt.Errorf("%w: %w", ErrNotRetried, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	row, err := queries.LockWithdrawalRequestForDecision(ctx, pgtype.UUID{Bytes: request, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Payout{}, Instruction{}, fmt.Errorf("%w: %s", ErrNoSuchWithdrawal, request)
	case err != nil:
		return Payout{}, Instruction{}, fmt.Errorf("%w: reading %s: %w", ErrNotRetried, request, err)
	}
	withdrawal, err := withdrawalFrom(row)
	if err != nil {
		return Payout{}, Instruction{}, err
	}

	payoutRow, err := queries.LockPayoutForRequest(ctx, pgtype.UUID{Bytes: request, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Payout{}, Instruction{}, fmt.Errorf("%w: %s", ErrNoPayout, request)
	case err != nil:
		return Payout{}, Instruction{}, fmt.Errorf("%w: reading the payout for %s: %w", ErrNotRetried, request, err)
	}
	sending, err := payoutFrom(payoutRow)
	if err != nil {
		return Payout{}, Instruction{}, err
	}
	if sending.State != StatusSubmitted {
		return Payout{}, Instruction{}, fmt.Errorf("%w: %s is %s", ErrNothingToRetry, sending.ID, sending.State)
	}

	destination, err := r.destinationOf(ctx, tx, withdrawal)
	if err != nil {
		return Payout{}, Instruction{}, err
	}
	// Read-only, so there is nothing to commit; the rollback releases the
	// locks before the rail is asked.
	return sending, Instruction{
		IdempotencyKey: sending.IdempotencyKey,
		Amount:         sending.Amount,
		Destination:    destination.ToRef(),
		Descriptor:     r.descriptor,
	}, nil
}

// destinationOf reads where the money goes, inside the caller's transaction.
func (r *Retries) destinationOf(ctx context.Context, tx pgx.Tx, withdrawal Withdrawal) (Destination, error) {
	destinations, err := NewDestinations(tx)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: %w", ErrNotRetried, err)
	}
	destination, err := destinations.Get(ctx, withdrawal.Member, withdrawal.Destination)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: %w", ErrNotRetried, err)
	}
	return destination, nil
}

// recordSent stores the reference a re-send finally got back.
//
// Narrowed on the payout having none yet, so the retry that arrives second
// records nothing and reads back what the first stored. The first reference
// is the one an auditor follows out of this system; replacing it silently
// would break that trail.
func (r *Retries) recordSent(ctx context.Context, owed Payout, reference RailReference) (Outcome, error) {
	row, err := store.New(r.db).RecordPayoutSubmission(ctx, store.RecordPayoutSubmissionParams{
		ID:            pgtype.UUID{Bytes: owed.ID, Valid: true},
		RailReference: pgtype.Text{String: reference.Ref(), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already carries one: another retry got there first, under the
			// same key, so it is the same payment. Read it back rather than
			// reporting a failure that did not happen.
			return r.reread(ctx, owed)
		}
		return Outcome{}, fmt.Errorf("%w: recording the rail reference for %s: %w", ErrNotRetried, owed.ID, err)
	}
	sent, err := payoutFrom(row)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Payout: sent}, nil
}

// reread answers the payout as it now stands.
func (r *Retries) reread(ctx context.Context, owed Payout) (Outcome, error) {
	row, err := store.New(r.db).GetPayoutForRequest(ctx, pgtype.UUID{Bytes: owed.Request, Valid: true})
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: re-reading %s: %w", ErrNotRetried, owed.ID, err)
	}
	sent, err := payoutFrom(row)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Payout: sent}, nil
}

// giveUp marks a terminally failed payment failed and puts the money back
// (FR-053, US4 scenario 5).
//
// One transaction, and the ledger is posted inside it for the reason a
// rejection posts inside its own: there is no rail call left to make. What
// cannot be undone by a rollback is the release, so it happens after every
// refusal and before the two writes that describe it - and a retry re-posts
// under the same key, finds the ledger already holding it, and writes the
// rows that did not get written.
//
// The release key is the one a REJECTION would have used, deliberately. Only
// one of the two can ever happen to a request: a rejection is a decision
// before approval and a terminal failure is one after it. Sharing the key
// means that if the impossible happened, the money would come back once.
func (r *Retries) giveUp(ctx context.Context, request uuid.UUID, cause error) (Outcome, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %w", ErrNotRetried, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	payoutRow, err := queries.LockPayoutForRequest(ctx, pgtype.UUID{Bytes: request, Valid: true})
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: reading the payout for %s: %w", ErrNotRetried, request, err)
	}
	failing, err := payoutFrom(payoutRow)
	if err != nil {
		return Outcome{}, err
	}
	if failing.State != StatusSubmitted {
		return Outcome{}, fmt.Errorf("%w: %s is %s", ErrNothingToRetry, failing.ID, failing.State)
	}
	if r.receivable == "" {
		return Outcome{}, ErrNoReceivable
	}

	requestRow, err := queries.LockWithdrawalRequestForDecision(ctx, pgtype.UUID{Bytes: request, Valid: true})
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: reading %s: %w", ErrNotRetried, request, err)
	}
	withdrawal, err := withdrawalFrom(requestRow)
	if err != nil {
		return Outcome{}, err
	}

	entries := earningsstore.New(tx)
	machine, err := earnings.NewEntries(entries, r.ledger, r.receivable)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %w", ErrNotRetried, err)
	}
	held, err := machine.Reserved(ctx, entries, withdrawal.ReservedTransfer)
	if err != nil {
		return Outcome{}, err
	}
	if len(held) == 0 {
		return Outcome{}, fmt.Errorf("%w: %s", ErrNothingReserved, request)
	}

	// Past this line the ledger has been touched.
	released, err := machine.Release(ctx, tx, entries, held, request)
	if err != nil {
		return Outcome{}, err
	}

	failedRow, err := queries.RecordPayoutOutcome(ctx, store.RecordPayoutOutcomeParams{
		ID:        pgtype.UUID{Bytes: failing.ID, Valid: true},
		FromState: string(StatusSubmitted),
		ToState:   string(StatusFailed),
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: failing %s: %w", ErrNotRetried, failing.ID, err)
	}
	// The approver stays the decider: they are still the human who decided
	// this request, and the rail is what decided the payment. Recording
	// somebody else - or nobody - would lose the one name C-4 exists to
	// keep.
	decidedRow, err := queries.RecordWithdrawalDecision(ctx, store.RecordWithdrawalDecisionParams{
		ID:             pgtype.UUID{Bytes: request, Valid: true},
		FromState:      string(StateApproved),
		ToState:        string(StateFailed),
		DecidedBy:      pgtype.UUID{Bytes: failing.ApprovedBy, Valid: true},
		DecisionReason: pgtype.Text{String: railRefusal(cause), Valid: true},
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: failing %s: %w", ErrNotRetried, request, err)
	}

	failed, err := payoutFrom(failedRow)
	if err != nil {
		return Outcome{}, err
	}
	decided, err := withdrawalFrom(decidedRow)
	if err != nil {
		return Outcome{}, err
	}
	// Beside the release and both state changes, so the money going back
	// and the stream saying why are one commit. The instant is the
	// decision's, read off the request row: the payout row carries only
	// when it was SUBMITTED, which is not when it failed.
	if err := r.announcer.Failed(ctx, tx, failed, classificationOf(cause), decided.DecidedAt); err != nil {
		return Outcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Outcome{}, fmt.Errorf("%w: %s: %w", ErrNotRetried, request, err)
	}
	return Outcome{
		Payout:          failed,
		Released:        released.Amount,
		ReleaseTransfer: string(released.Transfer),
	}, nil
}

// railRefusal renders why a payment will never happen, for the decision
// reason a member is shown.
//
// The rail's own words, bounded: a reason is the record of what happened,
// and a rail that returned a megabyte of XML would otherwise put it in a
// member-facing field. Never blank, because
// withdrawal_request_decision_reason_not_blank refuses one that is.
func railRefusal(cause error) string {
	const most = 500
	said := "the payout rail refused this payment"
	if cause != nil {
		said = cause.Error()
	}
	if runes := []rune(said); len(runes) > most {
		said = string(runes[:most]) + "..."
	}
	return said
}

// classificationOf names the rail's verdict for the event payload.
//
// Only a terminal failure reaches [Retries.giveUp], so this answers
// "terminal" in practice - and it is derived rather than written out, because
// a field that says what the caller assumed rather than what the error
// carries is a field that goes on saying it after the assumption stops
// holding.
func classificationOf(cause error) string {
	switch {
	case errors.Is(cause, ErrRailTerminal):
		return "terminal"
	case errors.Is(cause, ErrRailRetryable):
		return "retryable"
	default:
		return "unclassified"
	}
}
