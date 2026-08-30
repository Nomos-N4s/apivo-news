package store_test

// The two statements the operator surface owns, against the real, migrated
// schema (T059).
//
// Both are about a single column moving once. The resolution is the one
// part of a queue row 0013 leaves movable, and 0024 makes even that
// one-way, so what is worth asserting here is not that an update works but
// that a SECOND one does not: `resolved_at is null` in the WHERE is what
// turns two operators deciding the same row at the same moment into one
// recorded reason and one refusal, and nothing above this layer can prove
// it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// tag is a short random string that keeps one case's fixtures from
// colliding with another's.
func tag(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// queueRow seeds a network, a publisher account, one report with no click
// reference, and the queue row recording it. It answers both ids, because
// the classification statement asks about the report as well as the row.
func queueRow(ctx context.Context, t *testing.T, tx pgx.Tx) (row, report pgtype.UUID, networkID string, accountID pgtype.UUID) {
	t.Helper()
	networkID = "opsfix_" + tag(t)

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Ops Fixture Network', 'clickref', 31, 6, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.opsfix.credential', true)
		returning id`, networkID).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	report = transaction(ctx, t, tx, networkID, accountID, pgtype.UUID{})
	if err := tx.QueryRow(ctx, `
		insert into cashback.unattributed_transaction (network_transaction_id)
		values ($1) returning id`, report).Scan(&row); err != nil {
		t.Fatalf("seeding the queue row: %v", err)
	}
	return row, report, networkID, accountID
}

// transaction writes one report with no click reference. A successor
// differs in the reported status, because an identical successor is the
// same report by digest.
func transaction(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, accountID, supersedes pgtype.UUID) pgtype.UUID {
	t.Helper()
	at := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	status := "pending"
	if supersedes.Valid {
		status = "confirmed"
	}
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload, supersedes_id)
		values ($1, $2, 'FIX-1001', null, $3, $3, 4999, 499, 'EUR', $4, $5, $6, $7, $8, $9)
		returning id`,
		networkID, accountID, status,
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"FIX-1001"}`), supersedes,
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// person seeds an account in the given role.
func person(ctx context.Context, t *testing.T, tx pgx.Tx, role string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Test Person', $2) returning id`,
		role+"-"+tag(t)+"@example.test", role).Scan(&id); err != nil {
		t.Fatalf("seeding an account: %v", err)
	}
	return id
}

// resolve runs the statement under test.
func resolve(ctx context.Context, q *store.Queries, row, by pgtype.UUID, reason string) (store.CashbackUnattributedTransaction, error) {
	return q.ResolveUnattributedReport(ctx, store.ResolveUnattributedReportParams{
		ID:             row,
		ResolvedBy:     by,
		ResolvedReason: pgtype.Text{String: reason, Valid: true},
	})
}

// each runs one case inside a savepoint of its own - pgx spells a nested
// Begin as one - and rolls it back afterwards, so a case that provokes a
// refusal leaves the outer transaction usable for the next.
func each(ctx context.Context, t *testing.T, tx pgx.Tx, name string, scenario func(t *testing.T, tx pgx.Tx, q *store.Queries)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		sub, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer func() { _ = sub.Rollback(ctx) }()
		scenario(t, sub, store.New(sub))
	})
}

// constraintOf names the constraint a refusal came from, so a case asserts
// the rule it meant to rather than any error with the right SQLSTATE.
func constraintOf(t *testing.T, err error) string {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a database refusal", err)
	}
	return pgErr.ConstraintName
}

func TestTheOperatorStatementsAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the operator statements")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	each(ctx, t, tx, "closing a row records who, why and when together", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, report, _, _ := queueRow(ctx, t, tx)
		operator := person(ctx, t, tx, "operator")

		closed, err := resolve(ctx, q, row, operator, "a staff test order")
		if err != nil {
			t.Fatalf("ResolveUnattributedReport(): %v", err)
		}
		if closed.NetworkTransactionID != report {
			t.Errorf("the closed row names report %v, want %v", closed.NetworkTransactionID, report)
		}
		if closed.ResolvedBy != operator {
			t.Errorf("resolved_by = %v, want %v", closed.ResolvedBy, operator)
		}
		if !closed.ResolvedAt.Valid || closed.ResolvedReason.String != "a staff test order" {
			t.Errorf("the row holds a half-recorded resolution: %+v", closed)
		}
	})

	// The case the whole guard exists for. Above this layer the openness
	// read refuses a second decision first, so only here can the statement
	// be asked the question directly.
	each(ctx, t, tx, "a row already closed matches nothing, so no reason is overwritten", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, _, _, _ := queueRow(ctx, t, tx)
		first, second := person(ctx, t, tx, "operator"), person(ctx, t, tx, "operator")

		if _, err := resolve(ctx, q, row, first, "duplicate of TX-9"); err != nil {
			t.Fatalf("the first resolution: %v", err)
		}

		if _, err := resolve(ctx, q, row, second, "no, something else"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the second resolution = %v, want %v", err, pgx.ErrNoRows)
		}

		var by pgtype.UUID
		var reason string
		if err := tx.QueryRow(ctx,
			`select resolved_by, resolved_reason from cashback.unattributed_transaction where id = $1`,
			row).Scan(&by, &reason); err != nil {
			t.Fatalf("reading the row back: %v", err)
		}
		if by != first || reason != "duplicate of TX-9" {
			t.Errorf("the row holds %v / %q, want the first operator's decision", by, reason)
		}
	})

	each(ctx, t, tx, "a blank reason is refused by the schema, not only by the endpoint", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, _, _, _ := queueRow(ctx, t, tx)
		operator := person(ctx, t, tx, "operator")

		_, err := resolve(ctx, q, row, operator, "   ")
		if constraint := constraintOf(t, err); constraint != "unattributed_resolved_reason_not_blank" {
			t.Errorf("it was refused by %q, want unattributed_resolved_reason_not_blank", constraint)
		}
	})

	each(ctx, t, tx, "an approver with no account is refused by the foreign key", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, _, _, _ := queueRow(ctx, t, tx)

		// FR-061's "a named human" is the foreign key's guarantee, not the
		// application's: an id nobody holds cannot be recorded as having
		// decided anything.
		_, err := resolve(ctx, q, row, pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, "a reason")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.ForeignKeyViolation {
			t.Fatalf("resolving as a nonexistent account = %v, want a foreign key violation", err)
		}
	})

	each(ctx, t, tx, "an id that names no row is classified as nothing at all", func(t *testing.T, _ pgx.Tx, q *store.Queries) {
		_, err := q.ClassifyUnattributedReport(ctx, pgtype.UUID{Bytes: [16]byte{2}, Valid: true})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClassifyUnattributedReport() of an unknown id = %v, want %v", err, pgx.ErrNoRows)
		}
	})

	each(ctx, t, tx, "the classification names each reason a row is closed", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, _, _, _ := queueRow(ctx, t, tx)
		operator := person(ctx, t, tx, "operator")

		open, err := q.ClassifyUnattributedReport(ctx, row)
		if err != nil {
			t.Fatalf("ClassifyUnattributedReport(): %v", err)
		}
		if open.ResolvedAt.Valid || open.Superseded || open.Credited {
			t.Errorf("an untouched row classifies as %+v, want open", open)
		}

		if _, err := resolve(ctx, q, row, operator, "a staff test order"); err != nil {
			t.Fatalf("resolving: %v", err)
		}
		closed, err := q.ClassifyUnattributedReport(ctx, row)
		if err != nil {
			t.Fatalf("ClassifyUnattributedReport(): %v", err)
		}
		// The reason travels with the refusal, so the operator whose page
		// went stale is told what the row was closed FOR rather than merely
		// that it was.
		if !closed.ResolvedAt.Valid || closed.ResolvedReason.String != "a staff test order" {
			t.Errorf("the classification is %+v, want the resolution that stands", closed)
		}
	})

	each(ctx, t, tx, "a superseded report is classified as superseded", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, report, networkID, accountID := queueRow(ctx, t, tx)
		transaction(ctx, t, tx, networkID, accountID, report)

		got, err := q.ClassifyUnattributedReport(ctx, row)
		if err != nil {
			t.Fatalf("ClassifyUnattributedReport(): %v", err)
		}
		if !got.Superseded {
			t.Error("a row whose report has a successor does not classify as superseded")
		}
		if got.Credited || got.ResolvedAt.Valid {
			t.Errorf("it also classifies as %+v; only the successor changed", got)
		}
	})

	each(ctx, t, tx, "a credited report is classified as credited", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		row, report, _, _ := queueRow(ctx, t, tx)
		member := person(ctx, t, tx, "reader")

		// The belt an operator action wears beside its own resolution: a
		// report already credited must never come back as decidable, even
		// if the resolution were somehow lost.
		if _, err := tx.Exec(ctx, `
			insert into cashback.entry (account_id, brand_id, network_transaction_id, state, amount_minor, currency)
			values ($1, 'test-brand', $2, 'pending', 100, 'EUR')`, member, report); err != nil {
			t.Fatalf("crediting the report: %v", err)
		}

		got, err := q.ClassifyUnattributedReport(ctx, row)
		if err != nil {
			t.Fatalf("ClassifyUnattributedReport(): %v", err)
		}
		if !got.Credited {
			t.Error("a row whose report carries an entry does not classify as credited")
		}
		if got.Superseded || got.ResolvedAt.Valid {
			t.Errorf("it also classifies as %+v; only the entry was added", got)
		}
	})
}
