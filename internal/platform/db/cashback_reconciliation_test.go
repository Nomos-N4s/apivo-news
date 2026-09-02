package db_test

// C-3 names imported statements alongside network reports and click
// records. These tests hold the statement to the same standard source_item
// is held to: it is the counterparty's own account of the money, and an
// editable one is worth nothing in a dispute.
//
// The difference rows are deliberately the other way round - they exist to
// be resolved - so what is asserted there is that a resolution is who,
// when and why together, and that a difference always states the figures
// that make it one.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// seedReconciliationRun imports one statement covering the fixture's
// publisher account.
func seedReconciliationRun(t *testing.T, tx pgx.Tx, f cashbackFixtures) string {
	t.Helper()
	var runID string
	err := tx.QueryRow(context.Background(),
		`insert into cashback.reconciliation_run
		     (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
		 values ($1, now() - interval '30 days', now(), $2, $3::jsonb) returning id`,
		f.networkAccountID, f.approverID, `{"total_minor":500,"currency":"EUR"}`).Scan(&runID)
	if err != nil {
		t.Fatalf("seed reconciliation_run: %v", err)
	}
	return runID
}

// TestReconciliationRunIsImmutable asserts C-3 on the imported statement.
func TestReconciliationRunIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   string
		stmt string
	}{
		{name: "rewrite the statement", op: "UPDATE", stmt: `update cashback.reconciliation_run set raw_statement = '{}'::jsonb where id = $1`},
		{name: "move the period", op: "UPDATE", stmt: `update cashback.reconciliation_run set statement_period_end = now() + interval '30 days' where id = $1`},
		{name: "reassign the importer", op: "UPDATE", stmt: `update cashback.reconciliation_run set imported_by = gen_random_uuid() where id = $1`},
		{name: "delete the import", op: "DELETE", stmt: `delete from cashback.reconciliation_run where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackWithdrawal(t, tx)
			runID := seedReconciliationRun(t, tx, f)
			_, err := tx.Exec(context.Background(), tt.stmt, runID)
			wantImmutableRefusal(t, err, "reconciliation_run", tt.op)
		})
	}
}

// TestReconciliationRunRejectsTruncate closes the bulk route. Not parallel:
// TRUNCATE takes ACCESS EXCLUSIVE locks that would contend with the other
// subtests' open transactions.
func TestReconciliationRunRejectsTruncate(t *testing.T) {
	err := refusedTruncate(t, `truncate cashback.reconciliation_run cascade`)
	wantImmutableRefusal(t, err, "reconciliation_run", "TRUNCATE")
}

// TestCashbackReconciliationRejectsIllegalWrites is the rejection table for
// the statement and the differences derived from it.
func TestCashbackReconciliationRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error
		wantCode string
	}{
		{
			name: "statement imported by nobody",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, _ string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_run
					     (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
					 values ($1, now() - interval '1 day', now(), null, '{}'::jsonb)`, f.networkAccountID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "statement with no statement in it",
			rule: "C-3",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, _ string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_run
					     (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
					 values ($1, now() - interval '1 day', now(), $2, null)`,
					f.networkAccountID, f.approverID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "statement period that ends before it starts",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, _ string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_run
					     (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
					 values ($1, now(), now() - interval '1 day', $2, '{}'::jsonb)`,
					f.networkAccountID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "difference of an unknown kind",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference (run_id, network_account_id, kind, actual_minor, currency)
					 values ($1, $2, 'looks_odd', 100, 'EUR')`, runID, f.networkAccountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "mismatch whose amounts agree",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				// If the two figures are the same there is nothing to
				// reconcile, and an operator queue full of those is worse
				// than an empty one.
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, network_transaction_id, expected_minor, actual_minor, currency)
					 values ($1, $2, 'amount_mismatch', $3, 500, 500, 'EUR')`,
					runID, f.networkAccountID, f.networkTxn)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "mismatch stating only one figure",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, network_transaction_id, expected_minor, currency)
					 values ($1, $2, 'amount_mismatch', $3, 500, 'EUR')`,
					runID, f.networkAccountID, f.networkTxn)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "reported-not-paid naming no report",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				// The whole claim is "this report was never paid", so it has
				// to say which report.
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference (run_id, network_account_id, kind, expected_minor, currency)
					 values ($1, $2, 'reported_not_paid', 500, 'EUR')`, runID, f.networkAccountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "paid-not-reported naming a report anyway",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				// The whole claim is "this money matches no report". Naming
				// one contradicts the row's own kind, and reads as coherent
				// in an operator queue while meaning nothing.
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, network_transaction_id, actual_minor, currency)
					 values ($1, $2, 'paid_not_reported', $3, 100, 'EUR')`,
					runID, f.networkAccountID, f.networkTxn)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "reported-not-paid carrying an amount it was paid",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, network_transaction_id, expected_minor, actual_minor, currency)
					 values ($1, $2, 'reported_not_paid', $3, 500, 500, 'EUR')`,
					runID, f.networkAccountID, f.networkTxn)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "difference citing another publisher account's report",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				// A second publisher account at the same network, with its
				// own report. Every single-column foreign key is satisfied;
				// only the composite keys can see the mismatch.
				var otherAccount, otherReport string
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_account (network_id, external_publisher_id, credential_ref)
					 values ($1, $2, 'network.fixture.credential') returning id`,
					f.networkID, "publisher-two-"+f.suffix).Scan(&otherAccount); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'approved', 'confirmed', 5000, 250, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
					f.networkID, otherAccount, "foreign-"+f.suffix).Scan(&otherReport); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, network_transaction_id, expected_minor, actual_minor, currency)
					 values ($1, $2, 'amount_mismatch', $3, 500, 450, 'EUR')`,
					runID, f.networkAccountID, otherReport)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "difference filed against another account's run",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				var otherAccount string
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_account (network_id, external_publisher_id, credential_ref)
					 values ($1, $2, 'network.fixture.credential') returning id`,
					f.networkID, "publisher-three-"+f.suffix).Scan(&otherAccount); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, actual_minor, currency)
					 values ($1, $2, 'paid_not_reported', 100, 'EUR')`, runID, otherAccount)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "difference with no currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference (run_id, network_account_id, kind, actual_minor, currency)
					 values ($1, $2, 'paid_not_reported', 100, null)`, runID, f.networkAccountID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "difference resolved by somebody who gave no reason",
			rule: "US6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures, runID string) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.reconciliation_difference
					     (run_id, network_account_id, kind, actual_minor, currency, resolved_by, resolved_at)
					 values ($1, $2, 'paid_not_reported', 100, 'EUR', $3, now())`,
					runID, f.networkAccountID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackWithdrawal(t, tx)
			runID := seedReconciliationRun(t, tx, f)
			wantPgCode(t, tt.write(context.Background(), tx, f, runID), tt.wantCode)
		})
	}
}

// TestReconciliationDifferenceCanBeResolved is the positive control and the
// point of the table: a difference is raised, worked through, and closed by
// a named operator with a reason on the record (US6).
func TestReconciliationDifferenceCanBeResolved(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)
	runID := seedReconciliationRun(t, tx, f)

	var differenceID string
	err := tx.QueryRow(ctx,
		`insert into cashback.reconciliation_difference
		     (run_id, network_account_id, kind, network_transaction_id, expected_minor, actual_minor, currency)
		 values ($1, $2, 'amount_mismatch', $3, 500, 450, 'EUR') returning id`,
		runID, f.networkAccountID, f.networkTxn).Scan(&differenceID)
	if err != nil {
		t.Fatalf("a valid difference was rejected: %v", err)
	}

	var open int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.reconciliation_difference where run_id = $1 and resolved_at is null`,
		runID).Scan(&open); err != nil {
		t.Fatalf("reading the operator queue: %v", err)
	}
	if open != 1 {
		t.Fatalf("%d open differences, want 1", open)
	}

	if _, err := tx.Exec(ctx,
		`update cashback.reconciliation_difference
		    set resolved_by = $2, resolved_reason = 'network deducted a returns adjustment', resolved_at = now()
		  where id = $1`, differenceID, f.approverID); err != nil {
		t.Fatalf("resolving the difference: %v", err)
	}

	var resolver string
	if err := tx.QueryRow(ctx,
		`select a.display_name
		   from cashback.reconciliation_difference d
		   join account a on a.id = d.resolved_by
		  where d.id = $1`, differenceID).Scan(&resolver); err != nil {
		t.Fatalf("reading the resolution: %v", err)
	}
	if resolver == "" {
		t.Fatal("the resolved difference names no operator")
	}
}
