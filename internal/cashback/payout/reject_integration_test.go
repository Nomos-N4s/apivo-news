// The tests for reject.go, against the real schema (T093, US4 scenario 5).
//
// What is under test is a balance moving back, so a fake ledger would be
// asserting against itself. The scratch database and the shared memory ledger
// the withdrawal cases use are both real enough to disagree with the code.

package payout_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

// rejections builds the refuser over the fixture's pool and shared ledger.
func rejections(t *testing.T, f fixture) *payout.Rejections {
	t.Helper()
	r, err := payout.NewRejections(f.pool, f.ledger, receivable)
	if err != nil {
		t.Fatalf("NewRejections(): %v", err)
	}
	return r
}

// TestARefusedWithdrawalPutsTheMoneyBack is US4 scenario 5 in one case: the
// reserved amount returns to confirmed and the member is told why.
func TestARefusedWithdrawalPutsTheMoneyBack(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	before := f.stageBalance(ctx, t, wallet.StageConfirmed)
	reserved := f.stageBalance(ctx, t, wallet.StageReserved)
	if reserved != request.Amount {
		t.Fatalf("the reserved stage holds %s before the refusal, want %s", reserved, request.Amount)
	}

	refused, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request:  request.ID,
		Operator: operator,
		Reason:   "the destination bank returned the test credit",
	})
	if err != nil {
		t.Fatalf("Reject(): %v", err)
	}

	if refused.Request.State != payout.StateRejected {
		t.Errorf("the request is %s, want %s", refused.Request.State, payout.StateRejected)
	}
	if refused.Request.DecidedBy != operator {
		t.Errorf("decided_by = %s, want %s", refused.Request.DecidedBy, operator)
	}
	if refused.Request.DecisionReason == "" {
		t.Error("the request records no reason; a member is owed one for money they did not get")
	}
	if refused.Released != request.Amount {
		t.Errorf("released %s, want the %s reserved", refused.Released, request.Amount)
	}
	if refused.ReleaseTransfer == string(request.ReservedTransfer) {
		t.Error("the release reused the reservation's transfer; the ledger would have recorded nothing")
	}

	// The balances, which is the whole claim.
	if back := f.stageBalance(ctx, t, wallet.StageConfirmed); back.Minor != before.Minor+request.Amount.Minor {
		t.Errorf("confirmed holds %s, want %s back on top of %s", back, request.Amount, before)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); !still.IsZero() {
		t.Errorf("the reserved stage still holds %s, want nothing", still)
	}
	// And the entries came with it.
	if states := f.entryStates(ctx, t); states["reserved"] != 0 || states["confirmed"] == 0 {
		t.Errorf("entry states are %v, want nothing reserved", states)
	}
}

// TestARefusedWithdrawalCanBeAskedForAgain. The point of putting the money
// back is that the member is no worse off, so the balance has to be
// withdrawable a second time.
func TestARefusedWithdrawalCanBeAskedForAgain(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, first := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	if _, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: first.ID, Operator: operator, Reason: "asked in error",
	}); err != nil {
		t.Fatalf("Reject(): %v", err)
	}

	second, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: first.Amount,
	})
	if err != nil {
		t.Fatalf("the second request after a refusal: %v", err)
	}
	if second.Amount != first.Amount {
		t.Errorf("the second request reserved %s, want the same %s", second.Amount, first.Amount)
	}
	if second.ReservedTransfer == first.ReservedTransfer {
		t.Error("both requests name one reservation transfer; they are two movements of money")
	}
}

// TestARejectionWithoutAReasonIsRefused. The schema refuses it too
// (withdrawal_request_rejection_has_reason); this refuses it before anything
// is read, so the answer names the rule rather than a constraint - and
// nothing is released for a decision that will not be recorded.
func TestARejectionWithoutAReasonIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	for _, reason := range []string{"", "   ", "\n\t "} {
		_, err := rejections(t, f).Reject(ctx, payout.Rejection{
			Request: request.ID, Operator: operator, Reason: reason,
		})
		if !errors.Is(err, payout.ErrNoDecisionReason) {
			t.Errorf("Reject(%q) = %v, want one wrapping %v", reason, err, payout.ErrNoDecisionReason)
		}
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s after a refused rejection, want the %s still reserved", still, request.Amount)
	}
}

// TestRefusingSomethingAlreadyDecidedIsRefused, and refused before the ledger
// is asked - which is what stops a second release putting the money back
// twice.
func TestRefusingSomethingAlreadyDecidedIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	refuser := rejections(t, f)

	if _, err := refuser.Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: "the first refusal",
	}); err != nil {
		t.Fatalf("the first rejection: %v", err)
	}

	_, err := refuser.Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: "the second refusal",
	})
	if !errors.Is(err, payout.ErrNotAwaitingApproval) {
		t.Fatalf("the second rejection = %v, want one wrapping %v", err, payout.ErrNotAwaitingApproval)
	}
	if back := f.stageBalance(ctx, t, wallet.StageConfirmed); back.Minor != approvableAmount*2 {
		t.Errorf("confirmed holds %s, want the balance released exactly once", back)
	}
}

// TestAnApprovedWithdrawalCannotBeRefused. Once a payout exists the money has
// been submitted, and releasing the reservation would let the member spend a
// balance that has already left.
func TestAnApprovedWithdrawalCannotBeRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	if _, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	_, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: "changed my mind",
	})
	if !errors.Is(err, payout.ErrNotAwaitingApproval) {
		t.Fatalf("Reject() after approval = %v, want one wrapping %v", err, payout.ErrNotAwaitingApproval)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s, want the %s still claimed by the payout", still, request.Amount)
	}
}

// TestRefusingAWithdrawalThatReservedNothingIsRefused. reserved_transfer_ref
// is NOT NULL precisely so every request has entries behind it; one with none
// cannot have its money given back, because nothing can be found to give.
func TestRefusingAWithdrawalThatReservedNothingIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	operator := seedOperator(ctx, t, pool)

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

	_, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: uuid.UUID(id.Bytes), Operator: operator, Reason: "nothing to release",
	})
	if !errors.Is(err, payout.ErrNothingReserved) {
		t.Fatalf("Reject() = %v, want one wrapping %v", err, payout.ErrNothingReserved)
	}
}

// TestARefuserMissingAPartIsRefusedAtConstruction.
func TestARefuserMissingAPartIsRefusedAtConstruction(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	if _, err := payout.NewRejections(nil, f.ledger, receivable); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no database = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewRejections(f.pool, nil, receivable); !errors.Is(err, payout.ErrNoLedger) {
		t.Errorf("with no ledger = %v, want one wrapping %v", err, payout.ErrNoLedger)
	}
}

// TestADeploymentThatNamedNoReceivableAnswersOnTheRefusal. Not refused at
// construction: doing so would take the whole operator surface down over a
// deployment that simply cannot pay out yet. Production refuses to start
// without the key; here the refusal says so.
func TestADeploymentThatNamedNoReceivableAnswersOnTheRefusal(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	refuser, err := payout.NewRejections(f.pool, f.ledger, "")
	if err != nil {
		t.Fatalf("NewRejections() with no receivable = %v, want it built", err)
	}
	if _, err := refuser.Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: "a reason",
	}); !errors.Is(err, payout.ErrNoReceivable) {
		t.Errorf("Reject() = %v, want one wrapping %v", err, payout.ErrNoReceivable)
	}
	if still := f.stageBalance(ctx, t, wallet.StageReserved); still != request.Amount {
		t.Errorf("the reserved stage holds %s, want it untouched", still)
	}
}

// TestARejectionNamingNobodyIsRefused, before any read.
func TestARejectionNamingNobodyIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f, request := approvable(ctx, t)
	refuser := rejections(t, f)

	if _, err := refuser.Reject(ctx, payout.Rejection{
		Request: request.ID, Reason: "who?",
	}); !errors.Is(err, payout.ErrNotRejected) {
		t.Errorf("with no operator = %v, want one wrapping %v", err, payout.ErrNotRejected)
	}
	if _, err := refuser.Reject(ctx, payout.Rejection{
		Operator: uuid.New(), Reason: "what?",
	}); !errors.Is(err, payout.ErrNotRejected) {
		t.Errorf("with no request = %v, want one wrapping %v", err, payout.ErrNotRejected)
	}
}

// TestTheReasonIsStoredAsGiven, trimmed of the whitespace a form adds and
// otherwise untouched: it is the audit record and the member's explanation.
func TestTheReasonIsStoredAsGiven(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	const reason = "the network reversed the commission after the return window"

	refused, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: "  " + reason + "\n",
	})
	if err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	if refused.Request.DecisionReason != reason {
		t.Errorf("reason = %q, want %q", refused.Request.DecisionReason, reason)
	}
	if strings.TrimSpace(refused.Request.DecisionReason) != refused.Request.DecisionReason {
		t.Error("the stored reason carries surrounding whitespace")
	}
}
