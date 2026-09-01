// The tests for retry.go, against the real schema (T094, FR-053).
//
// The claim under test is which of two things a failure means, and both
// answers move money - one by leaving it where it is, one by giving it back.
// A fake would be asserting against itself.

package payout_test

import (
	"context"
	"errors"
	"strings"
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

// TestRetryingAPaymentTheRailAlreadyTookChangesNothing. The rail is
// idempotent on the key, so a retry of a submission that in fact landed gets
// the same reference back - and the payout already carries it. Recording it
// again would replace the reference an auditor follows, so the retry reads it
// back instead.
func TestRetryingAPaymentTheRailAlreadyTookChangesNothing(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()

	// A clean approval: the rail took it and the reference is stored.
	approved, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	if approved.RailReference == "" {
		t.Fatal("the approval recorded no reference; this case needs one")
	}

	outcome, err := retries(t, f, rail).Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry(): %v", err)
	}

	if outcome.Payout.RailReference != approved.RailReference {
		t.Errorf("the retry answered %q, want the reference already on record %q",
			outcome.Payout.RailReference, approved.RailReference)
	}
	if outcome.Payout.ID != approved.ID {
		t.Errorf("the retry answered payout %s, want %s", outcome.Payout.ID, approved.ID)
	}
	if !outcome.Released.IsZero() {
		t.Errorf("released %s, want nothing: the payment stands", outcome.Released)
	}
}

// TestARailsRefusalIsRecordedButNotWholesale. The reason is a member-facing
// field and the rail's message is the rail's to choose, so a rail that
// answers with a wall of text must not put all of it there.
func TestARailsRefusalIsRecordedButNotWholesale(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)

	rail.FailNext(payout.Terminal(errors.New(strings.Repeat("verbose ", 400))))

	if _, err := retries(t, f, rail).Retry(ctx, request.ID); err != nil {
		t.Fatalf("Retry(): %v", err)
	}

	failed, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if failed.DecisionReason == "" {
		t.Fatal("the request records no reason")
	}
	if runes := []rune(failed.DecisionReason); len(runes) > 600 {
		t.Errorf("the reason is %d characters, want it bounded", len(runes))
	}
	if !strings.Contains(failed.DecisionReason, "verbose") {
		t.Errorf("the reason = %q, want the rail's own words in it", failed.DecisionReason)
	}
}

// TestADeploymentThatNamedNoReceivableCannotGiveUp. Giving up releases money,
// and releasing needs the house account earnings came from. Refused before
// the ledger is touched, leaving the payout submitted for a deployment that
// has been fixed.
func TestADeploymentThatNamedNoReceivableCannotGiveUp(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New(stub.WithTimeout())
	f, request := approvedThrough(ctx, t, rail)

	retrier, err := payout.NewRetries(f.pool, rail, f.ledger, "", descriptor)
	if err != nil {
		t.Fatalf("NewRetries() with no receivable = %v, want it built", err)
	}
	rail.FailNext(payout.Terminal(errors.New("the destination account is closed")))

	if _, err := retrier.Retry(ctx, request.ID); !errors.Is(err, payout.ErrNoReceivable) {
		t.Fatalf("Retry() = %v, want one wrapping %v", err, payout.ErrNoReceivable)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s, want it untouched", still)
	}
	if state := payoutOf(ctx, t, f, request.ID).State; state != string(payout.StatusSubmitted) {
		t.Errorf("the payout is %s, want it left %s for a deployment that has been fixed", state, payout.StatusSubmitted)
	}
}

// TestGivingUpOnAWithdrawalThatReservedNothingIsRefused. The release is what
// gives a member their money back, and a request whose reservation moved no
// entries has nothing to give. Refused rather than marked failed, which would
// record a refund that did not happen.
func TestGivingUpOnAWithdrawalThatReservedNothingIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, approvableAmount*2)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()

	// A request naming a reservation transfer no entry was ever moved under,
	// approved so there is a payout to retry.
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.withdrawal_request
		    (account_id, destination_id, amount_minor, currency, reserved_transfer_ref, state, decided_by, decided_at)
		values ($1, $2, 3000, 'EUR', $3, 'approved', $4, now()) returning id`,
		pgtype.UUID{Bytes: f.member, Valid: true},
		pgtype.UUID{Bytes: f.destination, Valid: true},
		"transfer:orphaned:"+uuid.NewString(),
		pgtype.UUID{Bytes: operator, Valid: true}).Scan(&id); err != nil {
		t.Fatalf("seeding the orphaned request: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		values ($1, $2, $3, 3000, 'EUR', 'stub')`,
		fixtureBrand, id, pgtype.UUID{Bytes: operator, Valid: true}); err != nil {
		t.Fatalf("seeding the payout: %v", err)
	}

	rail.FailNext(payout.Terminal(errors.New("the destination account is closed")))
	if _, err := retries(t, f, rail).Retry(ctx, uuid.UUID(id.Bytes)); !errors.Is(err, payout.ErrNothingReserved) {
		t.Fatalf("Retry() = %v, want one wrapping %v", err, payout.ErrNothingReserved)
	}
}

// TestASettledPayoutIsNotRetried. Money that has reached the member is
// terminal (payout_guard says so), and re-sending would be a second payment.
func TestASettledPayoutIsNotRetried(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()

	approved, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	// The far end of the journey, driven straight into the row: the rail
	// tells a deployment this happened, and nothing in the domain writes it
	// yet (that is the settlement path, not this one).
	if _, err := pool.Exec(ctx,
		`update cashback.payout set state = 'settled', settled_at = now() where id = $1`,
		pgtype.UUID{Bytes: approved.ID, Valid: true}); err != nil {
		t.Fatalf("settling the payout: %v", err)
	}

	if _, err := retries(t, f, rail).Retry(ctx, request.ID); !errors.Is(err, payout.ErrNothingToRetry) {
		t.Fatalf("Retry() on a settled payout = %v, want one wrapping %v", err, payout.ErrNothingToRetry)
	}
}
