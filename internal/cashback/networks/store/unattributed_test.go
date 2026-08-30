package store_test

// Exercises the unattributed queue's three statements against the real,
// migrated schema (T058).
//
// The write is one statement whose whole job is a predicate the DATABASE
// owns, and the two reads are one predicate spelled twice - so all three are
// worth asserting where that predicate actually lives. In particular
// "still work" is derived rather than stored: an observation stops being
// work when a later report replaces the one it names, and nothing is edited
// when that happens. Only the schema can be asked whether that is true.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// beginning is older than any detected_at a case will produce, so a listing
// that starts here starts at the beginning.
var beginning = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// openPage lists from the beginning, which is what every case here wants.
func openPage(ctx context.Context, t *testing.T, q *store.Queries) []store.ListOpenUnattributedReportsRow {
	t.Helper()
	rows, err := q.ListOpenUnattributedReports(ctx, store.ListOpenUnattributedReportsParams{
		AfterDetectedAt: pgtype.Timestamptz{Time: beginning, Valid: true},
		AfterID:         pgtype.UUID{Valid: true},
		PageSize:        50,
	})
	if err != nil {
		t.Fatalf("ListOpenUnattributedReports(): %v", err)
	}
	return rows
}

// storeReport writes one evidence row and answers its id. The report is
// unattributed unless a click reference is given. Every case makes its own
// network, so one external id is enough to keep them apart.
func storeReport(ctx context.Context, t *testing.T, q *store.Queries, networkID string, accountID pgtype.UUID, clickRef string, supersedes pgtype.UUID) pgtype.UUID {
	t.Helper()
	params := report(networkID, accountID)
	params.ClickRef = pgtype.Text{}
	if clickRef != "" {
		params.ClickRef = pgtype.Text{String: clickRef, Valid: true}
	}
	params.SupersedesID = supersedes
	// A superseding row must differ in some reported fact or the digest
	// makes it the same report.
	if supersedes.Valid {
		params.StatusRaw, params.Status = "validated", "confirmed"
	}
	row, err := q.InsertNetworkTransaction(ctx, params)
	if err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return row.ID
}

func TestUnattributedQueueAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the unattributed queue")
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

	each(ctx, t, tx, "a report the network attributed is not queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		attributed := storeReport(ctx, t, q, networkID, accountID, "Zml4dHVyZS1jbGljay0wMDAwMDAwMQ", pgtype.UUID{})

		// The predicate is the statement's. A caller cannot ask it to queue
		// a report the network attributed.
		_, err := q.RecordUnattributedReport(ctx, attributed)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("queueing an attributed report returned %v, want pgx.ErrNoRows", err)
		}
		if rows := openPage(ctx, t, q); len(rows) != 0 {
			t.Errorf("%d row(s) of work for an attributed report, want 0", len(rows))
		}
	})

	each(ctx, t, tx, "a report with no reference is recorded once, and only once", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		reportID := storeReport(ctx, t, q, networkID, accountID, "", pgtype.UUID{})

		queued, err := q.RecordUnattributedReport(ctx, reportID)
		if err != nil {
			t.Fatalf("RecordUnattributedReport(): %v", err)
		}
		if queued.NetworkTransactionID != reportID {
			t.Errorf("the row names report %v, want %v", queued.NetworkTransactionID, reportID)
		}
		if !queued.DetectedAt.Valid {
			t.Error("the row carries no detection instant")
		}

		// A window re-read after a crash records the same observation
		// again, and must not fail: a raw 23505 would abort the whole
		// transaction and take the rest of the window with it.
		if _, err := q.RecordUnattributedReport(ctx, reportID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("recording the same observation twice returned %v, want pgx.ErrNoRows", err)
		}
		if rows := openPage(ctx, t, q); len(rows) != 1 {
			t.Fatalf("%d row(s) of work after recording twice, want 1", len(rows))
		}
	})

	each(ctx, t, tx, "an observation stops being work when a later report replaces it", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		first := storeReport(ctx, t, q, networkID, accountID, "", pgtype.UUID{})
		if _, err := q.RecordUnattributedReport(ctx, first); err != nil {
			t.Fatalf("recording the first observation: %v", err)
		}
		if rows := openPage(ctx, t, q); len(rows) != 1 {
			t.Fatalf("%d row(s) of work before the supersede, want 1", len(rows))
		}

		// The network joins the sale to a click. Nothing is edited: the
		// observation stays exactly as true as it was, and simply stops
		// being anybody's work.
		storeReport(ctx, t, q, networkID, accountID, "Zml4dHVyZS1jbGljay0wMDAwMDAwMQ", first)

		if rows := openPage(ctx, t, q); len(rows) != 0 {
			t.Errorf("%d row(s) of work after the network attributed the transaction, want 0", len(rows))
		}
		var recorded int
		if err := tx.QueryRow(ctx, `select count(*) from cashback.unattributed_transaction where network_transaction_id = $1`, first).Scan(&recorded); err != nil {
			t.Fatalf("counting the recorded observation: %v", err)
		}
		if recorded != 1 {
			t.Errorf("the observation was %d row(s) afterwards, want it still recorded", recorded)
		}
		if _, err := q.GetOpenUnattributedReport(ctx, pgtype.UUID{}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetOpenUnattributedReport(zero) returned %v, want pgx.ErrNoRows", err)
		}
	})

	each(ctx, t, tx, "a resolved observation is not work", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		reportID := storeReport(ctx, t, q, networkID, accountID, "", pgtype.UUID{})
		queued, err := q.RecordUnattributedReport(ctx, reportID)
		if err != nil {
			t.Fatalf("RecordUnattributedReport(): %v", err)
		}

		operator := seedAccount(ctx, t, tx, "operator")
		if _, err := tx.Exec(ctx, `
			update cashback.unattributed_transaction
			   set resolved_by = $2, resolved_reason = 'dismissed: our own test purchase', resolved_at = now()
			 where id = $1`, queued.ID, operator); err != nil {
			t.Fatalf("resolving: %v", err)
		}

		if rows := openPage(ctx, t, q); len(rows) != 0 {
			t.Errorf("%d row(s) of work after an operator resolved it, want 0", len(rows))
		}
		if _, err := q.GetOpenUnattributedReport(ctx, queued.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a resolved row answered %v, want pgx.ErrNoRows", err)
		}
	})

	each(ctx, t, tx, "a credited report is not work, whatever the queue row says", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		reportID := storeReport(ctx, t, q, networkID, accountID, "", pgtype.UUID{})
		if _, err := q.RecordUnattributedReport(ctx, reportID); err != nil {
			t.Fatalf("RecordUnattributedReport(): %v", err)
		}

		// The belt. An operator action writes the entry and the resolution
		// together, but a report already credited must never come back as
		// work even if that resolution were lost.
		member := seedAccount(ctx, t, tx, "reader")
		if _, err := tx.Exec(ctx, `
			insert into cashback.entry (account_id, brand_id, network_transaction_id, state, amount_minor, currency)
			values ($1, 'test-brand', $2, 'pending', 100, 'EUR')`, member, reportID); err != nil {
			t.Fatalf("crediting the report: %v", err)
		}

		if rows := openPage(ctx, t, q); len(rows) != 0 {
			t.Errorf("%d row(s) of work for a report already credited, want 0", len(rows))
		}
	})

	each(ctx, t, tx, "a reference matching no click is work an operator may only dismiss", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		// The state T067 will record: the network named a reference, and no
		// click of ours carries it. This module does not write such a row
		// yet, so it is planted - what is under test is that the READ can
		// carry it, which is what lets the operator surface be written once.
		orphan := storeReport(ctx, t, q, networkID, accountID, "bm90LWEtY2xpY2std2Uta25vdw", pgtype.UUID{})
		if _, err := tx.Exec(ctx,
			`insert into cashback.unattributed_transaction (network_transaction_id) values ($1)`, orphan); err != nil {
			t.Fatalf("planting the orphan observation: %v", err)
		}

		rows := openPage(ctx, t, q)
		if len(rows) != 1 {
			t.Fatalf("%d row(s) of work, want 1", len(rows))
		}
		if rows[0].Attributable {
			t.Error("a report naming a click nobody minted was offered as attributable; entry_evidence_guard refuses a null click_id there, and there is no click to cite")
		}
	})

	each(ctx, t, tx, "the open row carries the current facts, in detection order", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, accountID := account(ctx, t, tx)
		first := storeReport(ctx, t, q, networkID, accountID, "", pgtype.UUID{})
		if _, err := q.RecordUnattributedReport(ctx, first); err != nil {
			t.Fatalf("recording the first observation: %v", err)
		}
		// The network restates the amount, still with no reference. The
		// second observation is the work; the first is not, and the money
		// an operator reads is the restated figure rather than the
		// withdrawn one - because the open row IS the current report.
		restated := report(networkID, accountID)
		restated.ClickRef = pgtype.Text{}
		restated.SupersedesID = first
		restated.CommissionMinor = 1125
		second, err := q.InsertNetworkTransaction(ctx, restated)
		if err != nil {
			t.Fatalf("storing the restated report: %v", err)
		}
		if _, err := q.RecordUnattributedReport(ctx, second.ID); err != nil {
			t.Fatalf("recording the second observation: %v", err)
		}

		rows := openPage(ctx, t, q)
		if len(rows) != 1 {
			t.Fatalf("%d row(s) of work for one transaction, want 1", len(rows))
		}
		if rows[0].NetworkTransactionID != second.ID {
			t.Errorf("the open row names report %v, want the current one %v", rows[0].NetworkTransactionID, second.ID)
		}
		if rows[0].CommissionMinor != 1125 {
			t.Errorf("the open row shows a commission of %d, want the restated 1125", rows[0].CommissionMinor)
		}

		// And the same row answers the same way one at a time, which is how
		// an action re-asks the question in its own transaction.
		one, err := q.GetOpenUnattributedReport(ctx, rows[0].ID)
		if err != nil {
			t.Fatalf("GetOpenUnattributedReport(): %v", err)
		}
		if one.NetworkTransactionID != second.ID || one.CommissionMinor != 1125 {
			t.Errorf("the single-row read disagrees with the listing: %+v", one)
		}
	})
}
