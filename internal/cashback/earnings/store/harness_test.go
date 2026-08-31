package store_test

// The way into the schema that every suite in this package shares: one
// connection, one outer transaction, and a savepoint per case.
//
// One file rather than a copy per suite, because the alternative is two sets
// of fixtures drifting apart while both look right - and a fixture that has
// drifted asserts something nobody meant.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// schemaTx migrates, connects, and opens the outer transaction every case
// runs a savepoint inside. The whole suite rolls back, so a run leaves the
// database exactly as it found it.
func schemaTx(t *testing.T) (context.Context, pgx.Tx, func()) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise these statements")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin: %v", err)
	}
	return ctx, tx, func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	}
}

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

// world seeds everything a report and a click both need: a network, a
// publisher account, a member, and an offer to have been clicked.
func world(ctx context.Context, t *testing.T, tx pgx.Tx) (networkID string, publisher, member, offer pgtype.UUID) {
	t.Helper()
	id := tag(t)
	networkID = "earnfix_" + id

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Earnings Fixture Network', 'clickref', 31, 5, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.earnfix.credential', true)
		returning id`, networkID).Scan(&publisher); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Clicking Member', 'reader') returning id`,
		"member-"+id+"@example.test").Scan(&member); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	var merchant, route pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', 'de', 'active') returning id`, "earn-fixture-"+id).Scan(&merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('fixture', $1, $2, $3, now(), '{"id":"fixture"}'::jsonb, 'active', true) returning id`,
		merchant, networkID, "ext-"+id).Scan(&route); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_fixed_minor, currency, member_share_bps, valid_from, deeplink_template)
		values ($1, 'fixed', 250, 'EUR', 6000, now() - interval '1 day', 'https://example.test/deeplink?ref={ref}')
		returning id`, route).Scan(&offer); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}
	return networkID, publisher, member, offer
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
