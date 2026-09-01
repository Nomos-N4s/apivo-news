// The tests for retry.go, against the real schema (T094, FR-053).
//
// The claim under test is which of two things a failure means, and both
// answers move money - one by leaving it where it is, one by giving it back.
// A fake would be asserting against itself.

package payout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

// retries builds the retry service over the fixture and a rail.
func retries(t *testing.T, f fixture, rail payout.Rail) *payout.Retries {
	t.Helper()
	r, err := payout.NewRetries(f.pool, rail, f.ledger, receivable, descriptor)
	if err != nil {
		t.Fatalf("NewRetries(): %v", err)
	}
	return r
}

// approvedThrough approves a withdrawal through the given rail and returns
// the fixture, the request and the resulting payout state.
func approvedThrough(ctx context.Context, t *testing.T, rail *stub.Rail) (fixture, payout.Withdrawal) {
	t.Helper()
	_, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	if _, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); err != nil && !errors.Is(err, payout.ErrRailRetryable) && !errors.Is(err, payout.ErrRailTerminal) {
		t.Fatalf("Approve(): %v", err)
	}
	return f, request
}

// TestATimedOutSubmissionIsResentUnderTheSameKey is FR-053's retryable half
// and the whole reason the key is generated rather than chosen: the second
// attempt is a question about the first, not a second payment.
func TestATimedOutSubmissionIsResentUnderTheSameKey(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)

	// The approval reached the rail and timed out, so the payout exists with
	// no reference.
	before := payoutOf(ctx, t, f, request.ID)
	if before.RailReference.Valid {
		t.Fatalf("the payout already carries %q, want the timed-out state", before.RailReference.String)
	}

	outcome, err := retries(t, f, rail).Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry(): %v", err)
	}

	if outcome.Payout.RailReference == "" {
		t.Error("the retry recorded no rail reference")
	}
	// The stub builds its reference from the key it was submitted under, so
	// the reference is the proof the same key was re-sent.
	if want := "stub:" + before.IdempotencyKey; outcome.Payout.RailReference != want {
		t.Errorf("the rail answered %q, want %q - the retry sent a different key", outcome.Payout.RailReference, want)
	}
	if outcome.Payout.State != payout.StatusSubmitted {
		t.Errorf("the payout is %s, want %s", outcome.Payout.State, payout.StatusSubmitted)
	}
	// Nothing was released: the money may have been in flight all along.
	if !outcome.Released.IsZero() {
		t.Errorf("released %s on a retryable failure, want nothing", outcome.Released)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s, want the %s still claimed", still, request.Amount)
	}
}

// TestATerminalFailureGivesTheMoneyBack is US4 scenario 5: the payment will
// never happen, so the member has their balance again.
func TestATerminalFailureGivesTheMoneyBack(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)

	confirmedBefore := f.stageBalance(ctx, t, wallet.StageConfirmed)
	rail.FailNext(payout.Terminal(errors.New("the destination account is closed")))

	outcome, err := retries(t, f, rail).Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry(): %v", err)
	}

	if outcome.Payout.State != payout.StatusFailed {
		t.Errorf("the payout is %s, want %s", outcome.Payout.State, payout.StatusFailed)
	}
	if outcome.Released != request.Amount {
		t.Errorf("released %s, want the %s reserved", outcome.Released, request.Amount)
	}
	if outcome.ReleaseTransfer == string(request.ReservedTransfer) {
		t.Error("the release reused the reservation's transfer; the ledger would have recorded nothing")
	}
	if back := f.stageBalance(ctx, t, wallet.StageConfirmed); back.Minor != confirmedBefore.Minor+request.Amount.Minor {
		t.Errorf("confirmed holds %s, want %s back on top of %s", back, request.Amount, confirmedBefore)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); !still.IsZero() {
		t.Errorf("the reserved stage holds %s, want nothing", still)
	}

	// The request followed, and it still names the human who approved it.
	failed, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if failed.State != payout.StateFailed {
		t.Errorf("the request is %s, want %s", failed.State, payout.StateFailed)
	}
	if failed.DecidedBy == uuid.Nil {
		t.Error("the request names no decider; the approver is still the human who decided it (C-4)")
	}
	if failed.DecisionReason == "" {
		t.Error("the request records no reason; a member is owed one for a payment that will not happen")
	}
}

// TestAFailedPayoutIsNotRetriedAgain. Its reservation has been released, so
// re-sending would be a payment against a balance the member has back.
func TestAFailedPayoutIsNotRetriedAgain(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)
	retrier := retries(t, f, rail)

	rail.FailNext(payout.Terminal(errors.New("the destination account is closed")))
	if _, err := retrier.Retry(ctx, request.ID); err != nil {
		t.Fatalf("the first retry: %v", err)
	}

	if _, err := retrier.Retry(ctx, request.ID); !errors.Is(err, payout.ErrNothingToRetry) {
		t.Fatalf("the second retry = %v, want one wrapping %v", err, payout.ErrNothingToRetry)
	}
	// And the money came back exactly once.
	if back := f.stageBalance(ctx, t, wallet.StageConfirmed); back.Minor != approvableAmount*2 {
		t.Errorf("confirmed holds %s, want the balance released exactly once", back)
	}
}

// TestRetryingAWithdrawalNobodyApprovedIsRefused. A request still waiting for
// a decision has no submission to re-send, and saying so is different from
// saying a payment went wrong.
func TestRetryingAWithdrawalNobodyApprovedIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f, request := approvable(ctx, t)

	_, err := retries(t, f, stub.New()).Retry(ctx, request.ID)
	if !errors.Is(err, payout.ErrNoPayout) {
		t.Fatalf("Retry() = %v, want one wrapping %v", err, payout.ErrNoPayout)
	}
}

// TestRetryingSomethingThatIsNotAWithdrawalIsRefused.
func TestRetryingSomethingThatIsNotAWithdrawalIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	if _, err := retries(t, f, stub.New()).Retry(ctx, uuid.New()); !errors.Is(err, payout.ErrNoSuchWithdrawal) {
		t.Errorf("Retry(unknown) = %v, want one wrapping %v", err, payout.ErrNoSuchWithdrawal)
	}
	if _, err := retries(t, f, stub.New()).Retry(ctx, uuid.Nil); !errors.Is(err, payout.ErrNotRetried) {
		t.Errorf("Retry(nil) = %v, want one wrapping %v", err, payout.ErrNotRetried)
	}
}

// TestAnUnclassifiedFailureIsTreatedAsRetryable. "I do not know" read as
// terminal releases a reservation for a payment that may be in flight, and
// the member then spends money that has already left. The safe reading is
// the one that costs a retry.
func TestAnUnclassifiedFailureIsTreatedAsRetryable(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)

	rail.FailNext(errors.New("the rail said something nobody classified"))

	_, err := retries(t, f, rail).Retry(ctx, request.ID)
	if err == nil {
		t.Fatal("Retry() through an unclassified failure succeeded, want it reported")
	}
	if errors.Is(err, payout.ErrRailTerminal) {
		t.Fatalf("Retry() = %v, want it not treated as terminal", err)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s, want the %s still claimed", still, request.Amount)
	}
	if state := payoutOf(ctx, t, f, request.ID).State; state != string(payout.StatusSubmitted) {
		t.Errorf("the payout is %s, want it left %s for another retry", state, payout.StatusSubmitted)
	}
}

// TestARetrierMissingAPartIsRefusedAtConstruction.
func TestARetrierMissingAPartIsRefusedAtConstruction(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	if _, err := payout.NewRetries(nil, stub.New(), f.ledger, receivable, descriptor); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no database = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewRetries(f.pool, nil, f.ledger, receivable, descriptor); !errors.Is(err, payout.ErrNoRail) {
		t.Errorf("with no rail = %v, want one wrapping %v", err, payout.ErrNoRail)
	}
	if _, err := payout.NewRetries(f.pool, stub.New(), nil, receivable, descriptor); !errors.Is(err, payout.ErrNoLedger) {
		t.Errorf("with no ledger = %v, want one wrapping %v", err, payout.ErrNoLedger)
	}
	if _, err := payout.NewRetries(f.pool, stub.New(), f.ledger, receivable, ""); !errors.Is(err, payout.ErrNoDescriptor) {
		t.Errorf("with no descriptor = %v, want one wrapping %v", err, payout.ErrNoDescriptor)
	}
}

// payoutOf reads the payout row for a request, as the database holds it.
func payoutOf(ctx context.Context, t *testing.T, f fixture, request uuid.UUID) struct {
	IdempotencyKey string
	RailReference  pgtype.Text
	State          string
} {
	t.Helper()
	var row struct {
		IdempotencyKey string
		RailReference  pgtype.Text
		State          string
	}
	if err := f.pool.QueryRow(ctx,
		`select idempotency_key, rail_reference, state from cashback.payout where request_id = $1`,
		pgtype.UUID{Bytes: request, Valid: true},
	).Scan(&row.IdempotencyKey, &row.RailReference, &row.State); err != nil {
		t.Fatalf("reading the payout for %s: %v", request, err)
	}
	return row
}
