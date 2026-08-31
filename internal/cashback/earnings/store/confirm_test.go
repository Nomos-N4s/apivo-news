package store_test

// The reconciliation gate, against the real migrated schema (T070, FR-043).
//
// Every case here is about money a member can withdraw. The gate exists so
// that a confirmed balance means the commission behind it actually arrived,
// so the cases that matter are the ones where it did NOT: no statement, a
// statement for the wrong period, a statement for another publisher, and a
// difference nobody has resolved.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
)

// purchasedAt is when every fixture's purchase happened. The statement
// periods below are placed either side of it.
var purchasedAt = time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)

// operator seeds the named human a reconciliation run needs (US6).
func operator(ctx context.Context, t *testing.T, tx pgx.Tx) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Reconciling Operator', 'operator') returning id`,
		"ops-"+tag(t)+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	return id
}

// reportAt stores one report for the given publisher, transacted at
// purchasedAt.
func reportAt(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, publisher pgtype.UUID) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload)
		values ($1, $2, $3, null, 'confirmed', 'confirmed', 4999, 499, 'EUR', $4, $5, $6, $7, $8)
		returning id`,
		networkID, publisher, "RECON-"+tag(t),
		purchasedAt, purchasedAt.Add(time.Hour),
		purchasedAt.Add(-48*time.Hour), purchasedAt.Add(48*time.Hour),
		[]byte(`{"transaction_id":"RECON"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// statement imports a run covering the given period for the given publisher.
func statement(ctx context.Context, t *testing.T, tx pgx.Tx, publisher, by pgtype.UUID, from, to time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.reconciliation_run (
			network_account_id, statement_period_start, statement_period_end,
			imported_by, raw_statement)
		values ($1, $2, $3, $4, '{"lines":[]}'::jsonb) returning id`,
		publisher, from, to, by).Scan(&id); err != nil {
		t.Fatalf("importing the statement: %v", err)
	}
	return id
}

// reconciled runs the statement under test.
func reconciled(ctx context.Context, t *testing.T, q *store.Queries, report pgtype.UUID) bool {
	t.Helper()
	answer, err := q.ReportIsReconciled(ctx, report)
	if err != nil {
		t.Fatalf("ReportIsReconciled(): %v", err)
	}
	// `exists` is never null, so an answer that is not valid means the shape
	// of the statement changed under the test rather than that the database
	// does not know - and reading .Bool off it would silently be false.
	if !answer.Valid {
		t.Fatal("ReportIsReconciled() answered null, which exists() cannot produce")
	}
	return answer.Bool
}

// covering is a statement period that contains purchasedAt, and the two
// helpers beside it are periods that do not.
func covering() (time.Time, time.Time) {
	return purchasedAt.Add(-24 * time.Hour), purchasedAt.Add(24 * time.Hour)
}

func TestTheReconciliationGateAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaTx(t)
	defer done()

	// The whole point of the gate. Until a statement arrives, the network has
	// said what it intends to pay and nothing has said it paid.
	each(ctx, t, tx, "a report with no statement is not reconciled", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)

		if reconciled(ctx, t, q, report) {
			t.Error("a report no statement covers was reported as reconciled")
		}
	})

	each(ctx, t, tx, "a report a statement covers is reconciled", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		from, to := covering()
		statement(ctx, t, tx, publisher, operator(ctx, t, tx), from, to)

		if !reconciled(ctx, t, q, report) {
			t.Error("a report covered by a clean statement was not reported as reconciled")
		}
	})

	// A statement for the wrong month pays for other purchases. Matching it
	// would confirm money that is still outstanding.
	each(ctx, t, tx, "a statement for another period does not cover it", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		operatorID := operator(ctx, t, tx)
		statement(ctx, t, tx, publisher, operatorID, purchasedAt.Add(-72*time.Hour), purchasedAt.Add(-24*time.Hour))
		statement(ctx, t, tx, publisher, operatorID, purchasedAt.Add(24*time.Hour), purchasedAt.Add(72*time.Hour))

		if reconciled(ctx, t, q, report) {
			t.Error("a statement for a period either side of the purchase was treated as covering it")
		}
	})

	// The boundary, both ends. The period is half-open, so a purchase at the
	// end instant belongs to the NEXT statement and would otherwise be
	// reconciled twice - once by each.
	each(ctx, t, tx, "the period is half-open at its end", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		operatorID := operator(ctx, t, tx)
		statement(ctx, t, tx, publisher, operatorID, purchasedAt.Add(-24*time.Hour), purchasedAt)

		if reconciled(ctx, t, q, report) {
			t.Error("a statement ending exactly at the purchase was treated as covering it")
		}
	})

	each(ctx, t, tx, "the period is closed at its start", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		statement(ctx, t, tx, publisher, operator(ctx, t, tx), purchasedAt, purchasedAt.Add(24*time.Hour))

		if !reconciled(ctx, t, q, report) {
			t.Error("a statement starting exactly at the purchase was not treated as covering it")
		}
	})

	// Another publisher's statement is another publisher's money. The schema
	// keys a difference to one account for the same reason.
	each(ctx, t, tx, "another publisher's statement does not cover it", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		var other pgtype.UUID
		if err := tx.QueryRow(ctx, `
			insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
			values ($1, $2, 'config:networks.other.credential', true) returning id`,
			networkID, "publisher-"+tag(t)).Scan(&other); err != nil {
			t.Fatalf("seeding the other publisher: %v", err)
		}
		from, to := covering()
		statement(ctx, t, tx, other, operator(ctx, t, tx), from, to)

		if reconciled(ctx, t, q, report) {
			t.Error("a statement belonging to another publisher account was treated as covering it")
		}
	})

	// The case the second half of the gate exists for: the statement arrived
	// and disagrees. Confirming here would credit a member for money the
	// network's own account says it did not pay.
	each(ctx, t, tx, "an unresolved difference blocks it", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		from, to := covering()
		run := statement(ctx, t, tx, publisher, operator(ctx, t, tx), from, to)
		if _, err := tx.Exec(ctx, `
			insert into cashback.reconciliation_difference (
				run_id, network_account_id, kind, network_transaction_id,
				expected_minor, actual_minor, currency)
			values ($1, $2, 'reported_not_paid', $3, 499, null, 'EUR')`,
			run, publisher, report); err != nil {
			t.Fatalf("filing the difference: %v", err)
		}

		if reconciled(ctx, t, q, report) {
			t.Error("a report with an unresolved difference against it was reported as reconciled")
		}
	})

	// And the other side of it: a difference somebody has explained is no
	// longer a reason to hold a member's money.
	each(ctx, t, tx, "a resolved difference does not block it", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		from, to := covering()
		operatorID := operator(ctx, t, tx)
		run := statement(ctx, t, tx, publisher, operatorID, from, to)
		if _, err := tx.Exec(ctx, `
			insert into cashback.reconciliation_difference (
				run_id, network_account_id, kind, network_transaction_id,
				expected_minor, actual_minor, currency,
				resolved_by, resolved_reason, resolved_at)
			values ($1, $2, 'amount_mismatch', $3, 499, 450, 'EUR', $4, 'the network applied a partial refund', now())`,
			run, publisher, report, operatorID); err != nil {
			t.Fatalf("filing the resolved difference: %v", err)
		}

		if !reconciled(ctx, t, q, report) {
			t.Error("a difference somebody had resolved still blocked the report")
		}
	})

	// Money on a statement matching no report names no report, so it cannot
	// be about this one. The table's shape constraint is what makes that
	// structural, and this asserts the query relies on it correctly.
	each(ctx, t, tx, "money matching no report blocks nothing", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		from, to := covering()
		run := statement(ctx, t, tx, publisher, operator(ctx, t, tx), from, to)
		if _, err := tx.Exec(ctx, `
			insert into cashback.reconciliation_difference (
				run_id, network_account_id, kind, network_transaction_id,
				expected_minor, actual_minor, currency)
			values ($1, $2, 'paid_not_reported', null, null, 700, 'EUR')`,
			run, publisher); err != nil {
			t.Fatalf("filing the unmatched payment: %v", err)
		}

		if !reconciled(ctx, t, q, report) {
			t.Error("a payment matching no report blocked a report it does not name")
		}
	})
}
