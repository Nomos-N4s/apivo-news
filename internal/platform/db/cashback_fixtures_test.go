package db_test

// Shared fixtures for the cashback invariant suite. Every test here runs
// inside a transaction that is rolled back (beginTx), so the chain below is
// rebuilt per test and no case can see another's rows.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cashbackFixtures is a fully valid catalogue chain: network -> publisher
// account -> merchant (with copy and reach) -> route -> offer, plus a
// member account to click it. Illegal-write tests break one link at a time.
type cashbackFixtures struct {
	suffix            string
	accountID         string
	networkID         string
	networkAccountID  string
	merchantID        string
	merchantNetworkID string
	placeID           string
	offerID           string
}

func seedCashback(t *testing.T, tx pgx.Tx) cashbackFixtures {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)
	f := cashbackFixtures{suffix: suffix}

	// A member: an ordinary account, no role required to browse or click.
	err := tx.QueryRow(ctx,
		`insert into account (email, display_name) values ($1, $2) returning id`,
		"member-"+suffix+"@example.test", "Test Member "+suffix,
	).Scan(&f.accountID)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	f.networkID = "fixture_" + suffix
	_, err = tx.Exec(ctx,
		`insert into cashback.network
		     (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		 values ($1, $2, 'clickref', 31, 5, true)`,
		f.networkID, "Fixture Network "+suffix)
	if err != nil {
		t.Fatalf("seed network: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.network_account
		     (network_id, external_publisher_id, credential_ref, active)
		 values ($1, $2, $3, true) returning id`,
		f.networkID, "publisher-"+suffix, "network.fixture.credential",
	).Scan(&f.networkAccountID)
	if err != nil {
		t.Fatalf("seed network_account: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.merchant (slug, country, source_language_code)
		 values ($1, 'DE', 'de') returning id`,
		"merchant-"+suffix,
	).Scan(&f.merchantID)
	if err != nil {
		t.Fatalf("seed merchant: %v", err)
	}

	// The route: this retailer as reached through this network. A second
	// route through another network is what several tests add on top.
	err = tx.QueryRow(ctx,
		`insert into cashback.merchant_network
		     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, preferred)
		 values ('fixture', $1, $2, $3, now(), '{"id":"fixture"}'::jsonb, true) returning id`,
		f.merchantID, f.networkID, "ext-"+suffix,
	).Scan(&f.merchantNetworkID)
	if err != nil {
		t.Fatalf("seed merchant_network: %v", err)
	}

	_, err = tx.Exec(ctx,
		`insert into cashback.merchant_copy (merchant_id, language_code, name, summary)
		 values ($1, 'de', $2, 'Testhändler')`,
		f.merchantID, "Testhändler "+suffix)
	if err != nil {
		t.Fatalf("seed merchant_copy: %v", err)
	}

	// Munich: the alpha reference place seeded by 0002.
	if err := tx.QueryRow(ctx, `select id from place where slug = 'munich'`).Scan(&f.placeID); err != nil {
		t.Fatalf("reading the munich reference place: %v", err)
	}
	_, err = tx.Exec(ctx,
		`insert into cashback.merchant_place (merchant_id, place_id) values ($1, $2)`,
		f.merchantID, f.placeID)
	if err != nil {
		t.Fatalf("seed merchant_place: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.offer
		     (merchant_network_id, rate_kind, rate_bps, member_share_bps, conditions, deeplink_template)
		 values ($1, 'percent', 400, 5000, 'Excludes gift cards', 'https://example.test/deep?clickref={ref}')
		 returning id`,
		f.merchantNetworkID,
	).Scan(&f.offerID)
	if err != nil {
		t.Fatalf("seed offer: %v", err)
	}

	return f
}
