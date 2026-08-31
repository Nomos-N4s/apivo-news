package earnings_test

// A purchase nobody can be credited for, against the real schema (T074,
// FR-034, FR-061, SC-002).
//
// The property is a negative and it is the reason the queue exists: a report
// whose reference matches no click credits NOBODY. Not a guess, not the
// member who clicked something similar, not the house. It becomes a row an
// operator can see, and the only thing an operator may do to it here is
// dismiss it - which closes the work and still credits nobody.
//
// The whole journey runs in one transaction that is rolled back, and every
// step is the production code path.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	clickoutstore "github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

// aReferenceNobodyMinted is a well-formed click reference that was never
// issued. Well-formed on purpose: a malformed one would be refused before it
// reached the queue, and would prove nothing about the case that matters -
// networks echoing references minted by other publishers and by links that
// predate a deployment.
const aReferenceNobodyMinted = "not-a-reference-this-deployment-ever-minted"

// unreadableClicks is a click store that cannot be read at all - a dropped
// connection rather than a reference nobody minted. The only stand-in in
// this file, because a database that answers correctly cannot be made to
// fail this way without breaking it for the case as well.
type unreadableClicks struct{}

func (unreadableClicks) ByRef(context.Context, networks.ClickRef) (clickout.Click, error) {
	return clickout.Click{}, errors.New("connection reset by peer")
}

// creditsFor counts the entries citing one report. The negative this file is
// about, counted the way the schema would be asked.
func (j *theJourney) creditsFor(t *testing.T, report uuid.UUID) int {
	t.Helper()
	var n int
	if err := j.tx.QueryRow(j.ctx,
		`select count(*) from cashback.entry where network_transaction_id = $1`, report).Scan(&n); err != nil {
		t.Fatalf("counting credits: %v", err)
	}
	return n
}

// openQueueRow answers the queue row for a report and whether it is still
// open work.
func (j *theJourney) openQueueRow(t *testing.T, report uuid.UUID) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	var open bool
	err := j.tx.QueryRow(j.ctx, `
		select id, resolved_at is null
		  from cashback.unattributed_transaction
		 where network_transaction_id = $1`, report).Scan(&id, &open)
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	return id, open
}

// TestAnUnknownReferenceCreditsNobodyAndBecomesOperatorWork walks the miss
// from the report to the operator's decision.
func TestAnUnknownReferenceCreditsNobodyAndBecomesOperatorWork(t *testing.T) {
	j := begin(t)
	j.seed(t)

	// A real click by a real member, so the case cannot pass by there being
	// nobody to credit. The report below cites a DIFFERENT reference.
	clicked := j.clickOut(t)

	report := j.reports(t, aReferenceNobodyMinted, networks.StatusConfirmed)

	clicks, err := clickout.NewClicks(clickoutstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	matcher, err := earnings.NewMatcher(clicks, earningsstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}

	attributed, err := matcher.Match(j.ctx, j.tx,
		earnings.Report{ID: report, Ref: networks.NewClickRef(aReferenceNobodyMinted)})
	// A miss is ORDINARY. Reported as a value rather than an error, so a
	// caller walking a window cannot mistake it for a failure and stop.
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}
	if attributed.Matched {
		t.Fatalf("a reference nobody minted matched click %s", attributed.Click.ID)
	}
	if attributed.Queued == uuid.Nil {
		t.Fatal("the report matched nothing and was queued nowhere, so the money is invisible")
	}
	if attributed.Click.ID == clicked.ID {
		t.Fatal("the report was attributed to the only click there was, which is a guess and not a match")
	}

	// Nobody is credited. Not the member who clicked, not anybody.
	if n := j.creditsFor(t, report); n != 0 {
		t.Errorf("%d entry(ies) cite a report nobody can be credited for, want none", n)
	}
	for _, stage := range []wallet.Stage{wallet.StageHeld, wallet.StagePending, wallet.StageConfirmed} {
		if got := j.balance(t, stage); got != 0 {
			t.Errorf("the member's %s balance is %d after an unattributed report, want 0", stage, got)
		}
	}
	j.wantZeroSum(t)

	// It is visible as work: one open row, naming the report.
	row, open := j.openQueueRow(t, report)
	if row != attributed.Queued {
		t.Errorf("the queue row is %s, want the one Match reported (%s)", row, attributed.Queued)
	}
	if !open {
		t.Error("the queue row was closed the moment it was written, so no operator would ever see it")
	}

	// And announced, so a consumer does not have to know to go and look.
	announced := j.announcedAbout(t, report)
	queued := announced[earnings.TypeTransactionUnattributed]
	if len(queued) != 1 {
		t.Fatalf("announced %s %d times, want once", earnings.TypeTransactionUnattributed, len(queued))
	}
	if queued[0]["network_transaction_id"] != report.String() {
		t.Errorf("the event names report %v, want %s", queued[0]["network_transaction_id"], report)
	}

	// The operator dismisses it: who, when and why, together.
	store, err := ops.NewPGStore(j.tx)
	if err != nil {
		t.Fatalf("NewPGStore(): %v", err)
	}
	dismissed, err := store.Dismiss(j.ctx, ops.Dismissal{
		ID:       row,
		Operator: ops.Operator{ID: j.operator, Email: "operator@example.test", DisplayName: "Journey Operator"},
		Reason:   "the reference belongs to another publisher",
	})
	if err != nil {
		t.Fatalf("Dismiss(): %v", err)
	}
	if dismissed.ResolvedBy != j.operator {
		t.Errorf("the dismissal names %s, want the operator %s", dismissed.ResolvedBy, j.operator)
	}
	if dismissed.ResolvedAt.IsZero() || dismissed.Reason == "" {
		t.Errorf("the dismissal recorded %v by %s with reason %q; FR-061 wants all three",
			dismissed.ResolvedAt, dismissed.ResolvedBy, dismissed.Reason)
	}

	// Dismissing closes the WORK. It does not credit anybody, which is the
	// whole distinction: the row existed so the money was visible, not so it
	// was paid.
	if _, stillOpen := j.openQueueRow(t, report); stillOpen {
		t.Error("the queue row is still open after a dismissal that reported success")
	}
	if n := j.creditsFor(t, report); n != 0 {
		t.Errorf("dismissing the queue row created %d entry(ies), want none", n)
	}
	j.wantZeroSum(t)
}

// TestAQueueRowMayNotBeReHomed is what makes the row evidence rather than a
// note. Which report went unattributed and when it was noticed are frozen;
// only the resolution is a decision anyone may still make.
func TestAQueueRowMayNotBeReHomed(t *testing.T) {
	j := begin(t)
	j.seed(t)

	report := j.reports(t, aReferenceNobodyMinted, networks.StatusConfirmed)
	other := j.reports(t, "another-reference-nobody-minted", networks.StatusConfirmed)

	clicks, err := clickout.NewClicks(clickoutstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	matcher, err := earnings.NewMatcher(clicks, earningsstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}
	if _, err := matcher.Match(j.ctx, j.tx,
		earnings.Report{ID: report, Ref: networks.NewClickRef(aReferenceNobodyMinted)}); err != nil {
		t.Fatalf("Match(): %v", err)
	}
	row, _ := j.openQueueRow(t, report)

	// A savepoint, because the refusal aborts whatever it is raised in.
	sub, err := j.tx.Begin(j.ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err = sub.Exec(j.ctx,
		`update cashback.unattributed_transaction set network_transaction_id = $2 where id = $1`, row, other)
	_ = sub.Rollback(j.ctx)

	if err == nil {
		t.Fatal("a queue row was moved onto a different report, so the history of what an operator looked at is gone")
	}
}

// TestAMissIsNotAFailedRead. A read that FAILED is not a read that found
// nothing: queueing on one would turn a dropped connection into a permanent
// record that this purchase went unattributed - a record the schema freezes
// and nothing later re-examines.
func TestAMissIsNotAFailedRead(t *testing.T) {
	j := begin(t)
	j.seed(t)

	report := j.reports(t, aReferenceNobodyMinted, networks.StatusConfirmed)
	matcher, err := earnings.NewMatcher(unreadableClicks{}, earningsstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}

	_, err = matcher.Match(j.ctx, j.tx,
		earnings.Report{ID: report, Ref: networks.NewClickRef(aReferenceNobodyMinted)})

	if err == nil {
		t.Fatal("a click store that could not be read reported a miss")
	}
	if errors.Is(err, earnings.ErrNotQueued) {
		t.Fatalf("a failed read was treated as a miss and queued: %v", err)
	}
	var n int
	if scanErr := j.tx.QueryRow(j.ctx,
		`select count(*) from cashback.unattributed_transaction where network_transaction_id = $1`,
		report).Scan(&n); scanErr != nil {
		t.Fatalf("counting the queue: %v", scanErr)
	}
	if n != 0 {
		t.Errorf("a failed read left %d permanent queue row(s), want none", n)
	}
}
