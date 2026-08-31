package store_test

// The statement that queues a reference matching no click, against the real,
// migrated schema (T067, FR-034).
//
// Nothing above this layer can prove what is asserted here. The predicate
// lives in the statement rather than in Go precisely so that the stored
// columns decide, and a fake would only re-answer the question this file
// exists to ask of Postgres: does a reference that names a real click stay
// out of the queue, and does one that names nothing go into it?

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
)

// click records one click carrying the given reference.
func click(ctx context.Context, t *testing.T, tx pgx.Tx, member, offer pgtype.UUID, ref string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		insert into cashback.click
		    (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
		values ($1, $2, $3, '{"kind":"fixed"}'::jsonb, 6000)`, ref, member, offer); err != nil {
		t.Fatalf("seeding the click: %v", err)
	}
}

// report stores one network report carrying the given reference, or none
// when ref is empty. An external id of its own keeps two reports in one case
// from colliding on the network's uniqueness.
func report(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, publisher pgtype.UUID, ref string) pgtype.UUID {
	t.Helper()
	at := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload)
		values ($1, $2, $3, $4, 'pending', 'pending', 4999, 499, 'EUR', $5, $6, $7, $8, $9)
		returning id`,
		networkID, publisher, "EARN-"+tag(t),
		pgtype.Text{String: ref, Valid: ref != ""},
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"EARN"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

func TestTheUnmatchedReferenceStatementAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaTx(t)
	defer done()

	// The case FR-034's other half exists for: the network named something,
	// and nothing Apivo ever issued answers to it. Networks echo references
	// from other publishers and from stale links, so this is ordinary rather
	// than exceptional - and the money is real, which is why it is queued
	// instead of dropped.
	each(ctx, t, tx, "a reference naming no click is queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		stored := report(ctx, t, tx, networkID, publisher, "ref-that-names-nothing-000")

		row, err := q.RecordUnmatchedReference(ctx, stored)
		if err != nil {
			t.Fatalf("RecordUnmatchedReference(): %v", err)
		}
		if row.NetworkTransactionID != stored {
			t.Errorf("the queue row names report %v, want %v", row.NetworkTransactionID, stored)
		}
		if !row.DetectedAt.Valid {
			t.Error("the row carries no detection instant, so nothing can say when this was noticed")
		}
	})

	// The case that decides whether this statement can cost a member their
	// cashback. A reference that DOES name a click is attributed, and
	// queueing it would put a paid purchase in front of an operator as
	// unclaimed money.
	each(ctx, t, tx, "a reference naming a click is not queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-that-names-a-click"
		click(ctx, t, tx, member, offer, ref)
		stored := report(ctx, t, tx, networkID, publisher, ref)

		_, err := q.RecordUnmatchedReference(ctx, stored)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordUnmatchedReference() = %v, want %v - an attributed report was queued", err, pgx.ErrNoRows)
		}
	})

	// The sibling's half, asked of this statement so the two cannot both
	// claim a report. RecordUnattributedReport queues a report carrying NO
	// reference; if this one did too, one report would take two queue rows
	// and an operator would see the same money twice.
	each(ctx, t, tx, "a report carrying no reference is left to the other half", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		stored := report(ctx, t, tx, networkID, publisher, "")

		_, err := q.RecordUnmatchedReference(ctx, stored)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordUnmatchedReference() = %v, want %v - this half claimed the other's report", err, pgx.ErrNoRows)
		}
	})

	// The ordinary path after a crash: the window is re-read and every
	// observation in it is recorded again. A raw uniqueness violation here
	// would abort the whole window's transaction and leave the cursor where
	// it was, so the window would be re-read forever.
	each(ctx, t, tx, "recording the same observation twice is a no-op", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		stored := report(ctx, t, tx, networkID, publisher, "ref-recorded-twice-0000000")

		if _, err := q.RecordUnmatchedReference(ctx, stored); err != nil {
			t.Fatalf("the first RecordUnmatchedReference(): %v", err)
		}
		_, err := q.RecordUnmatchedReference(ctx, stored)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the second RecordUnmatchedReference() = %v, want %v", err, pgx.ErrNoRows)
		}
	})

	// A row naming a report that does not exist is not something to swallow.
	// The conflict clause names its constraint precisely so a foreign key
	// failure still raises rather than being absorbed as "nothing to do".
	each(ctx, t, tx, "a report that does not exist is not recorded", func(t *testing.T, _ pgx.Tx, q *store.Queries) {
		_, err := q.RecordUnmatchedReference(ctx, pgtype.UUID{Bytes: [16]byte{9, 9, 9}, Valid: true})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordUnmatchedReference() = %v, want %v", err, pgx.ErrNoRows)
		}
	})
}
