package earnings_test

// Releasing and rejecting a held credit against the real schema and the
// real ledger (T119, T121, US7 scenario 3, FR-061): each records a named
// human and a reason in the transaction that moves the money, announces
// it beside the move, and leaves the credit's own row exactly as it was.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// share is the member's share of the reported commission at the click-time
// band.
func (j *theJourney) share(t *testing.T, click clickout.Click) money.Amount {
	t.Helper()
	share, err := earnings.ShareOf(money.Amount{Minor: reportedCommission, Currency: "EUR"}, click.Promised)
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	return share.Member
}

// reviews builds the service over this journey's transaction and ledger.
func (j *theJourney) reviews(t *testing.T) *earnings.Reviews {
	t.Helper()
	r, err := earnings.NewReviews(j.tx, j.ledger, houseReceivable)
	if err != nil {
		t.Fatalf("NewReviews(): %v", err)
	}
	return r
}

// heldByTheRules drives a self-dealing pattern through the crediting job
// under the shared-context rule, and answers the held credit's report.
func (j *theJourney) heldByTheRules(t *testing.T) (report uuid.UUID, held storedEntry) {
	t.Helper()
	device := clickout.NewContextDigest("198.51.100.7", "Mozilla/5.0 (same phone)")
	j.clickFrom(t, j.anotherMember(t), device)
	click := j.clickFrom(t, j.member, device)
	report = j.reports(t, click.Ref.Ref(), networks.StatusConfirmed)
	job := j.lifecycle(t, earnings.HoldRules{SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour})
	if out := j.runs(t, job); out != (earnings.Outcome{Held: 1}) {
		t.Fatalf("the run did %+v, want one hold", out)
	}
	held = j.theOneEntryCiting(t, report)
	if held.State != string(earnings.StateHeld) {
		t.Fatalf("the credit is %s, want held", held.State)
	}
	return report, held
}

// heldQueue reads the whole queue.
func (j *theJourney) heldQueue(t *testing.T) []earnings.HeldCredit {
	t.Helper()
	queue, err := j.reviews(t).Held(j.ctx, earnings.HeldAfter{}, 100)
	if err != nil {
		t.Fatalf("Held(): %v", err)
	}
	return queue
}

// listed answers whether the queue lists an entry.
func listed(queue []earnings.HeldCredit, id uuid.UUID) bool {
	for _, credit := range queue {
		if credit.ID == id {
			return true
		}
	}
	return false
}

func TestAnOperatorReleasesAHeldCreditWithTheirNameAndReason(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	report, held := j.heldByTheRules(t)
	reviews := j.reviews(t)

	// Listed, with the rule, what the rule said, and what the network
	// reported - the whole of what an operator decides from.
	queue := j.heldQueue(t)
	if !listed(queue, held.ID) {
		t.Fatalf("the held credit is not in the queue: %+v", queue)
	}
	var row earnings.HeldCredit
	for _, credit := range queue {
		if credit.ID == held.ID {
			row = credit
		}
	}
	if row.Rule != earnings.RuleSharedContext || row.Reason == "" || row.Report != report || row.Member != j.member {
		t.Errorf("the queue row is %+v, want the shared-context rule with its reason, report %s, member %s", row, report, j.member)
	}
	if row.Amount.Minor != clickTimeMemberShare || row.Sale.Minor != reportedSaleMinor || row.Commission.Minor != reportedCommission || row.ReportStatus != networks.StatusConfirmed {
		t.Errorf("the queue row carries amount %v sale %v commission %v status %s", row.Amount, row.Sale, row.Commission, row.ReportStatus)
	}

	// Released by a named human, for a reason.
	released, err := reviews.Release(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "  the second account is the member's partner; reviewed  "})
	if err != nil {
		t.Fatalf("Release(): %v", err)
	}
	if released.Entry.State != earnings.StatePending || released.Entry.HoldRule != "" {
		t.Errorf("released to %s under %q, want pending under no rule", released.Entry.State, released.Entry.HoldRule)
	}
	if released.ReleasedBy != j.operator || released.Reason != "the second account is the member's partner; reviewed" || released.Rule != earnings.RuleSharedContext || released.Transfer == "" || released.At.IsZero() {
		t.Errorf("the release reads %+v, want the operator, the trimmed reason, the rule and the transfer", released)
	}
	if actor, reason := j.transitionRecorded(t, held.ID, earnings.StatePending); actor != j.operator || reason != released.Reason {
		t.Errorf("the transition recorded actor %s reason %q, want %s and %q", actor, reason, j.operator, released.Reason)
	}
	j.wantBalances(t, 0, clickTimeMemberShare, 0)
	j.wantZeroSum(t)

	// Announced, with the acting account and the reason in the payload.
	announced := j.announcedAbout(t, held.ID)[earnings.TypeHoldReleased]
	if len(announced) != 1 || announced[0]["released_by"] != j.operator.String() || announced[0]["reason"] != released.Reason || announced[0]["hold_rule"] != earnings.RuleSharedContext {
		t.Errorf("announced %s as %v, want once with the operator, the reason and the rule", earnings.TypeHoldReleased, announced)
	}

	// Out of the queue, and not decidable twice.
	if listed(j.heldQueue(t), held.ID) {
		t.Error("a released credit is still in the held queue")
	}
	var notHeld earnings.NotHeldError
	if _, err := reviews.Release(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "again"}); !errors.As(err, &notHeld) || notHeld.State != earnings.StatePending {
		t.Errorf("releasing twice = %v, want NotHeldError naming pending", err)
	}
	if _, err := reviews.Reject(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "changed my mind"}); !errors.Is(err, earnings.ErrNotHeld) {
		t.Errorf("rejecting a released credit = %v, want ErrNotHeld", err)
	}

	// And ordinary from here: the network's word and a statement confirm
	// it on the next run, as they would any pending credit.
	j.covered(t)
	if out := j.runs(t, j.lifecycle(t, earnings.HoldRules{})); out != (earnings.Outcome{Confirmed: 1}) {
		t.Fatalf("after the release the run did %+v, want one confirmation", out)
	}
	j.wantBalances(t, 0, 0, clickTimeMemberShare)
	j.wantZeroSum(t)
}

func TestAnOperatorRejectsAHeldCreditAndTheMoneyGoesBack(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	report, held := j.heldByTheRules(t)
	reviews := j.reviews(t)

	rejected, err := reviews.Reject(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "both accounts are the same person"})
	if err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	// A reversing entry beside the credit, citing the credit's own report,
	// born reversed, with the operator and the reason on its opening.
	if rejected.Reversal.State != earnings.StateReversed || rejected.Reversal.ReversalOf != held.ID || rejected.Reversal.Report != report || rejected.Reversal.Amount.Minor != clickTimeMemberShare {
		t.Errorf("the reversing entry is %+v, want reversed, undoing %s against report %s for %d", rejected.Reversal, held.ID, report, clickTimeMemberShare)
	}
	if rejected.RejectedBy != j.operator || rejected.Reason != "both accounts are the same person" || rejected.Rule != earnings.RuleSharedContext {
		t.Errorf("the rejection reads %+v, want the operator, the reason and the rule", rejected)
	}
	if actor, reason := j.transitionRecorded(t, rejected.Reversal.ID, earnings.StateReversed); actor != j.operator || reason != "both accounts are the same person" {
		t.Errorf("the reversal's opening recorded actor %s reason %q", actor, reason)
	}
	// The credit's own row is left exactly as it was, and the pair - the
	// credit and its reversal - both cite the report.
	pair := j.entriesCiting(t, report)
	if len(pair) != 2 {
		t.Fatalf("report %s has %d entries after the rejection, want the auditable pair: %+v", report, len(pair), pair)
	}
	for _, entry := range pair {
		if entry.ID == held.ID && (entry.State != string(earnings.StateHeld) || entry.HoldRule != earnings.RuleSharedContext) {
			t.Errorf("the rejected credit's row is %+v; a rejection edits nothing", entry)
		}
	}
	j.wantBalances(t, 0, 0, 0)
	j.wantZeroSum(t)

	announced := j.announcedAbout(t, held.ID)[earnings.TypeHoldRejected]
	if len(announced) != 1 || announced[0]["rejected_by"] != j.operator.String() || announced[0]["reversal_entry_id"] != rejected.Reversal.ID.String() {
		t.Errorf("announced %s as %v, want once naming the operator and the reversing entry", earnings.TypeHoldRejected, announced)
	}

	// Out of the queue, decided once, and not releasable afterwards.
	if listed(j.heldQueue(t), held.ID) {
		t.Error("a rejected credit is still in the held queue")
	}
	if _, err := reviews.Reject(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "again"}); !errors.Is(err, earnings.ErrAlreadyRejected) {
		t.Errorf("rejecting twice = %v, want ErrAlreadyRejected", err)
	}
	if _, err := reviews.Release(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "changed my mind"}); !errors.Is(err, earnings.ErrAlreadyRejected) {
		t.Errorf("releasing a rejected credit = %v, want ErrAlreadyRejected", err)
	}
	j.wantBalances(t, 0, 0, 0)

	// And the crediting job neither re-credits the report nor reverses it
	// again: the decision stands.
	j.covered(t)
	if out := j.runs(t, j.lifecycle(t, earnings.HoldRules{})); out != (earnings.Outcome{}) {
		t.Fatalf("after the rejection the run did %+v, want nothing", out)
	}
	j.wantBalances(t, 0, 0, 0)
}

func TestAReviewNamesACreditThatIsHeld(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	reviews := j.reviews(t)

	if _, err := reviews.Release(j.ctx, earnings.Review{Entry: uuid.New(), Operator: j.operator, Reason: "nothing there"}); !errors.Is(err, earnings.ErrNoSuchEntry) {
		t.Errorf("releasing an unknown id = %v, want ErrNoSuchEntry", err)
	}
	if _, err := reviews.Reject(j.ctx, earnings.Review{Entry: uuid.Nil, Operator: j.operator, Reason: "nothing there"}); !errors.Is(err, earnings.ErrInvalidReview) {
		t.Errorf("rejecting nothing = %v, want ErrInvalidReview", err)
	}

	// A pending credit is not held, and the queue does not list it.
	click := j.clickOut(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)
	if out := j.runs(t, j.lifecycle(t, earnings.HoldRules{})); out != (earnings.Outcome{Credited: 1}) {
		t.Fatalf("the run did %+v, want one credit", out)
	}
	pending := j.theOneEntryCiting(t, report)
	var notHeld earnings.NotHeldError
	if _, err := reviews.Reject(j.ctx, earnings.Review{Entry: pending.ID, Operator: j.operator, Reason: "looks wrong"}); !errors.As(err, &notHeld) || notHeld.State != earnings.StatePending {
		t.Errorf("rejecting a pending credit = %v, want NotHeldError naming pending", err)
	}
	if listed(j.heldQueue(t), pending.ID) {
		t.Error("a pending credit is listed in the held queue")
	}
	j.wantBalances(t, 0, clickTimeMemberShare, 0)
}

// TestTheHeldQueuePagesOldestFirst. The cursor is the row's own position,
// so a page continues exactly after the last row shown.
func TestTheHeldQueuePagesOldestFirst(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	machine, _ := j.entries(t)
	// Three held credits, opened by hand under a rule, in one transaction:
	// created_at ties, so the id is the tie-break the page must honour.
	var ids []uuid.UUID
	for range 3 {
		click := j.clickOut(t)
		report := j.reports(t, click.Ref.Ref(), networks.StatusPending)
		opened, err := machine.Open(j.ctx, j.tx, earnings.Credit{
			Member: j.member, Brand: "apivo-de", Report: report, Click: click.ID,
			State: earnings.StateHeld, HoldRule: earnings.RuleSaleCap,
			Amount: j.share(t, click), Reason: "over the cap",
		})
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		ids = append(ids, opened.ID)
	}
	reviews := j.reviews(t)
	var seen []uuid.UUID
	after := earnings.HeldAfter{}
	for range 4 {
		page, err := reviews.Held(j.ctx, after, 2)
		if err != nil {
			t.Fatalf("Held(): %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, credit := range page {
			if credit.Reason != "over the cap" || credit.Rule != earnings.RuleSaleCap {
				t.Errorf("row %s carries rule %q reason %q", credit.ID, credit.Rule, credit.Reason)
			}
			seen = append(seen, credit.ID)
		}
		after = page[len(page)-1].After()
	}
	for _, id := range ids {
		n := 0
		for _, s := range seen {
			if s == id {
				n++
			}
		}
		if n != 1 {
			t.Errorf("credit %s was listed %d times across the pages, want once", id, n)
		}
	}
}

// TestAReviewNeedsTheHouseAccountNamed. The receivable is refused at the
// decision rather than at construction, so the queue stays readable in a
// deployment that cannot move money yet - and neither decision moves any.
func TestAReviewNeedsTheHouseAccountNamed(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	report, held := j.heldByTheRules(t)
	unnamed, err := earnings.NewReviews(j.tx, j.ledger, "")
	if err != nil {
		t.Fatalf("NewReviews(): %v", err)
	}
	if queue, err := unnamed.Held(j.ctx, earnings.HeldAfter{}, 10); err != nil || !listed(queue, held.ID) {
		t.Errorf("the queue is not readable without a receivable: %v (err %v)", queue, err)
	}
	if _, err := unnamed.Release(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "fine"}); !errors.Is(err, earnings.ErrNoReceivable) {
		t.Errorf("Release() without a receivable = %v, want ErrNoReceivable", err)
	}
	if _, err := unnamed.Reject(j.ctx, earnings.Review{Entry: held.ID, Operator: j.operator, Reason: "not fine"}); !errors.Is(err, earnings.ErrNoReceivable) {
		t.Errorf("Reject() without a receivable = %v, want ErrNoReceivable", err)
	}
	j.wantBalances(t, clickTimeMemberShare, 0, 0)
	if got := j.theOneEntryCiting(t, report); got.State != string(earnings.StateHeld) {
		t.Errorf("a refused decision left the credit %s", got.State)
	}
}

// TestAHeldRowNothingHeldCannotBeReleased. D7 forbids a state without its
// transition; a held row with no transition into held is one somebody
// wrote around the machine, and a release keyed on the hold that put it
// there has nothing to derive its key from.
func TestAHeldRowNothingHeldCannotBeReleased(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	click := j.clickOut(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)
	var orphan uuid.UUID
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.entry
		    (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, hold_rule)
		values ('apivo-de', $1, $2, $3, 'held', 150, 'EUR', $4) returning id`,
		j.member, report, click.ID, earnings.RuleSaleCap).Scan(&orphan); err != nil {
		t.Fatalf("writing a held row around the machine: %v", err)
	}
	_, err := j.reviews(t).Release(j.ctx, earnings.Review{Entry: orphan, Operator: j.operator, Reason: "looks fine"})
	if !errors.Is(err, earnings.ErrNotReviewed) || !strings.Contains(err.Error(), "no transition held it") {
		t.Errorf("Release() of a row nothing held = %v, want ErrNotReviewed saying no transition held it", err)
	}
	j.wantBalances(t, 0, 0, 0)
}
