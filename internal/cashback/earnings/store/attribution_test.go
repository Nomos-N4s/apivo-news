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

// click records one click carrying the given reference and answers its id.
func click(ctx context.Context, t *testing.T, tx pgx.Tx, member, offer pgtype.UUID, ref string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.click
		    (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
		values ($1, $2, $3, '{"kind":"fixed"}'::jsonb, 6000) returning id`, ref, member, offer).Scan(&id); err != nil {
		t.Fatalf("seeding the click: %v", err)
	}
	return id
}

// credited records one credit resting on the click and the report.
func credited(ctx context.Context, t *testing.T, tx pgx.Tx, member, report, clicked pgtype.UUID) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		insert into cashback.entry
		    (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
		values ('fixture', $1, $2, $3, 'pending', 250, 'EUR')`, member, report, clicked); err != nil {
		t.Fatalf("crediting the click: %v", err)
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

// TestTheCreditedClickStatementAgainstSchema is the third way a report can
// be money nobody can be credited for (spec 004, T202): its reference named
// a click, and that click already backs its one credit (entry_click_id_idx).
// The statement decides, as the unmatched one does, and the two halves must
// not both claim a report.
func TestTheCreditedClickStatementAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaTx(t)
	defer done()

	each(ctx, t, tx, "a reference naming a credited click is queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-credited-already"
		clicked := click(ctx, t, tx, member, offer, ref)
		credited(ctx, t, tx, member, report(ctx, t, tx, networkID, publisher, ref), clicked)
		again := report(ctx, t, tx, networkID, publisher, ref)

		row, err := q.RecordCreditedClickReference(ctx, again)
		if err != nil {
			t.Fatalf("RecordCreditedClickReference(): %v", err)
		}
		if row.NetworkTransactionID != again {
			t.Errorf("the queue row names report %v, want the second report %v", row.NetworkTransactionID, again)
		}
		if !row.DetectedAt.Valid {
			t.Error("the row carries no detection instant")
		}
	})

	// A click that backs no credit yet is an attributable click; queueing
	// its report would take a first purchase away from its member.
	each(ctx, t, tx, "a reference naming an uncredited click is not queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-not-yet-credited"
		click(ctx, t, tx, member, offer, ref)
		stored := report(ctx, t, tx, networkID, publisher, ref)

		if _, err := q.RecordCreditedClickReference(ctx, stored); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordCreditedClickReference() = %v, want %v - an attributable report was queued", err, pgx.ErrNoRows)
		}
	})

	// The unmatched half's report: a reference that names no click is that
	// statement's to queue, and if this one queued it too an operator would
	// see one report as two.
	each(ctx, t, tx, "a reference naming nothing is left to the unmatched half", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		stored := report(ctx, t, tx, networkID, publisher, "ref-that-names-nothing-001")

		if _, err := q.RecordCreditedClickReference(ctx, stored); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordCreditedClickReference() = %v, want %v - this half claimed the unmatched half's report", err, pgx.ErrNoRows)
		}
	})

	each(ctx, t, tx, "recording the same observation twice is a no-op", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-credited-twice-asked"
		clicked := click(ctx, t, tx, member, offer, ref)
		credited(ctx, t, tx, member, report(ctx, t, tx, networkID, publisher, ref), clicked)
		again := report(ctx, t, tx, networkID, publisher, ref)

		if _, err := q.RecordCreditedClickReference(ctx, again); err != nil {
			t.Fatalf("the first RecordCreditedClickReference(): %v", err)
		}
		if _, err := q.RecordCreditedClickReference(ctx, again); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the second RecordCreditedClickReference() = %v, want %v", err, pgx.ErrNoRows)
		}
	})
}

// reportIn stores one report carrying the reference in the given currency.
func reportIn(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, publisher pgtype.UUID, ref, currency string) pgtype.UUID {
	t.Helper()
	at := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload)
		values ($1, $2, $3, $4, 'pending', 'pending', 4999, 499, $5, $6, $7, $8, $9, $10)
		returning id`,
		networkID, publisher, "EARN-"+tag(t), ref, currency,
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"EARN"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// TestTheForeignCurrencyStatementAgainstSchema is FR-109's queue write: a
// report in a currency its member is not in cashback in is queued, one in
// the member's currency is not, and a member in cashback in nothing at all
// is the same answer as the wrong currency.
func TestTheForeignCurrencyStatementAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaTx(t)
	defer done()

	each(ctx, t, tx, "a report in a currency the member cannot be paid in is queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-paid-in-dollars-00"
		click(ctx, t, tx, member, offer, ref)
		stored := reportIn(ctx, t, tx, networkID, publisher, ref, "USD")

		row, err := q.RecordForeignCurrencyReference(ctx, stored)
		if err != nil {
			t.Fatalf("RecordForeignCurrencyReference(): %v", err)
		}
		if row.NetworkTransactionID != stored {
			t.Errorf("the queue row names report %v, want %v", row.NetworkTransactionID, stored)
		}
	})

	each(ctx, t, tx, "a report in the member's currency is not queued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, offer := world(ctx, t, tx)
		ref := "a-reference-paid-in-euros-0000"
		click(ctx, t, tx, member, offer, ref)
		stored := reportIn(ctx, t, tx, networkID, publisher, ref, "EUR")

		if _, err := q.RecordForeignCurrencyReference(ctx, stored); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordForeignCurrencyReference() = %v, want %v - a creditable report was queued", err, pgx.ErrNoRows)
		}
	})

	each(ctx, t, tx, "a member in cashback in no currency at all is queued too", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, offer := world(ctx, t, tx)
		var stranger pgtype.UUID
		if err := tx.QueryRow(ctx, `
			insert into public.account (email, display_name, role)
			values ($1, 'Never Opted In', 'reader') returning id`,
			"stranger-"+tag(t)+"@example.test").Scan(&stranger); err != nil {
			t.Fatalf("seeding the stranger: %v", err)
		}
		ref := "a-reference-by-a-stranger-0000"
		click(ctx, t, tx, stranger, offer, ref)
		stored := reportIn(ctx, t, tx, networkID, publisher, ref, "EUR")

		if _, err := q.RecordForeignCurrencyReference(ctx, stored); err != nil {
			t.Fatalf("RecordForeignCurrencyReference() = %v, want a queue row: nothing says what currency this member is paid in", err)
		}
	})

	each(ctx, t, tx, "a reference naming nothing is left to the unmatched half", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, _, _ := world(ctx, t, tx)
		stored := reportIn(ctx, t, tx, networkID, publisher, "ref-that-names-nothing-002", "USD")

		if _, err := q.RecordForeignCurrencyReference(ctx, stored); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("RecordForeignCurrencyReference() = %v, want %v", err, pgx.ErrNoRows)
		}
	})
}
