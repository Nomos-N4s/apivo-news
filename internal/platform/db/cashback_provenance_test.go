package db_test

// C-7 and C-1, the two questions an auditor asks.
//
//	C-7  for any member payout, ONE query returns the full chain - payout,
//	     approver, ledger postings, cashback entries, network transaction
//	     evidence, click, and the offer rate at click time
//	C-1  every ledger currency nets to zero
//
// C-7 is asserted the way the news suite asserts I-5: build a real chain,
// then answer the whole audit with a single SELECT and check every link is
// present in the row that comes back. The timing is logged, because the
// invariant is that the audit is demonstrably fast, not asserted fast.
//
// C-1 is the invariant that lives outside this schema (ADR-0002, the named
// Principle VIII exception). What can be asserted here is that the check
// exists, is a real query over real rows, and reports no non-zero currency.

import (
	"context"
	"testing"
	"time"
)

// TestCashbackProvenanceAnswersTheWholeChain asserts C-7: one query, every
// link, no assembly by the caller.
func TestCashbackProvenanceAnswersTheWholeChain(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	// The reservation transition is the seam the view joins on (D9): the
	// entries a payout pays are the entries whose reservation transfer is
	// the request's own.
	var reservationID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
		 values ($1, 'confirmed', 'reserved', $2) returning id`,
		f.entryID, "reserve-"+f.suffix).Scan(&reservationID); err != nil {
		t.Fatalf("reserving the entry: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
		 values ($1, $2, $3)`, reservationID, f.entryID, "reserve-"+f.suffix); err != nil {
		t.Fatalf("linking the reservation posting: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update cashback.entry set state = 'reserved' where id = $1`, f.entryID); err != nil {
		t.Fatalf("moving the entry to reserved: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update cashback.withdrawal_request
		    set state = 'approved', decided_by = $2, decided_at = now(), decision_reason = 'balance confirmed'
		  where id = $1`, f.requestID, f.approverID); err != nil {
		t.Fatalf("approving the request: %v", err)
	}

	var payoutID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail, rail_reference, state, settled_at)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual', 'bank-ref-1', 'settled', now()) returning id`,
		f.requestID, f.approverID).Scan(&payoutID); err != nil {
		t.Fatalf("paying the request: %v", err)
	}

	var (
		approverName, approverEmail   string
		memberID, entryID, evidenceID string
		clickRef, networkStatus       string
		rateSnapshot                  []byte
		shareBps                      int
		commissionMinor               int64
		merchantSlug                  string
		transferRef, idempotencyKey   string
		ledgerPostedAt                time.Time
	)

	start := time.Now()
	err := tx.QueryRow(ctx,
		`select approver_name, approver_email, member_id, entry_id, network_transaction_id,
		        click_ref, network_status, rate_snapshot, member_share_bps_snapshot,
		        commission_minor, merchant_slug, ledger_transfer_ref, idempotency_key, ledger_posted_at
		   from cashback.provenance
		  where payout_id = $1`, payoutID).Scan(
		&approverName, &approverEmail, &memberID, &entryID, &evidenceID,
		&clickRef, &networkStatus, &rateSnapshot, &shareBps,
		&commissionMinor, &merchantSlug, &transferRef, &idempotencyKey, &ledgerPostedAt)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the C-7 audit query failed: %v", err)
	}
	t.Logf("C-7 audit: full payout chain in one query in %s", elapsed)

	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "approver", got: approverName, want: "Test Approver " + f.suffix},
		{name: "member", got: memberID, want: f.accountID},
		{name: "entry", got: entryID, want: f.entryID},
		{name: "network evidence", got: evidenceID, want: f.networkTxn},
		{name: "click", got: clickRef, want: f.clickRef},
		{name: "network status", got: networkStatus, want: "confirmed"},
		{name: "merchant", got: merchantSlug, want: "merchant-" + f.suffix},
		{name: "ledger transfer", got: transferRef, want: "reserve-" + f.suffix},
		{name: "idempotency key", got: idempotencyKey, want: "payout:" + f.requestID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("provenance %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if approverEmail == "" {
		t.Error("provenance names an approver with no email: the approver is not contactable from the audit")
	}
	if len(rateSnapshot) == 0 {
		t.Error("provenance carries no click-time rate snapshot: the rate that governed the credit is not in the answer (FR-013)")
	}
	if shareBps != 5000 {
		t.Errorf("provenance member share is %d bps, want the click-time snapshot of 5000", shareBps)
	}
	if commissionMinor != 500 {
		t.Errorf("provenance commission is %d minor units, want 500 as the network reported it", commissionMinor)
	}
	if ledgerPostedAt.IsZero() {
		t.Error("provenance carries no ledger posting time: the money movement is not in the answer")
	}
}

// TestCashbackProvenanceCoversAnOperatorAttributedEntry asserts the one
// place the chain is legitimately shorter: an entry an operator attributed
// by hand has no click, and the audit must still answer rather than drop
// the row. The evidence is not optional in the same way - C-2 makes it
// mandatory - so an inner join is correct there and a left join here.
func TestCashbackProvenanceCoversAnOperatorAttributedEntry(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	// A second report with no click reference at all, credited by hand.
	var orphanReport string
	if err := tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload)
		 values ($1, $2, $3, 'approved', 'confirmed', 5000, 250, 'EUR', now(),
		         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
		f.networkID, f.networkAccountID, "orphan-"+f.suffix).Scan(&orphanReport); err != nil {
		t.Fatalf("storing the unattributed report: %v", err)
	}

	var manualEntry string
	if err := tx.QueryRow(ctx,
		`insert into cashback.entry (brand_id, account_id, network_transaction_id, state, amount_minor, currency)
		 values ('fixture', $1, $2, 'reserved', 125, 'EUR') returning id`,
		f.accountID, orphanReport).Scan(&manualEntry); err != nil {
		t.Fatalf("crediting the unattributed report by hand: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref, actor_id)
		 values ($1, 'confirmed', 'reserved', $2, $3)`,
		manualEntry, "reserve-"+f.suffix, f.approverID); err != nil {
		t.Fatalf("reserving the manual entry: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update cashback.withdrawal_request
		    set state = 'approved', decided_by = $2, decided_at = now()
		  where id = $1`, f.requestID, f.approverID); err != nil {
		t.Fatalf("approving the request: %v", err)
	}

	var payoutID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual') returning id`,
		f.requestID, f.approverID).Scan(&payoutID); err != nil {
		t.Fatalf("paying the request: %v", err)
	}

	var rows, withoutClick int
	if err := tx.QueryRow(ctx,
		`select count(*), count(*) filter (where click_id is null)
		   from cashback.provenance where payout_id = $1`, payoutID).Scan(&rows, &withoutClick); err != nil {
		t.Fatalf("the C-7 audit query failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d provenance rows for the payout, want 1", rows)
	}
	if withoutClick != 1 {
		t.Fatal("the operator-attributed entry was dropped from the audit: a missing click must shorten the chain, not hide it")
	}
}

// TestLedgerZeroSumCanActuallySeeAnImbalance is the test that stops the
// C-1 check being decorative. Asserting "no currency is out of balance"
// against a database with no ledger in it proves nothing at all - it is
// the same shape as a privilege test that checks a role is mentioned, or a
// lint whose reader is blind. So this builds a ledger the check can see,
// puts a real imbalance in it, and requires the check to find it.
//
// The second case is the failure the check must NOT swallow: a ledger
// schema that is present but whose postings cannot be read. An earlier
// version of ledger_balance_relation() returned null there, exactly as it
// does for "no ledger at all", so the view came back empty and C-1 passed
// while seeing nothing - with a comment two lines above promising it would
// raise. Both cases run in a rolled-back transaction, so no ledger schema
// survives the test.
//
// Deliberately not parallel: the schema name blnk is fixed by the ledger,
// so two of these running at once would collide on creating it.
func TestLedgerZeroSumCanActuallySeeAnImbalance(t *testing.T) {
	t.Run("an out-of-balance currency is reported", func(t *testing.T) {
		tx := beginTx(t)
		ctx := context.Background()

		// A stand-in for the ledger's own balance rows, with the columns
		// ledger_net_minor() reads. EUR nets to zero; GBP does not.
		for _, stmt := range []string{
			`create schema blnk`,
			`create table blnk.balances (balance bigint not null, currency text not null)`,
			`insert into blnk.balances (balance, currency) values (100, 'EUR'), (-100, 'EUR'), (50, 'GBP')`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("building the stand-in ledger (%s): %v", stmt, err)
			}
		}

		balances := map[string]int64{}
		rows, err := tx.Query(ctx, `select currency, net_minor from cashback.ledger_zero_sum`)
		if err != nil {
			t.Fatalf("the C-1 check failed to run against a readable ledger: %v", err)
		}
		for rows.Next() {
			var currency string
			var net int64
			if err := rows.Scan(&currency, &net); err != nil {
				rows.Close()
				t.Fatalf("scanning: %v", err)
			}
			balances[currency] = net
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating: %v", err)
		}

		if len(balances) != 2 {
			t.Fatalf("the check saw %d currencies, want 2: it is not reading the ledger's own rows", len(balances))
		}
		if balances["EUR"] != 0 {
			t.Errorf("EUR nets to %d, want 0: the check miscounts a balanced currency", balances["EUR"])
		}
		if balances["GBP"] != 50 {
			t.Fatalf("GBP nets to %d, want 50: the check cannot see an imbalance, so asserting it finds none proves nothing", balances["GBP"])
		}

		// And the assertion the continuous check actually makes must now
		// fail, which is the whole point.
		var broken int
		if err := tx.QueryRow(ctx,
			`select count(*) from cashback.ledger_zero_sum where net_minor <> 0`).Scan(&broken); err != nil {
			t.Fatalf("running the C-1 assertion: %v", err)
		}
		if broken != 1 {
			t.Fatalf("the C-1 assertion found %d out-of-balance currencies against a deliberately broken ledger, want 1", broken)
		}
	})

	t.Run("a ledger present but unreadable fails loudly", func(t *testing.T) {
		tx := beginTx(t)
		ctx := context.Background()

		// The ledger is installed - the schema is there - but its balances
		// are not readable. Reporting zero rows here would be a check
		// passing on nothing.
		if _, err := tx.Exec(ctx, `create schema blnk`); err != nil {
			t.Fatalf("creating the ledger schema: %v", err)
		}
		_, err := tx.Exec(ctx, `select count(*) from cashback.ledger_zero_sum`)
		wantPgCode(t, err, codeRaiseException)
	})
}

// TestLedgerZeroSumReportsNoNonZeroCurrency asserts C-1 as far as this
// schema can: the check exists, it is a real query, and nothing it can see
// is out of balance.
//
// Where the ledger is not co-located (CI without the sidecar, or the
// ADR-0002 fallback where it has its own database) the view is empty and
// the assertion is vacuously true - which is honest, because there are no
// postings here to disagree about. What must never happen is a row with a
// non-zero net, in any deployment shape.
func TestLedgerZeroSumReportsNoNonZeroCurrency(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	rows, err := tx.Query(ctx, `select currency, net_minor from cashback.ledger_zero_sum where net_minor <> 0`)
	if err != nil {
		t.Fatalf("the C-1 zero-sum check failed to run: %v", err)
	}
	defer rows.Close()

	var broken []string
	for rows.Next() {
		var currency string
		var net int64
		if err := rows.Scan(&currency, &net); err != nil {
			t.Fatalf("scanning the zero-sum check: %v", err)
		}
		broken = append(broken, currency)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating the zero-sum check: %v", err)
	}
	if len(broken) > 0 {
		t.Fatalf("C-1 violated: the ledger does not net to zero in %v - money was created or destroyed inside it", broken)
	}

	// The check must be a query over the ledger's own rows, not a constant.
	// Where the ledger is absent that is visible as an empty currency list,
	// and the test says so rather than silently passing on nothing.
	var currencies int
	if err := tx.QueryRow(ctx, `select cardinality(cashback.ledger_currencies())`).Scan(&currencies); err != nil {
		t.Fatalf("reading the ledger's currencies: %v", err)
	}
	if currencies == 0 {
		t.Log("C-1: the ledger is not co-located in this database; the zero-sum check is vacuously true here and runs as a periodic job against wherever the ledger lives (ADR-0002)")
	}
}
