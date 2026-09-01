// The tests for settle.go, against the real schema (T146).
//
// The claim is that a payment arriving is OBSERVED rather than decided: the
// payout reaches its terminal state, the request follows, the member's
// approval is left exactly as the human left it, and the stream says so once.

package payout_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

// settlements builds the sweep over the fixture and a rail.
func settlements(t *testing.T, f fixture, rail payout.Rail) *payout.Settlements {
	t.Helper()
	s, err := payout.NewSettlements(discardLogger(), f.pool, rail, retries(t, f, rail))
	if err != nil {
		t.Fatalf("NewSettlements(): %v", err)
	}
	return s
}

// mustReference parses the reference a submission recorded, failing the case
// if there is none - every test here needs a payment the rail can be asked
// about.
func mustReference(t *testing.T, ref string) payout.RailReference {
	t.Helper()
	parsed, err := payout.NewRailReference(ref)
	if err != nil {
		t.Fatalf("the approval recorded no usable reference %q: %v", ref, err)
	}
	return parsed
}

// submittedThrough approves a request through a working rail, so a payment is
// in flight for the sweep to ask about.
func submittedThrough(ctx context.Context, t *testing.T, rail payout.Rail) (fixture, payout.Withdrawal, payout.Payout) {
	t.Helper()
	_, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	sent, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	if sent.State != payout.StatusSubmitted {
		t.Fatalf("the payout is %s, want %s - this case needs a payment in flight", sent.State, payout.StatusSubmitted)
	}
	return f, request, sent
}

// decision is the operator's decision as the request row holds it, read
// separately from the state so a settlement changing one and not the other is
// visible.
type decision struct {
	By uuid.UUID
	At time.Time
}

// decisionOf reads the operator's decision off the request row.
func decisionOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, request uuid.UUID) decision {
	t.Helper()
	var (
		by pgtype.UUID
		at pgtype.Timestamptz
	)
	if err := pool.QueryRow(ctx,
		`select decided_by, decided_at from cashback.withdrawal_request where id = $1`,
		pgtype.UUID{Bytes: request, Valid: true}).Scan(&by, &at); err != nil {
		t.Fatalf("reading the decision on %s: %v", request, err)
	}
	return decision{By: uuid.UUID(by.Bytes), At: at.Time}
}

// TestAPaymentThatArrivesIsRecordedWithoutRedecidingAnything is the whole
// claim. The payout settles, the request follows, and the human's decision -
// who made it and when - is untouched: a settlement is an observation, and a
// sweep's clock is not the moment an operator approved.
func TestAPaymentThatArrivesIsRecordedWithoutRedecidingAnything(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)

	approved := decisionOf(ctx, t, f.pool, request.ID)
	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}

	settled, err := settlements(t, f, rail).Settle(ctx, request.ID)
	if err != nil {
		t.Fatalf("Settle(): %v", err)
	}

	if settled.Status != payout.StatusSettled {
		t.Errorf("the rail said %s, want %s", settled.Status, payout.StatusSettled)
	}
	if settled.Payout.State != payout.StatusSettled {
		t.Errorf("the payout is %s, want %s", settled.Payout.State, payout.StatusSettled)
	}
	if settled.Payout.SettledAt.IsZero() {
		t.Error("the payout carries no settlement instant; payout_settled_iff_settlement_time would refuse the row")
	}
	if settled.Payout.RailReference != sent.RailReference {
		t.Errorf("the reference became %q, want the submission's %q", settled.Payout.RailReference, sent.RailReference)
	}

	// The request followed.
	paid, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if paid.State != payout.StatePaid {
		t.Errorf("the request is %s, want %s", paid.State, payout.StatePaid)
	}
	// And the decision is exactly as the operator left it. This is the
	// claim MarkWithdrawalPaid exists for: RecordWithdrawalDecision would
	// have stamped decided_at with the sweep's clock.
	switch after := decisionOf(ctx, t, f.pool, request.ID); {
	case after.By != approved.By:
		t.Errorf("the decision is now %s's, want the approver's %s", after.By, approved.By)
	case !after.At.Equal(approved.At):
		t.Errorf("decided_at moved to %s, want the approval's %s - a settlement is not a decision",
			after.At, approved.At)
	}

	// Announced once, on the payout, with what a member quotes to their bank.
	events := announcementsOf(ctx, t, pool, "cashback.payout.settled", settled.Payout.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cashback.payout.settled events, want exactly 1", len(events))
	}
	if events[0].Payload["rail_reference"] != sent.RailReference {
		t.Errorf("rail_reference = %v, want %q", events[0].Payload["rail_reference"], sent.RailReference)
	}
	if events[0].Payload["request_id"] != request.ID.String() {
		t.Errorf("request_id = %v, want %s", events[0].Payload["request_id"], request.ID)
	}
}

// TestAPaymentStillInFlightIsLeftAlone is the negative control, and the
// common case: most of what a sweep asks about has not arrived yet. Nothing
// may be written and nothing may be announced - a member waiting is not news.
func TestAPaymentStillInFlightIsLeftAlone(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)

	settled, err := settlements(t, f, rail).Settle(ctx, request.ID)
	if err != nil {
		t.Fatalf("Settle(): %v", err)
	}

	if settled.Status != payout.StatusSubmitted {
		t.Errorf("the rail said %s, want %s", settled.Status, payout.StatusSubmitted)
	}
	if settled.Payout.State != payout.StatusSubmitted {
		t.Errorf("the payout is %s, want it untouched", settled.Payout.State)
	}
	if waiting, err := f.withdrawals.Get(ctx, f.member, request.ID); err != nil {
		t.Fatalf("re-reading the request: %v", err)
	} else if waiting.State != payout.StateApproved {
		t.Errorf("the request is %s, want it still %s", waiting.State, payout.StateApproved)
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 0 {
		t.Errorf("a payment still in flight announced %d settlements, want none", len(events))
	}
}

// TestAPaymentTheRailCouldNotCompletePutsTheMoneyBack. The rail took it and
// ended badly, which is the same end as a refused submission - so the same
// code releases, and the member can ask again.
func TestAPaymentTheRailCouldNotCompletePutsTheMoneyBack(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)

	if err := rail.Fail(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("failing at the rail: %v", err)
	}

	settled, err := settlements(t, f, rail).Settle(ctx, request.ID)
	if err != nil {
		t.Fatalf("Settle(): %v", err)
	}

	if settled.Status != payout.StatusFailed {
		t.Errorf("the rail said %s, want %s", settled.Status, payout.StatusFailed)
	}
	if settled.Payout.State != payout.StatusFailed {
		t.Errorf("the payout is %s, want %s", settled.Payout.State, payout.StatusFailed)
	}
	if settled.Released.Released.IsZero() {
		t.Error("nothing was released; the money the rail did not move must come back")
	}
	if back := f.stageBalance(ctx, t, wallet.StageConfirmed); back.Minor != approvableAmount*2 {
		t.Errorf("confirmed = %d, want the full %d back", back.Minor, approvableAmount*2)
	}
	// A failure is not a settlement, and nothing may say it was.
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 0 {
		t.Errorf("a failed payment announced %d settlements, want none", len(events))
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.failed", sent.ID); len(events) != 1 {
		t.Errorf("got %d cashback.payout.failed events, want exactly 1", len(events))
	}
}

// TestASettledPaymentIsNotSettledAgain. payout_guard makes settled terminal,
// so a second sweep must be told rather than raising a constraint violation
// in the middle of a money movement - and must not announce a second arrival.
func TestASettledPaymentIsNotSettledAgain(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)

	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}
	sweep := settlements(t, f, rail)
	if _, err := sweep.Settle(ctx, request.ID); err != nil {
		t.Fatalf("the first Settle(): %v", err)
	}

	if _, err := sweep.Settle(ctx, request.ID); !errors.Is(err, payout.ErrNotSubmitted) {
		t.Errorf("the second Settle() = %v, want %v", err, payout.ErrNotSubmitted)
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 1 {
		t.Errorf("got %d settlement events after two sweeps, want exactly 1", len(events))
	}
}

// TestTheSweepSettlesWhatArrivedDespiteOneItCannotResolve. One payment's
// failure is not another's: they are independent payments to independent
// members, and a sweep that stopped at the first problem would leave everyone
// behind it unpaid.
//
// The unresolvable one is submitted FIRST, through a rail this sweep does not
// hold - SubmittedPayouts is ordered by submitted_at, so it is reached first
// and the settled one is only reached if the sweep carried on past it.
func TestTheSweepSettlesWhatArrivedDespiteOneItCannotResolve(t *testing.T) {
	ctx, _ := withdrawalPool(t)

	// A payment on somebody else's rail. This sweep will ask its own rail
	// about it, be told the reference is unknown, and must not stop there.
	stranger := stub.New()
	strandedFixture, stranded, _ := submittedThrough(ctx, t, stranger)

	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)
	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}

	// The sweep reports a failed run, because one payment could not be
	// resolved. That is the point: it reports AND finishes.
	if err := settlements(t, f, rail).Sweep(ctx); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Sweep() = %v, want it to report the payment it could not resolve", err)
	}

	paid, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if paid.State != payout.StatePaid {
		t.Errorf("the request is %s after a sweep, want %s - the sweep stopped at the payment it could not resolve",
			paid.State, payout.StatePaid)
	}
	// And the one it could not resolve is untouched, not guessed at.
	still, err := strandedFixture.withdrawals.Get(ctx, strandedFixture.member, stranded.ID)
	if err != nil {
		t.Fatalf("re-reading the stranded request: %v", err)
	}
	if still.State != payout.StateApproved {
		t.Errorf("the unresolvable request is %s, want it left at %s", still.State, payout.StateApproved)
	}
}

// TestASweepMissingAPartIsRefusedAtConstruction. Every one of these is
// something the sweep cannot do its job without, and a nil discovered mid-run
// is one discovered with a member's money in flight.
func TestASweepMissingAPartIsRefusedAtConstruction(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	rail := stub.New()

	if _, err := payout.NewSettlements(nil, f.pool, rail, retries(t, f, rail)); err == nil {
		t.Error("a sweep with no logger was built, want a refusal")
	}
	if _, err := payout.NewSettlements(discardLogger(), nil, rail, retries(t, f, rail)); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no database = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewSettlements(discardLogger(), f.pool, nil, retries(t, f, rail)); !errors.Is(err, payout.ErrNoRail) {
		t.Errorf("with no rail = %v, want one wrapping %v", err, payout.ErrNoRail)
	}
	if _, err := payout.NewSettlements(discardLogger(), f.pool, rail, nil); err == nil {
		t.Error("a sweep with nowhere to put the money back was built, want a refusal")
	}
}
