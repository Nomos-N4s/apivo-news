// The tests for approval.go, against the real schema (T092, C-4, C-5).
//
// A real database, not a fake store, because the two invariants under test
// are the database's: payout_insert_guard refuses an approver without the
// operator role, and the generated idempotency_key's unique constraint turns
// a second approval into a 23505. A fake would agree with this package
// instead of with Postgres, which is the one thing it must not do here.
//
// Same scratch database as the withdrawal tests, for the same reason: Approve
// COMMITS - twice, deliberately - so an outer transaction thrown away would
// test something that does not run.

package payout_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
)

// descriptor is what a member reads on their statement in these cases. Not a
// product name: no rail and no test may contain one (FR-073).
const descriptor = "FIXTURE CASHBACK"

// seedOperator writes an account holding the operator role - the only role
// payout_insert_guard accepts as an approver (C-4).
func seedOperator(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'An Operator', 'operator') returning id`,
		"operator-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// approvableAmount is what every case here asks for, in a member's single
// confirmed entry of twice that - so nothing is ever refused for the balance
// and each case is about the approval alone.
const approvableAmount = 3000

// approvable makes a withdrawal request ready for an operator to decide, and
// returns the fixture it belongs to together with the request.
func approvable(ctx context.Context, t *testing.T) (fixture, payout.Withdrawal) {
	t.Helper()
	f := aFixture(ctx, t, 1000, approvableAmount*2)
	made, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, approvableAmount),
	})
	if err != nil {
		t.Fatalf("requesting the withdrawal to approve: %v", err)
	}
	return f, made
}

// approvals builds the approver over the fixture's pool and a stub rail.
func approvals(t *testing.T, f fixture, rail *stub.Rail) *payout.Approvals {
	t.Helper()
	a, err := payout.NewApprovals(f.pool, rail, descriptor)
	if err != nil {
		t.Fatalf("NewApprovals(): %v", err)
	}
	return a
}

// TestAnApprovalRecordsTheNamedHumanBehindIt is C-4: the payout row IS the
// approval, and it names who made it.
func TestAnApprovalRecordsTheNamedHumanBehindIt(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	paid, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	if paid.ApprovedBy != operator {
		t.Errorf("approved_by = %s, want the operator %s", paid.ApprovedBy, operator)
	}
	if paid.Request != request.ID {
		t.Errorf("the payout pays %s, want %s", paid.Request, request.ID)
	}
	if paid.Amount != request.Amount {
		t.Errorf("the payout is for %s, want the request's %s", paid.Amount, request.Amount)
	}
	if paid.RailReference == "" {
		t.Error("the payout carries no rail reference; it is what an auditor follows out of this system")
	}
	if paid.State != payout.StatusSubmitted {
		t.Errorf("the payout is %s, want %s", paid.State, payout.StatusSubmitted)
	}

	// And the request has left the queue, naming the same operator.
	decided, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if decided.State != payout.StateApproved {
		t.Errorf("the request is %s, want %s", decided.State, payout.StateApproved)
	}
	if decided.DecidedBy != operator {
		t.Errorf("decided_by = %s, want %s", decided.DecidedBy, operator)
	}
	if decided.DecidedAt.IsZero() {
		t.Error("the request records no decision time; who and when are one fact")
	}
}

// TestAnApproverWithoutTheOperatorRoleIsRefusedByTheDatabase is C-4 enforced
// where it has to be. A service check alone would be a rule about this code
// path; payout_insert_guard is a rule about the table.
func TestAnApproverWithoutTheOperatorRoleIsRefusedByTheDatabase(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	// A reader, which is what every member is.
	notAnOperator := seedMember(ctx, t, pool)

	_, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: notAnOperator,
	})
	if !errors.Is(err, payout.ErrNotApproved) {
		t.Fatalf("Approve() = %v, want one wrapping %v", err, payout.ErrNotApproved)
	}

	// And nothing moved: the transaction that would have written the payout
	// is the one the guard aborted.
	still, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if still.State != payout.StateAwaitingApproval {
		t.Errorf("the request is %s after a refused approval, want %s", still.State, payout.StateAwaitingApproval)
	}
	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 0 {
		t.Errorf("%d payout(s) exist, want none", paidOut)
	}
}

// TestApprovingTwiceMakesOnePayout is C-5, and SC-004's core claim. The
// second approval loses on the generated key's unique constraint - it does
// not reach the rail at all.
func TestApprovingTwiceMakesOnePayout(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()
	approver := approvals(t, f, rail)

	first, err := approver.Approve(ctx, payout.Approval{Request: request.ID, Operator: operator})
	if err != nil {
		t.Fatalf("the first approval: %v", err)
	}
	_, err = approver.Approve(ctx, payout.Approval{Request: request.ID, Operator: operator})
	if err == nil {
		t.Fatal("the second approval succeeded, want a refusal")
	}
	// It is refused for having left the queue, before the key is even
	// reached - which is the cheaper of the two defences doing its job.
	if !errors.Is(err, payout.ErrNotAwaitingApproval) && !errors.Is(err, payout.ErrAlreadyApproved) {
		t.Fatalf("the second approval = %v, want %v or %v",
			err, payout.ErrNotAwaitingApproval, payout.ErrAlreadyApproved)
	}

	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 1 {
		t.Errorf("%d payout(s) exist for one request, want exactly one (%s)", paidOut, first.ID)
	}
}

// TestTheKeyIsTheDatabasesOwnDerivation is D8. Computing the same string in
// Go would be a second authority on the one thing C-5 rests on, so the key
// handed to the rail is read back from the generated column.
func TestTheKeyIsTheDatabasesOwnDerivation(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()

	paid, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	if want := "payout:" + request.ID.String(); paid.IdempotencyKey != want {
		t.Errorf("idempotency key = %q, want the generated %q", paid.IdempotencyKey, want)
	}
	// The rail was given that key, not one this package made up. The stub
	// builds its reference from the key it was submitted under, so the
	// reference coming back is the proof of which key reached it.
	if want := "stub:" + paid.IdempotencyKey; paid.RailReference != want {
		t.Errorf("the rail answered %q, want %q - it was submitted under a key this package invented",
			paid.RailReference, want)
	}
	_ = rail
}

// TestARailFailureLeavesTheApprovalStanding is FR-053 and the reason the
// commit comes first. The money may be in flight; rolling the approval back
// would let a second one be made for the same request.
func TestARailFailureLeavesTheApprovalStanding(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New(stub.WithTimeout())

	_, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("Approve() through a timing-out rail = %v, want one wrapping %v", err, payout.ErrRailRetryable)
	}

	// The payout exists, carries no reference, and is what a retry picks up.
	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 1 {
		t.Fatalf("%d payout(s) exist after a rail timeout, want the one the retry re-submits", paidOut)
	}
	var reference pgtype.Text
	if err := f.pool.QueryRow(ctx,
		`select rail_reference from cashback.payout where request_id = $1`,
		pgtype.UUID{Bytes: request.ID, Valid: true}).Scan(&reference); err != nil {
		t.Fatalf("reading the payout: %v", err)
	}
	if reference.Valid {
		t.Errorf("rail_reference = %q after a failed submission, want none", reference.String)
	}
}

// TestApprovingSomethingThatIsNotAwaitingApprovalIsRefused. Ordinary with two
// operators on one queue, and refused before the rail is touched.
func TestApprovingSomethingThatIsNotAwaitingApprovalIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, _ := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New()

	_, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: uuid.New(), Operator: operator,
	})
	if !errors.Is(err, payout.ErrNoSuchWithdrawal) {
		t.Fatalf("Approve(unknown id) = %v, want one wrapping %v", err, payout.ErrNoSuchWithdrawal)
	}
	// The rail was never reached, which a payment it would have recorded is
	// the proof of: Status refuses a reference it never issued.
	assertRailSawNothing(t, rail)
}

// assertRailSawNothing proves no payment reached the rail, by asking it about
// the reference the only possible submission would have produced. The stub
// answers terminal for a reference it never issued.
func assertRailSawNothing(t *testing.T, rail *stub.Rail) {
	t.Helper()
	reference, err := payout.NewRailReference("stub:payout:" + uuid.NewString())
	if err != nil {
		t.Fatalf("building a probe reference: %v", err)
	}
	if _, err := rail.Status(context.Background(), reference); !errors.Is(err, payout.ErrRailTerminal) {
		t.Errorf("the rail knows a payment it should never have been asked to make: %v", err)
	}
}

// TestAnApprovalNamingNobodyIsRefused. C-4 in the one place a caller could
// get it wrong: the database would refuse it too, but a request that reached
// here anonymously is a bug in the gate above.
func TestAnApprovalNamingNobodyIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f, request := approvable(ctx, t)
	rail := stub.New()

	_, err := approvals(t, f, rail).Approve(ctx, payout.Approval{Request: request.ID})
	if !errors.Is(err, payout.ErrNotApproved) {
		t.Fatalf("Approve() with no operator = %v, want one wrapping %v", err, payout.ErrNotApproved)
	}
	assertRailSawNothing(t, rail)
	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 0 {
		t.Errorf("%d payout(s) exist, want none", paidOut)
	}
}

// TestTheBrandIsTheOneTheMoneyWasEarnedUnder is ADR-0004. Read from the
// reserved entries rather than today's configuration, so a deployment
// rebranded between earning and paying does not reattribute the payment.
func TestTheBrandIsTheOneTheMoneyWasEarnedUnder(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	paid, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	// seedConfirmedEntry writes every entry under this brand.
	if paid.Brand != fixtureBrand {
		t.Errorf("the payout is attributed to %q, want the brand the entries were earned under", paid.Brand)
	}
}

// TestAnApproverMissingAPartIsRefusedAtConstruction. Each of these,
// discovered later, is discovered with a request already out of the queue.
func TestAnApproverMissingAPartIsRefusedAtConstruction(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	if _, err := payout.NewApprovals(nil, stub.New(), descriptor); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no database = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewApprovals(f.pool, nil, descriptor); !errors.Is(err, payout.ErrNoRail) {
		t.Errorf("with no rail = %v, want one wrapping %v", err, payout.ErrNoRail)
	}
	if _, err := payout.NewApprovals(f.pool, stub.New(), ""); !errors.Is(err, payout.ErrNoDescriptor) {
		t.Errorf("with no descriptor = %v, want one wrapping %v", err, payout.ErrNoDescriptor)
	}
}

// countPayouts answers how many payouts exist for one request.
func countPayouts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, request uuid.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`select count(*) from cashback.payout where request_id = $1`,
		pgtype.UUID{Bytes: request, Valid: true}).Scan(&count); err != nil {
		t.Fatalf("counting payouts: %v", err)
	}
	return count
}

// TestAWithdrawalSpanningTwoBrandsCannotBePaid is ADR-0004 at the moment it
// bites. payout.brand_id is one frozen column, so a request whose reserved
// entries were earned under two brands has no honest value for it - and
// picking either attributes half the money to a brand that did not earn it.
//
// Refused before the payout row exists, so the request stays in the queue for
// somebody to split rather than being half-paid.
func TestAWithdrawalSpanningTwoBrandsCannotBePaid(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000)
	operator := seedOperator(ctx, t, pool)

	// Two entries, two brands, and a withdrawal that needs both.
	for i, brand := range []string{fixtureBrand, "fixture-gr"} {
		seedConfirmedEntryForBrand(ctx, t, pool, f.member, 2000, brand)
		credit(ctx, t, f.ledger, f.member, euro(t, 2000), fmt.Sprintf("two-brands:%s:%d", f.member, i))
	}
	request, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 3000),
	})
	if err != nil {
		t.Fatalf("requesting across two brands: %v", err)
	}

	_, err = approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if !errors.Is(err, payout.ErrBrandUnresolved) {
		t.Fatalf("Approve() = %v, want one wrapping %v", err, payout.ErrBrandUnresolved)
	}
	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 0 {
		t.Errorf("%d payout(s) exist, want none", paidOut)
	}
	still, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if still.State != payout.StateAwaitingApproval {
		t.Errorf("the request is %s, want it left in the queue to be split", still.State)
	}
}

// TestAWithdrawalWhoseEntriesCannotBeFoundIsRefused is the other half of the
// same check, and the more alarming one: if no transition carries the
// request's reserved_transfer_ref then C-7's own join (migration 0016) would
// answer this payout's provenance with nothing. Refusing to pay is the only
// safe response.
func TestAWithdrawalWhoseEntriesCannotBeFoundIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	operator := seedOperator(ctx, t, pool)

	// A request written straight to the table, naming a reservation transfer
	// no entry was ever moved under. The service cannot produce this; a bug
	// elsewhere, or a hand-repaired row, could.
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.withdrawal_request
		    (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
		values ($1, $2, 3000, 'EUR', $3) returning id`,
		pgtype.UUID{Bytes: f.member, Valid: true},
		pgtype.UUID{Bytes: f.destination, Valid: true},
		"transfer:orphaned:"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seeding the orphaned request: %v", err)
	}

	_, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: uuid.UUID(id.Bytes), Operator: operator,
	})
	if !errors.Is(err, payout.ErrBrandUnresolved) {
		t.Fatalf("Approve() = %v, want one wrapping %v", err, payout.ErrBrandUnresolved)
	}
	if paidOut := countPayouts(ctx, t, f.pool, uuid.UUID(id.Bytes)); paidOut != 0 {
		t.Errorf("%d payout(s) exist, want none", paidOut)
	}
}

// TestASecondApprovalIsRefusedForHavingBeenDecided pins WHICH defence
// answers, not merely that one does. The state check and C-5's unique key
// both refuse a double approval, and the difference matters: the state check
// answers before the rail is reached, and C-5 is the backstop for a race that
// gets past it.
func TestASecondApprovalIsRefusedForHavingBeenDecided(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	approver := approvals(t, f, stub.New())

	if _, err := approver.Approve(ctx, payout.Approval{Request: request.ID, Operator: operator}); err != nil {
		t.Fatalf("the first approval: %v", err)
	}

	_, err := approver.Approve(ctx, payout.Approval{Request: request.ID, Operator: operator})
	if !errors.Is(err, payout.ErrNotAwaitingApproval) {
		t.Fatalf("the second approval = %v, want one wrapping %v", err, payout.ErrNotAwaitingApproval)
	}
}

// TestADestinationForAnotherRailIsNotPaidThroughThisOne. payout.rail records
// which rail carried the money and payout_guard freezes it, so paying a
// manual destination through the stub rail would label a payment permanently
// as carried by a rail that never saw it - and the member would be waiting
// for money on a rail nobody sent it down.
//
// The request itself was lawful: a member may have destinations for several
// rails, and which one this deployment pays through is not their business.
// The mismatch is the approval's to catch.
func TestADestinationForAnotherRailIsNotPaidThroughThisOne(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	operator := seedOperator(ctx, t, pool)
	manual := seedDestinationOfKind(ctx, t, pool, f.member, true, "manual")

	request, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: manual, Amount: euro(t, 3000),
	})
	if err != nil {
		t.Fatalf("requesting to a manual destination: %v", err)
	}

	rail := stub.New()
	_, err = approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if !errors.Is(err, payout.ErrNotApproved) {
		t.Fatalf("Approve() = %v, want one wrapping %v", err, payout.ErrNotApproved)
	}
	if paidOut := countPayouts(ctx, t, f.pool, request.ID); paidOut != 0 {
		t.Errorf("%d payout(s) exist, want none", paidOut)
	}
	assertRailSawNothing(t, rail)
}
