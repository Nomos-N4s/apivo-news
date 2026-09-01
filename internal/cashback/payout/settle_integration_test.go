// The tests for settle.go, against the real schema (T146).
//
// The claim is that a payment arriving is OBSERVED rather than decided: the
// payout reaches its terminal state, the request follows, the member's
// approval is left exactly as the human left it, and the stream says so once.

package payout_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/manual"
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

// TestARequestWithNoPayoutIsNotAskedAbout. A request nobody approved has no
// payment for a rail to know about, and asking would be asking about
// something that does not exist.
func TestARequestWithNoPayoutIsNotAskedAbout(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f, request := approvable(ctx, t)

	if _, err := settlements(t, f, stub.New()).Settle(ctx, request.ID); !errors.Is(err, payout.ErrNoPayout) {
		t.Errorf("Settle() on an unapproved request = %v, want %v", err, payout.ErrNoPayout)
	}
	if _, err := settlements(t, f, stub.New()).Settle(ctx, uuid.Nil); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Settle() with no request = %v, want %v", err, payout.ErrNotSettled)
	}
}

// closingRail answers about a payment and takes the database down on the way,
// which is the ordering that matters: the rail is asked OUTSIDE the
// transaction, so everything between its answer and the commit can fail with
// a member's money already moved.
type closingRail struct {
	payout.Rail
	pool *pgxpool.Pool
}

func (c closingRail) Status(ctx context.Context, reference payout.RailReference) (payout.RailStatus, error) {
	status, err := c.Rail.Status(ctx, reference)
	c.pool.Close()
	return status, err
}

// TestADatabaseThatGoesAwayWhileTheRailIsThinkingRecordsNothing. The payment
// arrived and this service could not write it down. Nothing may be half
// recorded - a payout marked settled whose request still reads approved is a
// member being told two different things by two screens - so the whole
// transaction is lost and the payout stays submitted for the next sweep.
func TestADatabaseThatGoesAwayWhileTheRailIsThinkingRecordsNothing(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)
	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}

	sweep, err := payout.NewSettlements(discardLogger(), f.pool,
		closingRail{Rail: rail, pool: f.pool}, retries(t, f, rail))
	if err != nil {
		t.Fatalf("NewSettlements(): %v", err)
	}
	if _, err := sweep.Settle(ctx, request.ID); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Settle() over a closed pool = %v, want %v", err, payout.ErrNotSettled)
	}

	// Read on a live connection: nothing was written and nothing announced.
	var state string
	if err := pool.QueryRow(ctx,
		`select state from cashback.payout where request_id = $1`,
		pgtype.UUID{Bytes: request.ID, Valid: true}).Scan(&state); err != nil {
		t.Fatalf("re-reading the payout: %v", err)
	}
	if state != string(payout.StatusSubmitted) {
		t.Errorf("the payout is %s, want it still %s for the next sweep", state, payout.StatusSubmitted)
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 0 {
		t.Errorf("a settlement that could not be recorded announced %d events, want none", len(events))
	}
}

// TestASweepThatCannotReadItsWorkListSaysSo. A sweep that answered "nothing to
// settle" when it could not read the table would look identical to a healthy
// one with an empty queue, every five minutes, forever.
func TestASweepThatCannotReadItsWorkListSaysSo(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	rail := stub.New()
	sweep := settlements(t, f, rail)

	f.pool.Close()

	if err := sweep.Sweep(ctx); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Sweep() over a closed pool = %v, want %v", err, payout.ErrNotSettled)
	}
}

// TestASweepCutShortLeavesTheRestForNextTime. The scheduler bounds a run, and
// a sweep that ignored that would hold its lock past the timeout. What it may
// NOT do is lose the payments it did not reach: they are still submitted, so
// the next tick starts where this one stopped.
func TestASweepCutShortLeavesTheRestForNextTime(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New()
	f, request, _ := submittedThrough(ctx, t, rail)

	stopped, cancel := context.WithCancel(ctx)
	cancel()

	if err := settlements(t, f, rail).Sweep(stopped); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Sweep() on a cancelled context = %v, want %v", err, payout.ErrNotSettled)
	}
	waiting, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if waiting.State != payout.StateApproved {
		t.Errorf("the request is %s, want it left at %s for the next sweep", waiting.State, payout.StateApproved)
	}
}

// TestASettledPaymentWhoseRequestIsNotApprovedIsRefused. The payout and the
// request are written together, so through this API they cannot disagree.
// They can disagree in a database somebody has been in by hand, and then the
// question is what a sweep does about it: nothing, loudly. Guessing would
// move money's paper trail on a hunch.
func TestASettledPaymentWhoseRequestIsNotApprovedIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)
	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}

	// Staged by hand, because no code path produces it: the payout is still
	// submitted and the request has moved on without it.
	if _, err := f.pool.Exec(ctx,
		`update cashback.withdrawal_request set state = 'failed' where id = $1`,
		pgtype.UUID{Bytes: request.ID, Valid: true}); err != nil {
		t.Fatalf("staging the inconsistency: %v", err)
	}

	if _, err := settlements(t, f, rail).Settle(ctx, request.ID); !errors.Is(err, payout.ErrNotSettled) {
		t.Errorf("Settle() on an inconsistent pair = %v, want %v", err, payout.ErrNotSettled)
	}
	// And nothing was written: the payout is where it was, and no arrival
	// was announced for a member whose request says it failed.
	var state string
	if err := pool.QueryRow(ctx,
		`select state from cashback.payout where request_id = $1`,
		pgtype.UUID{Bytes: request.ID, Valid: true}).Scan(&state); err != nil {
		t.Fatalf("re-reading the payout: %v", err)
	}
	if state != string(payout.StatusSubmitted) {
		t.Errorf("the payout is %s, want it untouched at %s", state, payout.StatusSubmitted)
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 0 {
		t.Errorf("an inconsistent pair announced %d settlements, want none", len(events))
	}
}

// TestTheSweepLearnsNothingFromTheManualRail, and that is the manual rail
// being honest rather than the sweep being broken.
//
// A manual payout moves when a person goes to a bank. The rail has no way to
// learn what they did, and one that guessed "settled" because time had passed
// would report money as delivered on the strength of a clock. So it answers
// submitted, always.
//
// The consequence is worth stating where somebody will find it: on the
// alpha's default rail this sweep runs and settles nothing. What closes the
// loop there is an operator recording the settlement against the payout row -
// the schema leaves state, settled_at and rail_reference movable for exactly
// that - and no endpoint does it yet. The sweep is the half that works the
// moment a rail which can answer is configured.
func TestTheSweepLearnsNothingFromTheManualRail(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, approvableAmount*2)
	operator := seedOperator(ctx, t, pool)
	// A manual destination: a rail refuses one it cannot carry, and the
	// fixture's default is the stub's.
	toABank := seedDestinationOfKind(ctx, t, f.pool, f.member, true, "manual")
	request, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: toABank, Amount: euro(t, approvableAmount),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}

	rail := manual.New()
	sent, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	settled, err := settlements(t, f, rail).Settle(ctx, request.ID)
	if err != nil {
		t.Fatalf("Settle(): %v", err)
	}
	if settled.Status != payout.StatusSubmitted {
		t.Errorf("the manual rail said %s, want %s - it cannot know", settled.Status, payout.StatusSubmitted)
	}
	if settled.Payout.State != payout.StatusSubmitted {
		t.Errorf("the payout is %s, want it untouched", settled.Payout.State)
	}
	if events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID); len(events) != 0 {
		t.Errorf("the manual rail produced %d settlements, want none", len(events))
	}
}

// TestAPaymentTheRailNeverAcknowledgedIsTheRetryPathsWork. A submission that
// timed out leaves a real payout with no rail reference, in state submitted -
// so this sweep picks it up, and there is nothing it can do: a rail cannot
// answer about a payment it did not confirm taking.
//
// Without this the sweep would fail on every such payout, every five minutes,
// for as long as one sat there - a page of identical errors describing a
// state only a retry can move.
func TestAPaymentTheRailNeverAcknowledgedIsTheRetryPathsWork(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	// A submission that timed out: the payout is committed before the rail
	// is asked (FR-052), so it exists with no reference.
	rail := stub.New(stub.WithTimeout())
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	if _, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("the approval = %v, want a retryable rail failure", err)
	}
	if row := payoutOf(ctx, t, f, request.ID); row.RailReference.Valid {
		t.Fatalf("the payout carries %q, want nothing recorded", row.RailReference.String)
	}

	sweep := settlements(t, f, rail)
	if _, err := sweep.Settle(ctx, request.ID); !errors.Is(err, payout.ErrNotAcknowledged) {
		t.Errorf("Settle() = %v, want %v", err, payout.ErrNotAcknowledged)
	}
	// And the sweep counts it apart from a failure. The error a sweep
	// returns is one line for the whole run, so what distinguishes the two
	// is what it SAYS: a WARN naming a queue for the retry path, not an
	// ERROR about a payment it could not resolve.
	var said strings.Builder
	loud, err := payout.NewSettlements(
		slog.New(slog.NewTextHandler(&said, &slog.HandlerOptions{Level: slog.LevelWarn})),
		f.pool, rail, retries(t, f, rail))
	if err != nil {
		t.Fatalf("NewSettlements(): %v", err)
	}
	_ = loud.Sweep(ctx)
	if !strings.Contains(said.String(), "never acknowledged") {
		t.Errorf("the sweep said nothing about payments waiting on a retry: %s", said.String())
	}
	if strings.Contains(said.String(), "level=ERROR") && strings.Contains(said.String(), request.ID.String()) {
		t.Errorf("the unacknowledged payment was reported as a failure: %s", said.String())
	}
	// It is still there for the retry, untouched.
	waiting, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if waiting.State != payout.StateApproved {
		t.Errorf("the request is %s, want it left at %s for a retry", waiting.State, payout.StateApproved)
	}
}

// TestAnOperatorRecordsAPaymentTheyMadeByHand is the manual rail's whole
// settlement path, and the only one it has: a person went to a bank, so a
// person says so.
//
// What must be true afterwards is what a rail-reported settlement leaves
// behind, plus one thing more - the reference is the BANK's, not the
// manual: placeholder that meant nobody had made the transfer yet.
func TestAnOperatorRecordsAPaymentTheyMadeByHand(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, approvableAmount*2)
	operator := seedOperator(ctx, t, pool)
	toABank := seedDestinationOfKind(ctx, t, f.pool, f.member, true, "manual")
	request, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: toABank, Amount: euro(t, approvableAmount),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}
	rail := manual.New()
	sent, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	if !strings.HasPrefix(sent.RailReference, "manual:") {
		t.Fatalf("the submission recorded %q, want the manual: placeholder", sent.RailReference)
	}
	approved := decisionOf(ctx, t, f.pool, request.ID)

	const fromTheBank = "DE-SEPA-2026-09-01-000417"
	settled, err := settlements(t, f, rail).Record(ctx, payout.Recording{
		Request: request.ID, Operator: operator, Reference: fromTheBank,
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	if settled.Payout.State != payout.StatusSettled {
		t.Errorf("the payout is %s, want %s", settled.Payout.State, payout.StatusSettled)
	}
	if settled.Payout.RailReference != fromTheBank {
		t.Errorf("the reference is %q, want the bank's %q", settled.Payout.RailReference, fromTheBank)
	}
	if settled.Payout.SettledAt.IsZero() {
		t.Error("the payout carries no settlement instant")
	}
	paid, err := f.withdrawals.Get(ctx, f.member, request.ID)
	if err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if paid.State != payout.StatePaid {
		t.Errorf("the request is %s, want %s", paid.State, payout.StatePaid)
	}
	// The approval is untouched, as it is when a rail reports the arrival.
	switch after := decisionOf(ctx, t, f.pool, request.ID); {
	case after.By != approved.By:
		t.Errorf("the decision is now %s's, want the approver's %s", after.By, approved.By)
	case !after.At.Equal(approved.At):
		t.Errorf("decided_at moved to %s, want the approval's %s", after.At, approved.At)
	}

	// Announced with the human who said so, which a rail-reported
	// settlement has no way to carry.
	events := announcementsOf(ctx, t, pool, "cashback.payout.settled", settled.Payout.ID)
	if len(events) != 1 {
		t.Fatalf("got %d settlement events, want exactly 1", len(events))
	}
	if events[0].Payload["actor"] != operator.String() {
		t.Errorf("actor = %v, want the operator %s (FR-061)", events[0].Payload["actor"], operator)
	}
	if events[0].Payload["rail_reference"] != fromTheBank {
		t.Errorf("rail_reference = %v, want the bank's %q", events[0].Payload["rail_reference"], fromTheBank)
	}
}

// TestARailReportedSettlementNamesNobody. Both are real settlements and
// neither is more true, but only one has a human to ask about it - and a
// consumer cannot tell them apart from the payload otherwise.
func TestARailReportedSettlementNamesNobody(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	rail := stub.New()
	f, request, sent := submittedThrough(ctx, t, rail)
	if err := rail.Settle(mustReference(t, sent.RailReference)); err != nil {
		t.Fatalf("settling at the rail: %v", err)
	}
	if _, err := settlements(t, f, rail).Settle(ctx, request.ID); err != nil {
		t.Fatalf("Settle(): %v", err)
	}

	events := announcementsOf(ctx, t, pool, "cashback.payout.settled", sent.ID)
	if len(events) != 1 {
		t.Fatalf("got %d settlement events, want exactly 1", len(events))
	}
	if actor, present := events[0].Payload["actor"]; present {
		t.Errorf("actor = %v on a rail-reported settlement, want it absent", actor)
	}
}

// TestWhatRecordingRefuses. Each of these would settle a payment on less
// evidence than a settlement needs.
func TestWhatRecordingRefuses(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	rail := stub.New()
	f, request, _ := submittedThrough(ctx, t, rail)
	sweep := settlements(t, f, rail)
	operator := seedOperator(ctx, t, f.pool)

	for name, recording := range map[string]payout.Recording{
		"no request":  {Operator: operator, Reference: "ref"},
		"no operator": {Request: request.ID, Reference: "ref"},
		// The placeholder means nobody has made the transfer. Settling
		// without replacing it would say money landed and leave nothing to
		// trace it by.
		"no reference":    {Request: request.ID, Operator: operator},
		"blank reference": {Request: request.ID, Operator: operator, Reference: "   "},
	} {
		if _, err := sweep.Record(ctx, recording); !errors.Is(err, payout.ErrNotSettled) {
			t.Errorf("%s = %v, want %v", name, err, payout.ErrNotSettled)
		}
	}
}
