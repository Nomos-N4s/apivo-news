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
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

func TestScenario(t *testing.T) {
	t.Run("earn-confirm", earnConfirm)
	t.Run("evidence-immutable", evidenceImmutable)
	t.Run("reversal", reversal)
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

	report := w.reports(t, click.Ref.Ref(), networks.StatusPending)
	attributed := w.match(t, report, click.Ref.Ref())
	if !attributed.Matched || attributed.Click.ID != click.ID {
		t.Fatalf("the report matched %v (matched=%v), want the click %s",
			attributed.Click.ID, attributed.Matched, click.ID)
	}

	// The share comes from the snapshot the matched click carries, not from
	// the catalogue as it now stands.
	share := w.shareOf(t, attributed.Click, reportedCommission)
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

	report := w.reports(t, click.Ref.Ref(), networks.StatusPending)
	share := w.shareOf(t, click, reportedCommission)
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
