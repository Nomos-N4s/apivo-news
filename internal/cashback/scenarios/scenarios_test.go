package scenarios_test

// TestScenario is the quickstart's acceptance gates, one subtest per
// scenario, named as the Makefile names them:
//
//	make cashback-scenario NAME=earn-confirm
//
// One test function rather than six, because `-run 'TestScenario/NAME'` is
// what the Makefile target already passes and because the list of names IS
// the quickstart's list - a scenario added there without a case here should
// be a missing subtest, not a missing file nobody notices.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	networksstore "github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	opspkg "github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

func TestScenario(t *testing.T) {
	t.Run("earn-confirm", earnConfirm)
	t.Run("evidence-immutable", evidenceImmutable)
	t.Run("reversal", reversal)
	t.Run("unattributed-and-held", unattributedAndHeld)
	t.Run("reconciliation", reconciliation)
}

// earnConfirm is V1 (US1, US3 · SC-001, SC-002, SC-006).
//
// Click-out → the network reports pending → the entry opens → the statement
// arrives → the network approves → the entry confirms. The published rate is
// changed in the middle, because FR-013 says the rate AT THE CLICK governs
// the credit and a scenario that changed nothing would pass either way.
func earnConfirm(t *testing.T) {
	w := begin(t).seed(t)

	// The click carries an unguessable reference and the band as published
	// at this moment (FR-013, FR-020).
	click := w.clickOut(t)
	if len(click.Ref.Ref()) < 22 {
		t.Errorf("the click reference is %d characters; FR-020 needs at least 22 for 128 bits of entropy", len(click.Ref.Ref()))
	}
	if click.Promised.Rate.Percent != clickTimeRateBps || click.Promised.MemberShare != clickTimeShareBps {
		t.Fatalf("the click snapshotted %d bps at a %d bps share, want %d at %d",
			click.Promised.Rate.Percent, click.Promised.MemberShare, clickTimeRateBps, clickTimeShareBps)
	}
	// It is a committed row before any redirect is returned, so a member who
	// buys on a redirect we issued always has evidence behind them.
	var stored string
	if err := w.tx.QueryRow(w.ctx, `select click_ref from cashback.click where id = $1`, click.ID).Scan(&stored); err != nil {
		t.Fatalf("the click was not recorded before the redirect: %v", err)
	}
	if stored != click.Ref.Ref() {
		t.Errorf("the stored reference is %q and the redirect carried %q", stored, click.Ref.Ref())
	}

	// The catalogue republishes at a fifth of the rate, before the purchase
	// is reported.
	w.republish(t)

	report := w.reports(t, click.Ref.Ref())
	attributed := w.match(t, report, click.Ref.Ref())
	if !attributed.Matched || attributed.Click.ID != click.ID {
		t.Fatalf("the report matched %v (matched=%v), want the click %s",
			attributed.Click.ID, attributed.Matched, click.ID)
	}

	// The share comes from the snapshot the matched click carries, not from
	// the catalogue as it now stands.
	share := w.shareOf(t, attributed.Click)
	if share.Minor != memberShareMinor {
		t.Fatalf("the member's share is %d, want %d (the click-time band); %d would be the band published afterwards",
			share.Minor, memberShareMinor, laterShareMinor)
	}

	machine, confirmations := w.machines(t)
	opened, err := machine.Open(w.ctx, w.tx, earnings.Credit{
		Member: w.member,
		Brand:  scenarioBrand,
		Report: report.id,
		Click:  click.ID,
		State:  earnings.StatePending,
		Amount: share,
		Reason: "the network reported the purchase",
	})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	// Wallet Pending, equal to an independently computed ledger sum
	// (SC-006).
	if got := w.balance(t, wallet.StagePending); got != memberShareMinor {
		t.Errorf("pending balance is %d, want %d", got, memberShareMinor)
	}
	if got := w.balance(t, wallet.StageConfirmed); got != 0 {
		t.Errorf("confirmed balance is %d before the network approved, want 0", got)
	}
	w.wantZeroSum(t)
	w.wantNoOrphanCredits(t)

	// The statement arrives, and only then may the credit confirm (FR-043).
	w.importsAStatement(t)
	confirmed, err := confirmations.Confirm(w.ctx, w.tx, opened, networks.StatusConfirmed, report.id)
	if err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	if confirmed.State != earnings.StateConfirmed {
		t.Fatalf("the entry is %s, want confirmed", confirmed.State)
	}

	// The member's total did not change. Only which bucket counts toward the
	// withdrawal threshold did (FR-050).
	if got := w.balance(t, wallet.StagePending); got != 0 {
		t.Errorf("pending balance is %d after confirmation, want 0", got)
	}
	if got := w.balance(t, wallet.StageConfirmed); got != memberShareMinor {
		t.Errorf("confirmed balance is %d, want %d", got, memberShareMinor)
	}
	w.wantZeroSum(t)
	w.wantNoOrphanCredits(t)

	// And the stream says what the tables say (T076).
	announced := w.eventsAbout(t, opened.ID)
	if len(announced[earnings.TypeEntryCreated]) != 1 {
		t.Errorf("announced %s %d times, want once", earnings.TypeEntryCreated, len(announced[earnings.TypeEntryCreated]))
	}
	moves := announced[earnings.TypeEntryStateChanged]
	if len(moves) != 2 {
		t.Fatalf("announced %s %d times, want twice (the opening and the confirmation)",
			earnings.TypeEntryStateChanged, len(moves))
	}
	for _, move := range moves {
		if ref, named := move["ledger_transfer_ref"].(string); !named || ref == "" {
			t.Errorf("a move to %v names no transfer, so nothing can follow the money from the stream", move["to"])
		}
	}
	if len(w.eventsAbout(t, click.ID)[clickout.TypeClickCreated]) != 1 {
		t.Error("the click that started this was not announced about itself")
	}
}

// evidenceImmutable is V2 (US2 · C-3).
//
// Three separate promises about the same table, and each one fails
// differently: a re-poll must not duplicate, a status change must supersede
// rather than edit, and the database itself must refuse an UPDATE or a
// DELETE — because "we never edit evidence" is only true if editing is
// impossible.
func evidenceImmutable(t *testing.T) {
	w := begin(t).seed(t)
	click := w.clickOut(t)

	first := w.reportsAs(t, "SCEN-DUP-"+suffix(t), click.Ref.Ref(), networks.StatusPending, reportedCommission)

	// 1. Re-polling the same window creates no duplicate row. The rule is a
	//    unique key on (network, external id, status), so the second insert
	//    of the same fact is refused rather than accepted and deduplicated
	//    later.
	if _, err := w.tx.Exec(w.ctx, `savepoint repoll`); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err := w.tx.Exec(w.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload)
		select network_id, network_account_id, external_id, click_ref,
		       status_raw, status, sale_amount_minor, commission_minor, currency,
		       transacted_at, now(), query_window_start, query_window_end, raw_payload
		  from cashback.network_transaction where id = $1`, first.id)
	if err == nil {
		t.Error("re-polling the same window inserted the transaction a second time; a duplicate report is a duplicate credit waiting to happen")
	}
	if _, err := w.tx.Exec(w.ctx, `rollback to savepoint repoll`); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}

	// 2. A changed status is a NEW row naming the one it supersedes, and
	//    both stay readable in order (C-3).
	var second string
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload, supersedes_id)
		select network_id, network_account_id, external_id, click_ref,
		       'approved', 'confirmed', sale_amount_minor, commission_minor, currency,
		       transacted_at, now(), query_window_start, query_window_end, raw_payload, id
		  from cashback.network_transaction where id = $1
		returning id::text`, first.id).Scan(&second); err != nil {
		t.Fatalf("superseding the report: %v", err)
	}
	var both int
	if err := w.tx.QueryRow(w.ctx, `
		select count(*) from cashback.network_transaction
		 where id = $1 or supersedes_id = $1`, first.id).Scan(&both); err != nil {
		t.Fatalf("counting the chain: %v", err)
	}
	if both != 2 {
		t.Errorf("the supersede chain holds %d row(s), want both the original and its successor", both)
	}

	// 3. And the database refuses to edit either of them. This is the one
	//    that makes the other two mean something: without it, "we never
	//    edit" is a convention rather than a property.
	for _, attempt := range []struct{ name, sql string }{
		{"an UPDATE", `update cashback.network_transaction set commission_minor = 1 where id = $1`},
		{"a DELETE", `delete from cashback.network_transaction where id = $1`},
	} {
		if _, err := w.tx.Exec(w.ctx, `savepoint immutable`); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err := w.tx.Exec(w.ctx, attempt.sql, first.id)
		if err == nil {
			t.Errorf("%s against stored evidence succeeded; C-3 says evidence is append-only", attempt.name)
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Errorf("%s was refused by something other than the database: %v", attempt.name, err)
			} else if pgErr.Code != "23514" && pgErr.Code != "P0001" {
				t.Errorf("%s raised SQLSTATE %s (%s); want the immutability guard", attempt.name, pgErr.Code, pgErr.Message)
			}
		}
		if _, err := w.tx.Exec(w.ctx, `rollback to savepoint immutable`); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	}
}

// reversal is V3 (US1 scenario 4 · SC-010).
//
// The network takes a commission back. The promise is that nothing about the
// original credit changes: a reversing entry is written beside it, the money
// comes out of the stage account it actually reached, and both entries stay
// in the member's wallet. "We never rewrite history" is only worth saying if
// the original row is byte-identical afterwards, so that is what is compared.
func reversal(t *testing.T) {
	w := begin(t).seed(t)
	click := w.clickOut(t)

	report := w.reports(t, click.Ref.Ref())
	share := w.shareOf(t, click)
	machine, confirmations := w.machines(t)
	opened, err := machine.Open(w.ctx, w.tx, earnings.Credit{
		Member: w.member, Brand: scenarioBrand, Report: report.id, Click: click.ID,
		State: earnings.StatePending, Amount: share, Reason: "the network reported the purchase",
	})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	w.importsAStatement(t)
	confirmed, err := confirmations.Confirm(w.ctx, w.tx, opened, networks.StatusConfirmed, report.id)
	if err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	if got := w.balance(t, wallet.StageConfirmed); got != memberShareMinor {
		t.Fatalf("confirmed balance is %d before the clawback, want %d", got, memberShareMinor)
	}

	// The original, as it stands before anything is undone. Read whole, so
	// the comparison afterwards is of every column rather than of the ones
	// somebody remembered to name.
	before := w.entryRow(t, confirmed.ID)

	// C-3: the network taking it back is a NEW report superseding the one
	// that said it was approved, never an edit to it.
	clawback := w.supersedes(t, report, "reversed", networks.StatusReversed)

	reversals, err := earnings.NewReversals(machine)
	if err != nil {
		t.Fatalf("NewReversals(): %v", err)
	}
	reversing, err := reversals.Reverse(w.ctx, w.tx, earnings.Reversal{
		Original: confirmed,
		Report:   clawback.id,
		Reason:   "the network reversed the commission",
	})
	if err != nil {
		t.Fatalf("Reverse(): %v", err)
	}

	// A reversing entry, and it says so rather than masquerading as a credit
	// in some other state (SC-010).
	if reversing.State != earnings.StateReversed {
		t.Errorf("the reversing entry is %s, want reversed", reversing.State)
	}
	if reversing.ID == confirmed.ID {
		t.Fatal("the reversal reused the original entry's row; a reversal inserts, it never edits")
	}

	// The original is untouched, column for column.
	if after := w.entryRow(t, confirmed.ID); after != before {
		t.Errorf("the original entry changed under the reversal:\n before %+v\n  after %+v", before, after)
	}

	// The money came out of the account it went into. Getting this wrong
	// leaves the confirmed balance standing while some other bucket goes
	// negative, which the zero-sum check would not catch on its own.
	if got := w.balance(t, wallet.StageConfirmed); got != 0 {
		t.Errorf("confirmed balance is %d after the clawback, want 0", got)
	}
	if got := w.balance(t, wallet.StagePending); got != 0 {
		t.Errorf("pending balance is %d after the clawback, want 0; the money must leave the stage it reached", got)
	}
	w.wantZeroSum(t)
	w.wantNoOrphanCredits(t)

	// And both are in the wallet: the credit that happened, and the reversal
	// that undid it. A member who saw the money must be able to see why it
	// went.
	var entries int
	if err := w.tx.QueryRow(w.ctx, `
		select count(*) from cashback.entry where account_id = $1`, w.member).Scan(&entries); err != nil {
		t.Fatalf("counting the member's entries: %v", err)
	}
	if entries != 2 {
		t.Errorf("the member holds %d entr(ies), want 2: the credit and its reversal, both readable", entries)
	}
}

// unattributedAndHeld is V5 (US1 scenario 5, US7 · FR-034, FR-060).
//
// Two ways a purchase does not simply become money, and one operator action
// on each. A reference nobody minted credits nobody and is queued for a
// human; a credit a rule distrusts is held, naming the rule, and released or
// rejected only with a reason.
func unattributedAndHeld(t *testing.T) {
	w := begin(t).seed(t)

	// A reference the network echoed that names no click of ours. Not
	// "missing" - present, and wrong, which is the case a bare null check
	// would let through.
	stranger := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + suffix(t)
	orphan := w.reports(t, stranger)
	attributed := w.match(t, orphan, stranger)
	if attributed.Matched {
		t.Fatal("a reference naming no click was attributed to one")
	}
	if attributed.Queued == uuid.Nil {
		t.Fatal("nothing was queued, so the report is money in no queue at all")
	}

	// It is readable as work, through the production read rather than a
	// query written for this test.
	queue, err := networks.NewUnattributedQueue(networksstore.New(w.tx))
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	open, err := queue.OpenByID(w.ctx, attributed.Queued)
	if err != nil {
		t.Fatalf("OpenByID(): %v", err)
	}
	if open.Report != orphan.id {
		t.Errorf("the queued row cites report %s, want %s", open.Report, orphan.id)
	}

	// And it credited nobody.
	if w.entriesFor(t, w.member) != 0 {
		t.Error("a report matching no click opened an entry; FR-034 says it goes to a human, not to a balance")
	}
	if got := w.balance(t, wallet.StagePending); got != 0 {
		t.Errorf("pending balance is %d after an unattributable report, want 0", got)
	}

	// Now the held half: a genuine purchase, above a cap the deployment set.
	click := w.clickOut(t)
	report := w.reports(t, click.Ref.Ref())
	matched := w.match(t, report, click.Ref.Ref())
	if !matched.Matched {
		t.Fatalf("the second report did not match its own click")
	}

	saleCap, err := money.New(reportedSaleMinor-1, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	holds, err := earnings.NewHolds(earnings.HoldRules{SaleCap: saleCap}, earningsstore.New(w.tx))
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	sale, err := money.New(reportedSaleMinor, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	hold, err := holds.Evaluate(w.ctx, earnings.Candidate{
		Member: w.member, Click: matched.Click, Sale: sale, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if !hold.Held() {
		t.Fatalf("a sale of %d was not held under a cap of %d", reportedSaleMinor, reportedSaleMinor-1)
	}

	machine, _ := w.machines(t)
	opened, err := machine.Open(w.ctx, w.tx, hold.Open(earnings.Credit{
		Member: w.member, Brand: scenarioBrand, Report: report.id, Click: click.ID,
		State: earnings.StatePending, Amount: w.shareOf(t, click),
		Reason: "the network reported the purchase",
	}))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if opened.State != earnings.StateHeld {
		t.Fatalf("the entry is %s, want held", opened.State)
	}
	// The rule is named on the row. An operator who cannot see WHY it was
	// held has to guess, and a guess about somebody else's money is the
	// thing this queue exists to prevent.
	if got := w.entryRow(t, opened.ID).holdRule; got == "" {
		t.Error("the held entry names no rule")
	}

	reviews, err := earnings.NewReviews(w.tx, w.ledger, houseReceivable)
	if err != nil {
		t.Fatalf("NewReviews(): %v", err)
	}

	// A review with no reason is refused before anything is read (FR-061).
	if _, err := reviews.Release(w.ctx, earnings.Review{
		Entry: opened.ID, Operator: w.operator, Reason: "   ",
	}); !errors.Is(err, earnings.ErrInvalidReview) {
		t.Errorf("Release() with a blank reason = %v, want one wrapping %v", err, earnings.ErrInvalidReview)
	}

	released, err := reviews.Release(w.ctx, earnings.Review{
		Entry: opened.ID, Operator: w.operator, Reason: "checked against the retailer's order",
	})
	if err != nil {
		t.Fatalf("Release(): %v", err)
	}
	if released.Entry.State == earnings.StateHeld {
		t.Error("the entry is still held after being released")
	}
	// Releasing clears the rule: US7's queue is "every entry whose hold_rule
	// is set", and that has to be the same set as "every held entry".
	if got := w.entryRow(t, opened.ID).holdRule; got != "" {
		t.Errorf("the released entry still names hold rule %q", got)
	}

	// And the decision is on the stream, with its reason and its operator,
	// so an auditor reading events sees what the operator saw.
	w.wantDecisionAnnounced(t, opened.ID, earnings.TypeHoldReleased, "released_by", "checked against the retailer's order")

	// The other operator action on the same queue: rejecting. A second held
	// credit, because the first one has been released and a released credit
	// is not a held one.
	rejected := w.holdAnother(t, holds, machine)
	if _, err := reviews.Reject(w.ctx, earnings.Review{
		Entry: rejected, Operator: w.operator, Reason: "\t\n ",
	}); !errors.Is(err, earnings.ErrInvalidReview) {
		t.Errorf("Reject() with a blank reason = %v, want one wrapping %v", err, earnings.ErrInvalidReview)
	}
	if _, err := reviews.Reject(w.ctx, earnings.Review{
		Entry: rejected, Operator: w.operator, Reason: "the retailer has no such order",
	}); err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	w.wantDecisionAnnounced(t, rejected, earnings.TypeHoldRejected, "rejected_by", "the retailer has no such order")

	// A rejection undoes the credit rather than editing it, so the member is
	// left holding nothing from either purchase.
	if got := w.balance(t, wallet.StagePending); got != memberShareMinor {
		t.Errorf("pending balance is %d, want %d: the released credit stands and the rejected one does not",
			got, memberShareMinor)
	}
	w.wantZeroSum(t)
	w.wantNoOrphanCredits(t)
}

// reconciliation is V6 (US6).
//
// The network's word and the network's money are different facts, and this
// is where they are compared. An approved purchase the statement never paid,
// and one it paid short: both must be listed with their deltas, neither may
// quietly change a member's confirmed balance, and resolving either must
// record who decided and why.
func reconciliation(t *testing.T) {
	w := begin(t).seed(t)

	// The statement first, naming the transaction ids the purchases will
	// carry: one paid 100 minor units short, and one it never mentions.
	// A run covers a period, and one period admits one run, so the statement
	// cannot be written after the purchases it is about.
	shortedID, missingID := "SCEN-SHORT-"+suffix(t), "SCEN-MISSING-"+suffix(t)
	run := w.importsStatement(t, fmt.Sprintf(
		`{"lines":[{"transaction_id":%q,"paid":{"minor":%d,"currency":"EUR"}}]}`,
		shortedID, reportedCommission-100))

	shorted := w.confirmedPurchase(t, shortedID)
	missing := w.confirmedPurchase(t, missingID)
	before := w.balance(t, wallet.StageConfirmed)

	ops, err := opspkg.NewPGStore(w.tx)
	if err != nil {
		t.Fatalf("NewPGStore(): %v", err)
	}
	detected, err := ops.DetectDifferences(w.ctx, run)
	if err != nil {
		t.Fatalf("DetectDifferences(): %v", err)
	}

	// Both, and each named for what it is: an operator chasing a short
	// payment and one chasing a missing one do different things.
	kinds := map[opspkg.DifferenceKind]opspkg.Difference{}
	for _, d := range detected.Found {
		kinds[d.Kind] = d
	}
	short, sawShort := kinds[opspkg.AmountMismatch]
	if !sawShort {
		t.Fatalf("a line paying %d against a report owed %d raised no amount mismatch; found %+v",
			reportedCommission-100, reportedCommission, detected.Found)
	}
	if short.TransactionID != shorted.external {
		t.Errorf("the mismatch is about %q, want %q", short.TransactionID, shorted.external)
	}
	delta, err := short.Delta()
	if err != nil {
		t.Fatalf("Delta(): %v", err)
	}
	if delta.Minor != -100 {
		t.Errorf("the shorted line's delta is %d, want -100: negative is money missing", delta.Minor)
	}
	unpaid, sawUnpaid := kinds[opspkg.ReportedNotPaid]
	if !sawUnpaid {
		t.Fatalf("a confirmed report the statement never mentions raised no difference; found %+v", detected.Found)
	}
	if unpaid.TransactionID != missing.external {
		t.Errorf("the unpaid difference is about %q, want %q", unpaid.TransactionID, missing.external)
	}

	// Neither touched the member's money. Detection derives and records; it
	// does not decide, and a balance that moved because a statement was
	// short would be money taken back without anybody saying so.
	if after := w.balance(t, wallet.StageConfirmed); after != before {
		t.Errorf("the confirmed balance moved from %d to %d during detection; reconciliation reports, it does not adjust",
			before, after)
	}

	// The queue reads back through the production listing.
	listed, err := ops.ListDifferences(w.ctx, run, opspkg.DifferenceAfter{}, 10)
	if err != nil {
		t.Fatalf("ListDifferences(): %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("the run lists %d difference(s), want 2", len(listed))
	}

	// Resolving records the named human and their reason (FR-061). A blank
	// one is refused before anything is written.
	if _, err := ops.ResolveDifference(w.ctx, opspkg.Resolution{
		ID: listed[0].ID, Verdict: opspkg.VerdictExplained, Reason: " ",
		Operator: opspkg.Operator{ID: w.operator},
	}); err == nil {
		t.Error("a resolution with a blank reason was accepted; FR-061 says a decision records why")
	}
	resolved, err := ops.ResolveDifference(w.ctx, opspkg.Resolution{
		ID: listed[0].ID, Verdict: opspkg.VerdictExplained,
		Reason: "the network confirmed the shortfall by email", Operator: opspkg.Operator{ID: w.operator},
	})
	if err != nil {
		t.Fatalf("ResolveDifference(): %v", err)
	}
	if resolved.ResolvedBy != w.operator {
		t.Errorf("the difference records %s as resolver, want the operator %s", resolved.ResolvedBy, w.operator)
	}
	w.wantZeroSum(t)
}
