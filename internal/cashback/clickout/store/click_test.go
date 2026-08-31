package store_test

// The two click statements against the real, migrated schema (T063).
//
// A click row is the evidence a member's money rests on, and almost every
// rule that makes it trustworthy lives in the database rather than in Go:
// the reference is unique and long enough to be unguessable (FR-020), the
// member is named and cannot be null (FR-023), the row can never be edited
// afterwards (C-3). None of those can be proved anywhere but here, and the
// insert is written to let each of them raise rather than swallowing any.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// aRateSnapshot is one published band as [catalogue.RateBand] encodes it -
// the shape FR-013 says governs the credit.
var aRateSnapshot = []byte(`{"kind":"percent","bps":400}`)

// suffix keeps one case's identifiers from colliding with another's.
func suffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// aClickRef is a reference of exactly the shape the minter produces: 22
// URL-safe characters, unique per call.
func aClickRef(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 11)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random reference: %v", err)
	}
	return hex.EncodeToString(raw)
}

// member seeds an account that may click.
func member(ctx context.Context, t *testing.T, tx pgx.Tx) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Clicking Member', 'reader') returning id`,
		"member-"+suffix(t)+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	return id
}

// offer seeds a network, merchant, route and one live percent band, and
// answers the offer's id.
func offer(ctx context.Context, t *testing.T, tx pgx.Tx) pgtype.UUID {
	t.Helper()
	tag := suffix(t)
	networkID := "clicktest_" + tag

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Click Test Network', 'clickref', 31, 300, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var merchantID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', 'de', 'active') returning id`, "click-test-"+tag).Scan(&merchantID); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	var routeID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('fixture', $1, $2, $3, now(), '{"id":"fixture"}'::jsonb, 'active', true) returning id`,
		merchantID, networkID, "ext-"+tag).Scan(&routeID); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	var offerID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, deeplink_template)
		values ($1, 'percent', 400, 5000, now() - interval '1 day', 'https://example.test/deeplink?ref={ref}')
		returning id`, routeID).Scan(&offerID); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}
	return offerID
}

// aClick is one insert's worth of parameters, already valid. A case states
// only what it changes, so the mutation under test is visible at the call
// site rather than buried in a helper.
func aClick(ctx context.Context, t *testing.T, tx pgx.Tx) store.InsertClickParams {
	t.Helper()
	return store.InsertClickParams{
		ClickRef:               aClickRef(t),
		AccountID:              member(ctx, t, tx),
		OfferID:                offer(ctx, t, tx),
		RateSnapshot:           aRateSnapshot,
		MemberShareBpsSnapshot: 5000,
	}
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

// refused runs one insert inside a savepoint of its own and returns its
// error, leaving the caller's transaction usable.
//
// A refusal aborts the transaction it happens in (25P02), so a case that
// provokes several - and every case below provokes several, because the
// near misses are the point - would otherwise be unable to seed or to try
// the next one.
func refused(ctx context.Context, t *testing.T, tx pgx.Tx, params store.InsertClickParams) error {
	t.Helper()
	sub, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() { _ = sub.Rollback(ctx) }()
	_, err = store.New(sub).InsertClick(ctx, params)
	return err
}

// decoded reads a jsonb column back as a document, for comparing snapshots
// by meaning rather than by the bytes the writer happened to send.
func decoded(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the snapshot %s is not a JSON object: %v", raw, err)
	}
	return out
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

func TestTheClickStatementsAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the click store")
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

	each(ctx, t, tx, "a click is recorded with the rate it was promised at", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		params := aClick(ctx, t, tx)
		params.ContextDigest = pgtype.Text{String: "ctx-" + suffix(t), Valid: true}

		before := time.Now().UTC()
		click, err := q.InsertClick(ctx, params)
		if err != nil {
			t.Fatalf("InsertClick(): %v", err)
		}

		if click.ClickRef != params.ClickRef || click.AccountID != params.AccountID || click.OfferID != params.OfferID {
			t.Errorf("the row reads %+v, want the click that was recorded", click)
		}
		// Compared as documents, not as bytes: jsonb stores a normalised
		// value, so the column reads back with its members in the
		// database's order rather than the writer's. What FR-013 needs
		// preserved is the band, and that is what is asserted.
		if got, want := decoded(t, click.RateSnapshot), decoded(t, aRateSnapshot); !reflect.DeepEqual(got, want) {
			t.Errorf("rate_snapshot = %v, want the band as published %v", got, want)
		}
		if click.MemberShareBpsSnapshot != 5000 {
			t.Errorf("member_share_bps_snapshot = %d, want 5000", click.MemberShareBpsSnapshot)
		}
		// Stamped by the row, not supplied: the instant an auditor reads is
		// the database's, and it is returned so no caller has to guess it.
		if !click.ClickedAt.Valid || click.ClickedAt.Time.Before(before.Add(-time.Minute)) {
			t.Errorf("clicked_at = %v, want the row's own clock", click.ClickedAt)
		}

		found, err := q.GetClickByRef(ctx, params.ClickRef)
		if err != nil {
			t.Fatalf("GetClickByRef(): %v", err)
		}
		if found.ID != click.ID {
			t.Errorf("GetClickByRef found %v, want the click just recorded %v", found.ID, click.ID)
		}
	})

	each(ctx, t, tx, "a reference is matched exactly or not at all", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		params := aClick(ctx, t, tx)
		params.ClickRef = "AAAAAAAAAAAAAAAAAAAAAa"
		if _, err := q.InsertClick(ctx, params); err != nil {
			t.Fatalf("InsertClick(): %v", err)
		}

		// Every near miss a normalising lookup would resolve. Each one is a
		// different member's credit if it matched: the reference is the only
		// thing standing between one purchase and another's payment.
		for _, near := range []string{
			strings.ToLower(params.ClickRef),
			strings.ToUpper(params.ClickRef),
			" " + params.ClickRef,
			params.ClickRef + " ",
			params.ClickRef[:len(params.ClickRef)-1],
			params.ClickRef + "a",
		} {
			if _, err := q.GetClickByRef(ctx, near); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("GetClickByRef(%q) = %v, want %v - only an exact reference is this click's", near, err, pgx.ErrNoRows)
			}
		}
	})

	each(ctx, t, tx, "a second click cannot take a reference already issued", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		first := aClick(ctx, t, tx)
		if _, err := q.InsertClick(ctx, first); err != nil {
			t.Fatalf("InsertClick(): %v", err)
		}

		// Deliberately not swallowed by the statement. At 128 bits this
		// means the entropy source is broken or a caller is re-using a
		// reference, and both must surface rather than quietly cost the
		// first member the credit their reference was going to carry.
		second := aClick(ctx, t, tx)
		second.ClickRef = first.ClickRef
		_, err := q.InsertClick(ctx, second)
		if constraint := constraintOf(t, err); constraint != "click_ref_unique" {
			t.Errorf("it was refused by %q, want click_ref_unique", constraint)
		}
	})

	each(ctx, t, tx, "a reference too short or punctuated to be unguessable is refused", func(t *testing.T, tx pgx.Tx, _ *store.Queries) {
		params := aClick(ctx, t, tx)
		for _, bad := range []string{
			"",
			"tooshort",
			"AAAAAAAAAAAAAAAAAAAAA",  // 21 characters: one short of 128 bits encoded
			"AAAAAAAAAAAAAAAAAAAA==", // 22 characters, but base64 padding
			"AAAAAAAAAAAAAAAAAAAA/+", // 22 characters, but not URL-safe
		} {
			params.ClickRef = bad
			if constraint := constraintOf(t, refused(ctx, t, tx, params)); constraint != "click_ref_url_safe_and_long_enough" {
				t.Errorf("%q was refused by %q, want click_ref_url_safe_and_long_enough", bad, constraint)
			}
		}
	})

	// FR-023, in the schema rather than in a comment: there is no null to
	// fall back on and no account behind an invented id, so a click that
	// names nobody cannot be written and can never later be adopted.
	each(ctx, t, tx, "a click that names no member is unrepresentable", func(t *testing.T, tx pgx.Tx, _ *store.Queries) {
		params := aClick(ctx, t, tx)
		var pgErr *pgconn.PgError

		params.AccountID = pgtype.UUID{}
		if err := refused(ctx, t, tx, params); !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.NotNullViolation {
			t.Errorf("a click with no account = %v, want a not-null violation", err)
		}

		params.AccountID = pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
		if err := refused(ctx, t, tx, params); !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.ForeignKeyViolation {
			t.Errorf("a click naming an account nobody holds = %v, want a foreign key violation", err)
		}
	})

	each(ctx, t, tx, "a member share outside the possible range is refused", func(t *testing.T, tx pgx.Tx, _ *store.Queries) {
		params := aClick(ctx, t, tx)
		for _, bps := range []int32{-1, 10001} {
			params.MemberShareBpsSnapshot = bps
			if constraint := constraintOf(t, refused(ctx, t, tx, params)); constraint != "click_member_share_bps_range" {
				t.Errorf("a share of %d bps was refused by %q, want click_member_share_bps_range", bps, constraint)
			}
		}
	})

	each(ctx, t, tx, "a blank context digest is refused, and an absent one is ordinary", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		params := aClick(ctx, t, tx)
		params.ContextDigest = pgtype.Text{String: "   ", Valid: true}
		if constraint := constraintOf(t, refused(ctx, t, tx, params)); constraint != "click_context_digest_not_blank" {
			t.Errorf("it was refused by %q, want click_context_digest_not_blank", constraint)
		}

		// Absent is a different fact and a legitimate one: the column is
		// nullable, and a click with no context to digest records none
		// rather than an empty string pretending to be one (FR-022).
		params.ContextDigest = pgtype.Text{}
		click, err := q.InsertClick(ctx, params)
		if err != nil {
			t.Fatalf("InsertClick() with no context digest: %v", err)
		}
		if click.ContextDigest.Valid {
			t.Errorf("context_digest = %q, want null", click.ContextDigest.String)
		}
	})

	// C-3 for the click: the evidence a credit rests on cannot be rewritten
	// after the credit exists, or the audit answers with whatever the last
	// writer preferred.
	each(ctx, t, tx, "a recorded click can never be edited or deleted", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		click, err := q.InsertClick(ctx, aClick(ctx, t, tx))
		if err != nil {
			t.Fatalf("InsertClick(): %v", err)
		}

		for _, attempt := range []struct{ name, sql string }{
			{"an edited rate", `update cashback.click set rate_snapshot = '{"kind":"percent","bps":9999}'::jsonb where id = $1`},
			{"a re-homed member", `update cashback.click set account_id = account_id where id = $1`},
			{"a deletion", `delete from cashback.click where id = $1`},
		} {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			_, err = sub.Exec(ctx, attempt.sql, click.ID)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.RaiseException {
				t.Errorf("%s = %v, want the immutability trigger to raise", attempt.name, err)
			}
			_ = sub.Rollback(ctx)
		}
	})
}
