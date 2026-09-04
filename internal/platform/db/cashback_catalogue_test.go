package db_test

// These tests assert that the DATABASE, not the catalogue code, is what
// keeps a published rate band computable and a merchant's copy honest.
//
// The rate band matters more than it looks: it is snapshotted onto every
// click and governs the credit that follows (FR-013). A band that says
// "percent" and carries a currency, or "fixed" and carries none, is a rate
// nobody can turn into money - and by the time that is discovered, the
// clicks have already been issued against it.
//
// C-6 gets its own schema-wide assertion here rather than a per-column one:
// the invariant is that no fractional type EXISTS in the cashback schema,
// which is a question about the whole schema, asked of the catalog.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCashbackCatalogueRejectsIllegalWrites is the catalogue's rejection
// table: every case is a write the database itself must refuse.
func TestCashbackCatalogueRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "network with a blank display name",
			rule: "network",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute)
					 values ($1, '   ', 'clickref', 31, 300)`, "blank_"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "network reporting a negative lag, which would claim it reports the future",
			rule: "network",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, reporting_lag_minutes)
					 values ($1, 'Backwards', 'clickref', 31, 300, -1)`, "backwards_"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "network with an unusable identifier",
			rule: "network",
			write: func(ctx context.Context, tx pgx.Tx, _ cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute)
					 values ('Not An Id!', 'Unusable', 'clickref', 31, 300)`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "network with no click reference parameter",
			rule: "network",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Without it the redirect carries no attribution and every
				// click through this network is lost (FR-021).
				_, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute)
					 values ($1, 'Nameless', '  ', 31, 300)`, "noref_"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "network with a zero query window",
			rule: "network",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute)
					 values ($1, 'Stalled', 'clickref', 0, 300)`, "stalled_"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "publisher account with a blank credential reference",
			rule: "ADR-0003",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_account (network_id, external_publisher_id, credential_ref)
					 values ($1, $2, '   ')`, f.networkID, "other-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "trailing cursor ahead of the main cursor",
			rule: "FR-031",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Ahead means a restart advanced the wrong cursor, which is
				// exactly how a transaction window gets skipped.
				_, err := tx.Exec(ctx,
					`update cashback.network_account
					    set cursor_at = now() - interval '10 days', trailing_cursor_at = now()
					  where id = $1`, f.networkAccountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "trailing cursor with no main cursor",
			rule: "FR-031",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A re-read of history that was never read: the trailing
				// cursor walks a window the main cursor has already passed,
				// so it cannot exist before the main cursor does.
				_, err := tx.Exec(ctx,
					`update cashback.network_account
					    set cursor_at = null, trailing_cursor_at = now() - interval '10 days'
					  where id = $1`, f.networkAccountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "the same publisher account twice at one network",
			rule: "FR-031",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Two rows would mean two independent cursors over one
				// transaction stream: guaranteed double counting.
				_, err := tx.Exec(ctx,
					`insert into cashback.network_account (network_id, external_publisher_id, credential_ref)
					 values ($1, $2, 'network.fixture.credential')`, f.networkID, "publisher-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "merchant with an unroutable slug",
			rule: "catalogue",
			write: func(ctx context.Context, tx pgx.Tx, _ cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant (slug, source_language_code) values ('Not A Slug', 'de')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "two merchants on one slug",
			rule: "catalogue",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant (slug, source_language_code) values ($1, 'de')`, "merchant-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "merchant in an unknown status",
			rule: "catalogue",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant (slug, source_language_code, status) values ($1, 'de', 'deleted')`, "gone-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "route with no imported payload",
			rule: "FR-012",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The provenance of a catalogue entry belongs to the route
				// it came in through, and it is not optional.
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ('fixture', $1, $2, $3, now(), null)`, f.merchantID, f.networkID, "bare-"+f.suffix)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "route with no identifier at the network",
			rule: "FR-012",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ('fixture', $1, $2, '   ', now(), '{}'::jsonb)`, f.merchantID, f.networkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "one network identifier claimed by two merchants",
			rule: "catalogue",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				var other string
				if err := tx.QueryRow(ctx,
					`insert into cashback.merchant (slug, source_language_code) values ($1, 'de') returning id`,
					"rival-"+f.suffix).Scan(&other); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ('fixture', $1, $2, $3, now(), '{}'::jsonb)`, other, f.networkID, "ext-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "two routes to one merchant through one network",
			rule: "catalogue",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Two rows would be two cursors over the same commission
				// stream for the same retailer.
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ('fixture', $1, $2, $3, now(), '{}'::jsonb)`, f.merchantID, f.networkID, "second-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "route with no brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Merchant availability is one of the four records ADR-0004
				// names, and a route whose brand nobody stated is a route
				// nobody can scope later.
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ($1, $2, $3, now(), '{}'::jsonb)`, f.merchantID, f.networkID, "brandless-"+f.suffix)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "route with a blank brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
					 values ('  ', $1, $2, $3, now(), '{}'::jsonb)`, f.merchantID, f.networkID, "blankbrand-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "a second preferred route for one merchant",
			rule: "arbitration",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A retailer live on two networks needs exactly one answer
				// to "which route do we publish". Two preferred routes is
				// that answer becoming whichever code path runs first.
				otherNetwork := "rival_" + f.suffix
				if _, err := tx.Exec(ctx,
					`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute)
					 values ($1, 'Rival Network', 'clickref', 31, 300)`, otherNetwork); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_network
					     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, preferred)
					 values ('fixture', $1, $2, $3, now(), '{}'::jsonb, true)`,
					f.merchantID, otherNetwork, "ext-rival-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "merchant whose source language does not exist",
			rule: "constitution VII",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A combined locale tag is not a language: 0001 rejects it
				// from the language table, so it can never be referenced.
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant (slug, source_language_code) values ($1, 'el-DE')`, "combined-"+f.suffix)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "merchant copy with a blank name",
			rule: "US5",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A blank name is exactly the failure US5 scenario 2 forbids:
				// never a blank, never a machine-invented name.
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_copy (merchant_id, language_code, name)
					 values ($1, 'el', '   ')`, f.merchantID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "two copies of one merchant in one language",
			rule: "US5",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_copy (merchant_id, language_code, name)
					 values ($1, 'de', 'Zweiter Name')`, f.merchantID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "merchant scoped to a place that does not exist",
			rule: "constitution VII",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.merchant_place (merchant_id, place_id)
					 values ($1, gen_random_uuid())`, f.merchantID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "percent band carrying a currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, currency, member_share_bps, deeplink_template)
					 values ($1, 'percent', 400, 'EUR', 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "percent band with no rate",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, member_share_bps, deeplink_template)
					 values ($1, 'percent', 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "fixed band with no currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A fixed amount with no currency is not money (C-6).
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_fixed_minor, member_share_bps, deeplink_template)
					 values ($1, 'fixed', 500, 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "fixed band also carrying a percentage",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_fixed_minor, currency, rate_bps, member_share_bps, deeplink_template)
					 values ($1, 'fixed', 500, 'EUR', 400, 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "band with a lowercase currency code",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_fixed_minor, currency, member_share_bps, deeplink_template)
					 values ($1, 'fixed', 500, 'eur', 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "rate above one hundred percent",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, deeplink_template)
					 values ($1, 'percent', 10001, 5000, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "member share above one hundred percent",
			rule: "Q4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Paying out more than the commission received is not a
				// generous share, it is a loss the schema will not represent.
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, deeplink_template)
					 values ($1, 'percent', 400, 10001, 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "band that expires before it starts",
			rule: "FR-013",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, valid_to, deeplink_template)
					 values ($1, 'percent', 400, 5000, now(), now() - interval '1 day', 'https://example.test/d')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "band with no deeplink to redirect to",
			rule: "FR-021",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, deeplink_template)
					 values ($1, 'percent', 400, 5000, '   ')`,
					f.merchantNetworkID)
				return err
			},
			wantCode: codeCheckViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashback(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, f), tt.wantCode)
		})
	}
}

// TestCashbackCatalogueAcceptsTheLegalPath is the positive control: without
// it the rejection table above could be satisfied by a schema that refuses
// everything.
func TestCashbackCatalogueAcceptsTheLegalPath(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashback(t, tx)

	// Zero is the ORDINARY value on reporting_lag_minutes, unlike the two
	// bounds beside it which are constrained above zero. A network with no
	// reporting lag answers up to the moment, which is what every row meant
	// before the column existed. Asserted here so that tightening the check
	// to `> 0` - the obvious mistake, made by matching the pattern of its
	// neighbours - fails rather than silently shortening every window.
	for _, lag := range []int{0, 90} {
		if _, err := tx.Exec(ctx,
			`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, reporting_lag_minutes)
			 values ($1, 'Lagging', 'clickref', 31, 300, $2)`,
			fmt.Sprintf("lag%d_%s", lag, f.suffix), lag); err != nil {
			t.Fatalf("a reporting lag of %d minutes was refused, and it is a legal declaration: %v", lag, err)
		}
	}

	// A second rate band on the same route: rates vary by category, and
	// every published band must be representable (US5 scenario 3).
	var fixedOffer string
	err := tx.QueryRow(ctx,
		`insert into cashback.offer
		     (merchant_network_id, rate_kind, rate_fixed_minor, currency, member_share_bps, conditions, exclusions, deeplink_template)
		 values ($1, 'fixed', 750, 'EUR', 5000, 'New customers only', 'Excludes electronics', 'https://example.test/d')
		 returning id`,
		f.merchantNetworkID).Scan(&fixedOffer)
	if err != nil {
		t.Fatalf("a valid fixed rate band was rejected: %v", err)
	}

	// The same retailer, reached through a second network, at a different
	// rate. This is the ordinary case at real catalogue size, not an edge
	// one: it must be representable without a second merchant row.
	secondNetwork := "second_" + f.suffix
	if _, err := tx.Exec(ctx,
		`insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		 values ($1, 'Second Network', 'ref', 45, 180, true)`, secondNetwork); err != nil {
		t.Fatalf("a second network was rejected: %v", err)
	}
	var secondRoute string
	if err := tx.QueryRow(ctx,
		`insert into cashback.merchant_network
		     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload)
		 values ('fixture', $1, $2, $3, now(), '{"id":"second"}'::jsonb) returning id`,
		f.merchantID, secondNetwork, "ext-second-"+f.suffix).Scan(&secondRoute); err != nil {
		t.Fatalf("a second route to the merchant was rejected: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, deeplink_template)
		 values ($1, 'percent', 550, 5000, 'https://example.test/second')`, secondRoute); err != nil {
		t.Fatalf("a rate band on the second route was rejected: %v", err)
	}

	// Copy in a second language, and reach into a second place: the two
	// axes move independently (constitution VII).
	if _, err := tx.Exec(ctx,
		`insert into cashback.merchant_copy (merchant_id, language_code, name, terms)
		 values ($1, 'el', $2, 'Όροι')`, f.merchantID, "Δοκιμαστικός έμπορος "+f.suffix); err != nil {
		t.Fatalf("Greek merchant copy was rejected: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into cashback.merchant_place (merchant_id, place_id)
		 select $1, id from place where slug = 'germany'`, f.merchantID); err != nil {
		t.Fatalf("a second place for the merchant was rejected: %v", err)
	}

	var languages, places, routes, offers, preferred int
	err = tx.QueryRow(ctx,
		`select (select count(*) from cashback.merchant_copy where merchant_id = $1),
		        (select count(*) from cashback.merchant_place where merchant_id = $1),
		        (select count(*) from cashback.merchant_network where merchant_id = $1),
		        (select count(*) from cashback.offer o
		           join cashback.merchant_network mn on mn.id = o.merchant_network_id
		          where mn.merchant_id = $1),
		        (select count(*) from cashback.merchant_network where merchant_id = $1 and preferred)`,
		f.merchantID).Scan(&languages, &places, &routes, &offers, &preferred)
	if err != nil {
		t.Fatalf("counting the catalogue: %v", err)
	}
	if languages != 2 || places != 2 || routes != 2 || offers != 3 {
		t.Fatalf("catalogue after the legal path: %d languages, %d places, %d routes, %d bands; want 2, 2, 2, 3",
			languages, places, routes, offers)
	}
	if preferred != 1 {
		t.Fatalf("%d preferred routes for one merchant, want exactly 1: arbitration must have a single answer", preferred)
	}
}

// TestCurrentOfferQueryUsesTheValidityIndex proves the index change rather
// than asserting it. An index never changes what a query returns, only what
// it costs, so the only way to show the fix works is to read the plan: with
// sequential scans disabled the planner must reach for
// offer_validity_window_idx for the "bands in force at this moment" query,
// including for a band that is current but has a future valid_to.
//
// The superseded partial index (`where valid_to is null`) fails this: it
// cannot answer for a band that closes later, which is exactly the case the
// old comment claimed to cover and did not.
func TestCurrentOfferQueryUsesTheValidityIndex(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashback(t, tx)

	// A band that is current and closes next month - invisible to an index
	// partial on `valid_to is null`.
	if _, err := tx.Exec(ctx,
		`insert into cashback.offer (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, valid_to, deeplink_template)
		 values ($1, 'percent', 600, 5000, now() - interval '1 day', now() + interval '30 days', 'https://example.test/closing')`,
		f.merchantNetworkID); err != nil {
		t.Fatalf("a closing rate band was rejected: %v", err)
	}

	// A route's history, because on three rows every index costs the same
	// and the planner's choice between them is a coin toss - it would name
	// offer_merchant_network_idx, whose leading column is the same, and this
	// test would be asserting which side the coin landed on. With a history
	// the two stop tying: the equality-only index returns the whole route
	// and filters, and the validity index answers the window from the index
	// itself. That difference IS the thing being tested.
	if _, err := tx.Exec(ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, valid_to, deeplink_template)
		select $1, 'percent', 100, 5000,
		       now() - interval '400 days' + (g || ' days')::interval,
		       now() - interval '399 days' + (g || ' days')::interval,
		       'https://example.test/expired'
		  from generate_series(1, 300) g`, f.merchantNetworkID); err != nil {
		t.Fatalf("seeding the route's history: %v", err)
	}
	// Analysed here so the plan below is decided against statistics this
	// test wrote, rather than against whatever a shared database happens to
	// have been analysed to. ANALYZE sees this transaction's own rows and is
	// rolled back with them.
	if _, err := tx.Exec(ctx, `analyze cashback.offer`); err != nil {
		t.Fatalf("analysing the offers: %v", err)
	}

	const currentBands = `select id from cashback.offer
	                       where merchant_network_id = $1
	                         and valid_from <= now()
	                         and coalesce(valid_to, 'infinity'::timestamptz) > now()`

	var found int
	if err := tx.QueryRow(ctx, `select count(*) from (`+currentBands+`) o`, f.merchantNetworkID).Scan(&found); err != nil {
		t.Fatalf("reading the bands in force: %v", err)
	}
	if found != 2 {
		t.Fatalf("%d bands in force, want 2 - the open-ended one and the one that closes next month", found)
	}

	// enable_seqscan off makes the planner state its preference: on a table
	// this small a scan is cheapest whatever indexes exist, so without this
	// the assertion would pass for any index at all - including none.
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatalf("disabling sequential scans: %v", err)
	}

	rows, err := tx.Query(ctx, `explain `+currentBands, f.merchantNetworkID)
	if err != nil {
		t.Fatalf("explaining the catalogue query: %v", err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning the plan: %v", err)
		}
		plan += line + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating the plan: %v", err)
	}
	if !strings.Contains(plan, "offer_validity_window_idx") {
		t.Fatalf("the bands-in-force query does not use offer_validity_window_idx; plan was:\n%s", plan)
	}
	// And answers the window FROM THE INDEX. An index that were only used
	// for the equality, with valid_from and valid_to left to a Filter, would
	// satisfy the check above while reading the route's whole history - which
	// is exactly what the index exists to avoid.
	if strings.Contains(plan, "Filter:") {
		t.Errorf("the bands-in-force query filters rows the index should have excluded; plan was:\n%s", plan)
	}
}

// TestNoFractionalMoneyTypeExistsInTheCashbackSchema asserts C-6 as the
// invariant map states it: not "this column is a bigint" but "no numeric,
// real, double precision or money column EXISTS here". Asked of the
// catalog, so it keeps answering for every table a later migration adds.
func TestNoFractionalMoneyTypeExistsInTheCashbackSchema(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)

	rows, err := tx.Query(context.Background(),
		`select c.relname, a.attname, t.typname
		   from pg_attribute a
		   join pg_class c on c.oid = a.attrelid
		   join pg_namespace n on n.oid = c.relnamespace
		   join pg_type t on t.oid = a.atttypid
		  where n.nspname = 'cashback'
		    and c.relkind in ('r', 'p', 'v', 'm')
		    and a.attnum > 0
		    and not a.attisdropped
		    and t.typname in ('numeric', 'float4', 'float8', 'money')
		  order by c.relname, a.attname`)
	if err != nil {
		t.Fatalf("reading pg_attribute: %v", err)
	}
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, column, typeName string
		if err := rows.Scan(&table, &column, &typeName); err != nil {
			t.Fatalf("scanning pg_attribute: %v", err)
		}
		offenders = append(offenders, "cashback."+table+"."+column+" is "+typeName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating pg_attribute: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("C-6 violated: fractional types in the cashback schema: %v", offenders)
	}
}

// TestNoForeignKeyLeavesTheCashbackSchemaForANewsTable asserts the
// constitution's Products rule at the catalog level: cashback may reference
// its own tables and the shared reference data, and nothing else. Asked of
// pg_constraint rather than of the migration text, so it also catches a key
// added by a hand-run ALTER in an environment.
func TestNoForeignKeyLeavesTheCashbackSchemaForANewsTable(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)

	rows, err := tx.Query(context.Background(),
		`select con.conname, tn.nspname || '.' || tc.relname
		   from pg_constraint con
		   join pg_class src on src.oid = con.conrelid
		   join pg_namespace sn on sn.oid = src.relnamespace
		   join pg_class tc on tc.oid = con.confrelid
		   join pg_namespace tn on tn.oid = tc.relnamespace
		  where con.contype = 'f'
		    and sn.nspname = 'cashback'
		    and not (
		        tn.nspname = 'cashback'
		        or (tn.nspname = 'public' and tc.relname in ('account', 'place', 'language'))
		    )
		  order by con.conname`)
	if err != nil {
		t.Fatalf("reading pg_constraint: %v", err)
	}
	defer rows.Close()

	var crossings []string
	for rows.Next() {
		var name, target string
		if err := rows.Scan(&name, &target); err != nil {
			t.Fatalf("scanning pg_constraint: %v", err)
		}
		crossings = append(crossings, name+" -> "+target)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating pg_constraint: %v", err)
	}
	if len(crossings) > 0 {
		t.Fatalf("a foreign key crosses the product boundary: %v", crossings)
	}
}
