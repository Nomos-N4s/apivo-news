package earnings_test

// The crediting job against the real schema and the real ledger (#435):
// what the sweeps stored becomes what a member is owed, confirms when the
// network approves AND a statement covers it, reverses when the network
// takes it back, and a second run over the same rows does nothing.
//
// Every step is the production code path, driven the way the scheduler
// drives it. The job opens its own transactions; here they are savepoints on
// the journey's, so everything rolls back with it.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// lifecycleTestLocker hands out every lock it is asked for.
type lifecycleTestLocker struct{}

func (lifecycleTestLocker) TryLock(context.Context, string) (scheduler.Lock, bool, error) {
	return lifecycleTestLock{}, true, nil
}

type lifecycleTestLock struct{}

func (lifecycleTestLock) Release(context.Context) error { return nil }

// lifecycle builds the job over this journey's transaction and ledger.
func (j *theJourney) lifecycle(t *testing.T, rules earnings.HoldRules) *earnings.Lifecycle {
	t.Helper()
	job, err := earnings.NewLifecycle(slog.New(slog.DiscardHandler), j.tx, j.ledger, houseReceivable, rules)
	if err != nil {
		t.Fatalf("NewLifecycle(): %v", err)
	}
	return job
}

// runs makes one pass and refuses a run that was not clean.
func (j *theJourney) runs(t *testing.T, job *earnings.Lifecycle) earnings.Outcome {
	t.Helper()
	out, err := job.Run(j.ctx)
	if err != nil {
		t.Fatalf("Run(): %v (did %+v)", err, out)
	}
	return out
}

// restates stores the network's newer word about a transaction: a report
// superseding the given one, carrying everything it carried but the status.
func (j *theJourney) restates(t *testing.T, predecessor uuid.UUID, status networks.Status) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload, supersedes_id)
		select network_id, network_account_id, external_id, click_ref,
		       $2, $2, sale_amount_minor, commission_minor, currency,
		       transacted_at, now(), query_window_start, query_window_end, raw_payload, id
		  from cashback.network_transaction where id = $1
		returning id`, predecessor, string(status)).Scan(&id); err != nil {
		t.Fatalf("restating the report as %s: %v", status, err)
	}
	return id
}

// storedEntry is an entry as the tables hold it after a run.
type storedEntry struct {
	ID         uuid.UUID
	State      string
	HoldRule   string
	Amount     int64
	ReversalOf uuid.UUID
}

// entriesCiting reads every entry resting on a report.
func (j *theJourney) entriesCiting(t *testing.T, report uuid.UUID) []storedEntry {
	t.Helper()
	rows, err := j.tx.Query(j.ctx, `
		select id, state, coalesce(hold_rule, ''), amount_minor, coalesce(reversal_of_id, '00000000-0000-0000-0000-000000000000')
		  from cashback.entry where network_transaction_id = $1 order by created_at, id`, report)
	if err != nil {
		t.Fatalf("reading the entries citing %s: %v", report, err)
	}
	defer rows.Close()
	var out []storedEntry
	for rows.Next() {
		var e storedEntry
		if err := rows.Scan(&e.ID, &e.State, &e.HoldRule, &e.Amount, &e.ReversalOf); err != nil {
			t.Fatalf("scanning an entry: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the entries citing %s: %v", report, err)
	}
	return out
}

// theOneEntryCiting reads the single entry a report earned, failing on any
// other count.
func (j *theJourney) theOneEntryCiting(t *testing.T, report uuid.UUID) storedEntry {
	t.Helper()
	entries := j.entriesCiting(t, report)
	if len(entries) != 1 {
		t.Fatalf("report %s has %d entries, want exactly one: %+v", report, len(entries), entries)
	}
	return entries[0]
}

// wantBalances is the member's three stage accounts at once.
func (j *theJourney) wantBalances(t *testing.T, held, pending, confirmed int64) {
	t.Helper()
	if h, p, c := j.balance(t, wallet.StageHeld), j.balance(t, wallet.StagePending), j.balance(t, wallet.StageConfirmed); h != held || p != pending || c != confirmed {
		t.Errorf("balances held=%d pending=%d confirmed=%d, want %d/%d/%d", h, p, c, held, pending, confirmed)
	}
}

func TestTheJobTurnsAReportIntoACreditConfirmsItAndTakesItBack(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	click := j.clickOut(t)
	// The rate is republished, lower, before the purchase is reported: the
	// job must credit at the click-time share (FR-013), not at this one.
	j.publishANewRate(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)
	job := j.lifecycle(t, earnings.HoldRules{})

	// 1. Reported, so credited: pending, at the click-time share.
	if out := j.runs(t, job); out != (earnings.Outcome{Credited: 1}) {
		t.Fatalf("the first run did %+v, want one credit and nothing else", out)
	}
	opened := j.theOneEntryCiting(t, report)
	if opened.State != string(earnings.StatePending) || opened.Amount != clickTimeMemberShare {
		t.Fatalf("the credit opened as %s for %d, want pending for %d", opened.State, opened.Amount, clickTimeMemberShare)
	}
	j.wantBalances(t, 0, clickTimeMemberShare, 0)
	if _, reason := j.transitionRecorded(t, opened.ID, earnings.StatePending); reason != "the network reported the purchase" {
		t.Errorf("the opening recorded %q as its reason", reason)
	}

	// 2. Nothing changed, so a second run does nothing - and above all
	//    does not credit the same report twice.
	if out := j.runs(t, job); out != (earnings.Outcome{}) {
		t.Fatalf("a second run over the same rows did %+v, want nothing", out)
	}
	j.theOneEntryCiting(t, report)
	j.wantBalances(t, 0, clickTimeMemberShare, 0)

	// 3. The network approves. Its word alone confirms nothing (FR-043):
	//    the entry waits for a statement, and the run says so.
	approved := j.restates(t, report, networks.StatusConfirmed)
	if out := j.runs(t, job); out != (earnings.Outcome{Awaiting: 1}) {
		t.Fatalf("with the network's word and no statement the run did %+v, want one entry awaiting a statement", out)
	}
	j.wantBalances(t, 0, clickTimeMemberShare, 0)

	// 4. A statement covers it, and both halves hold.
	j.covered(t)
	if out := j.runs(t, job); out != (earnings.Outcome{Confirmed: 1}) {
		t.Fatalf("with the statement imported the run did %+v, want one confirmation", out)
	}
	if got := j.theOneEntryCiting(t, report); got.State != string(earnings.StateConfirmed) {
		t.Fatalf("after the statement the entry is %s, want confirmed", got.State)
	}
	j.wantBalances(t, 0, 0, clickTimeMemberShare)
	j.wantZeroSum(t)

	// 5. The network takes it back. A reversing entry, citing the report
	//    that reversed it (C-3), and the money goes back where it came from.
	reversed := j.restates(t, approved, networks.StatusReversed)
	if out := j.runs(t, job); out != (earnings.Outcome{Reversed: 1}) {
		t.Fatalf("with the commission reversed the run did %+v, want one reversal", out)
	}
	undoing := j.theOneEntryCiting(t, reversed)
	if undoing.State != string(earnings.StateReversed) || undoing.ReversalOf != opened.ID || undoing.Amount != clickTimeMemberShare {
		t.Fatalf("the reversing entry is %+v, want reversed, undoing %s for %d", undoing, opened.ID, clickTimeMemberShare)
	}
	if _, reason := j.transitionRecorded(t, undoing.ID, earnings.StateReversed); reason != "the network reversed the commission" {
		t.Errorf("the reversal recorded %q as its reason", reason)
	}
	// The original is untouched: the pair is what makes this auditable.
	if original := j.theOneEntryCiting(t, report); original.State != string(earnings.StateConfirmed) {
		t.Errorf("the original entry was edited to %s; a reversal never edits what it undoes", original.State)
	}
	j.wantBalances(t, 0, 0, 0)
	j.wantZeroSum(t)
	// And not spendable: the credit's row still says confirmed, because a
	// reversal edits nothing (SC-010), and a withdrawal reading it as
	// confirmed money would reserve money the stage no longer holds.
	machine, _ := j.entries(t)
	if spendable, err := machine.Confirmed(j.ctx, earningsstore.New(j.tx), j.member, "EUR"); err != nil || len(spendable) != 0 {
		t.Errorf("a reversed credit is still listed as spendable: %v (err %v)", spendable, err)
	}

	// 6. And once undone, undone: nothing is reversed twice.
	if out := j.runs(t, job); out != (earnings.Outcome{}) {
		t.Fatalf("a run after the reversal did %+v, want nothing", out)
	}
	j.wantBalances(t, 0, 0, 0)
}

func TestTheJobHoldsASelfDealingCreditAtIngestion(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)

	// One device, two accounts, and the rules configured the way the
	// deployment configures them (T118): the credit must open held, with
	// the money in the held stage and the rule on the entry.
	device := clickout.NewContextDigest("198.51.100.7", "Mozilla/5.0 (same phone)")
	j.clickFrom(t, j.anotherMember(t), device)
	click := j.clickFrom(t, j.member, device)
	report := j.reports(t, click.Ref.Ref(), networks.StatusConfirmed)
	j.covered(t)
	job := j.lifecycle(t, earnings.HoldRules{SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour})

	if out := j.runs(t, job); out != (earnings.Outcome{Held: 1}) {
		t.Fatalf("the run did %+v, want one hold and nothing else", out)
	}
	held := j.theOneEntryCiting(t, report)
	if held.State != string(earnings.StateHeld) || held.HoldRule != earnings.RuleSharedContext {
		t.Fatalf("the credit opened as %s under %q, want held under %s", held.State, held.HoldRule, earnings.RuleSharedContext)
	}
	j.wantBalances(t, clickTimeMemberShare, 0, 0)
	if _, reason := j.transitionRecorded(t, held.ID, earnings.StateHeld); reason == "" {
		t.Error("the hold recorded no reason for the operator to read")
	}

	// Approved by the network AND covered by a statement, and still not
	// confirmed: held goes only to pending, and only by a human (T119).
	if out := j.runs(t, job); out != (earnings.Outcome{}) {
		t.Fatalf("a run over a held credit did %+v, want nothing", out)
	}
	j.wantBalances(t, clickTimeMemberShare, 0, 0)
	j.wantZeroSum(t)
}

func TestAReferenceNamingNoClickIsQueuedNotCredited(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	report := j.reports(t, "nobody-minted-"+tag(t), networks.StatusConfirmed)
	job := j.lifecycle(t, earnings.HoldRules{})

	if out := j.runs(t, job); out != (earnings.Outcome{Queued: 1}) {
		t.Fatalf("the run did %+v, want one report queued and nothing credited", out)
	}
	if entries := j.entriesCiting(t, report); len(entries) != 0 {
		t.Fatalf("a report matching no click earned %+v", entries)
	}
	var queued int
	if err := j.tx.QueryRow(j.ctx, `
		select count(*) from cashback.unattributed_transaction where network_transaction_id = $1`, report).Scan(&queued); err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	if queued != 1 {
		t.Fatalf("the report is in the unattributed queue %d times, want once", queued)
	}
	// Queued is decided: the next run neither re-queues nor re-asks.
	if out := j.runs(t, job); out != (earnings.Outcome{}) {
		t.Fatalf("a second run did %+v, want nothing", out)
	}
	j.wantBalances(t, 0, 0, 0)
}

// TestAReportDeclinedBeforeCreditMovesNoMoney. Crediting it only to reverse
// it would move money twice to say nothing.
func TestAReportDeclinedBeforeCreditMovesNoMoney(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	click := j.clickOut(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)
	declined := j.restates(t, report, networks.StatusDeclined)
	job := j.lifecycle(t, earnings.HoldRules{})

	if out := j.runs(t, job); out != (earnings.Outcome{}) {
		t.Fatalf("the run did %+v over a declined transaction, want nothing", out)
	}
	if entries := append(j.entriesCiting(t, report), j.entriesCiting(t, declined)...); len(entries) != 0 {
		t.Fatalf("a transaction declined before credit earned %+v", entries)
	}
	j.wantBalances(t, 0, 0, 0)
}

// TestTheJobRunsUnderItsOwnNameOnTheScheduler. The name is what the
// fleet-wide lock is taken on, so a second registration under it is refused
// rather than giving two jobs one lock.
func TestTheJobRunsUnderItsOwnNameOnTheScheduler(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	click := j.clickOut(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)

	jobs := scheduler.New(slog.New(slog.DiscardHandler), lifecycleTestLocker{}, scheduler.Config{})
	if err := j.lifecycle(t, earnings.HoldRules{}).Register(jobs); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if err := j.lifecycle(t, earnings.HoldRules{}).Register(jobs); err == nil {
		t.Error("a second job registered under the same name, so two would share one lock")
	}
	ran, err := jobs.RunOnce(j.ctx, earnings.LifecycleJobName)
	if err != nil || !ran {
		t.Fatalf("the job ran=%t, err=%v", ran, err)
	}
	if got := j.theOneEntryCiting(t, report); got.State != string(earnings.StatePending) {
		t.Errorf("driven by the scheduler the credit is %s, want pending", got.State)
	}
}
