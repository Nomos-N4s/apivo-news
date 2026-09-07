package db_test

// 0037 (spec 004, FR-096): a click carries the route and the network it was
// issued through, and the schema holds both to the offer by key. The two
// columns are denormalised - the same answer the join through the offer
// gives - and what makes the copy trustworthy is that a disagreeing pair is
// unrepresentable, not that the code writing it is careful.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAClickCarriesTheNetworkThatIssuedIt(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashback(t, tx)

	// A second network, and the same retailer reached through it: a real
	// route, on a real network, that the fixture's offer is simply not on.
	otherNetwork := "other_" + f.suffix
	if _, err := tx.Exec(ctx,
		`insert into cashback.network
		     (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		 values ($1, 'The Other Network', 'ref', 31, 300, true)`, otherNetwork); err != nil {
		t.Fatalf("seed the other network: %v", err)
	}
	var otherRoute string
	if err := tx.QueryRow(ctx,
		`insert into cashback.merchant_network
		     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, preferred)
		 values ('fixture', $1, $2, $3, now(), '{"id":"other"}'::jsonb, false) returning id`,
		f.merchantID, otherNetwork, "ext-other-"+f.suffix).Scan(&otherRoute); err != nil {
		t.Fatalf("seed the other route: %v", err)
	}

	insert := func(sp pgx.Tx, route, network any) error {
		_, err := sp.Exec(ctx,
			`insert into cashback.click
			     (click_ref, account_id, offer_id, merchant_network_id, network_id, rate_snapshot, member_share_bps_snapshot)
			 values ($1, $2, $3, $4, $5, '{}'::jsonb, 5000)`,
			randomSuffix(t)+randomSuffix(t), f.accountID, f.offerID, route, network)
		return err
	}

	cases := []struct {
		name           string
		route, network any
		wantCode       string
		wantConstraint string
	}{
		{
			// The route is the offer's; the network is not the route's.
			name: "a network the route does not belong to", route: f.merchantNetworkID, network: otherNetwork,
			wantCode: pgerrcode.ForeignKeyViolation, wantConstraint: "click_network_matches_route",
		},
		{
			// The pair is consistent with each other - that route IS on
			// that network - but the route is not the offer's.
			name: "a route the offer is not on", route: otherRoute, network: otherNetwork,
			wantCode: pgerrcode.ForeignKeyViolation, wantConstraint: "click_route_matches_offer",
		},
		{
			name: "no network at all", route: f.merchantNetworkID, network: nil,
			wantCode: pgerrcode.NotNullViolation,
		},
		{
			name: "no route at all", route: nil, network: f.networkID,
			wantCode: pgerrcode.NotNullViolation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			var pgErr *pgconn.PgError
			err := refused(ctx, tx, func(sp pgx.Tx) error { return insert(sp, tc.route, tc.network) })
			if !errors.As(err, &pgErr) || pgErr.Code != tc.wantCode {
				t.Fatalf("error = %v, want SQLSTATE %s", err, tc.wantCode)
			}
			if tc.wantConstraint != "" && pgErr.ConstraintName != tc.wantConstraint {
				t.Errorf("refused by %q, want %q", pgErr.ConstraintName, tc.wantConstraint)
			}
		})
	}

	t.Run("the offer's own route on its own network is accepted", func(t *testing.T) {
		if err := insert(tx, f.merchantNetworkID, f.networkID); err != nil {
			t.Fatalf("a click on the route and network its offer is on was refused: %v", err)
		}
	})

	// The backfill's promise, as the catalog now states it: neither column
	// may be null, so a row written before 0037 - or by anything other than
	// the click-out - is one the keys above have already judged.
	t.Run("neither column is nullable", func(t *testing.T) {
		rows, err := tx.Query(ctx,
			`select column_name, is_nullable from information_schema.columns
			  where table_schema = 'cashback' and table_name = 'click'
			    and column_name in ('merchant_network_id', 'network_id')`)
		if err != nil {
			t.Fatalf("reading the catalog: %v", err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var column, nullable string
			if err := rows.Scan(&column, &nullable); err != nil {
				t.Fatalf("scanning the catalog: %v", err)
			}
			seen++
			if nullable != "NO" {
				t.Errorf("cashback.click.%s is nullable", column)
			}
		}
		if seen != 2 {
			t.Errorf("found %d of the two columns 0037 adds", seen)
		}
		var unfilled int
		if err := tx.QueryRow(ctx,
			`select count(*) from cashback.click where merchant_network_id is null or network_id is null`).Scan(&unfilled); err != nil {
			t.Fatalf("counting unfilled clicks: %v", err)
		}
		if unfilled != 0 {
			t.Errorf("%d click(s) carry no route or no network after the backfill", unfilled)
		}
	})
}
