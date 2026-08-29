package store_test

// Exercises the generated catalogue store against the real, migrated
// schema. The point is not coverage of generated code - the coverage gate
// deliberately excludes internal/cashback/*/store/ - but proof that the
// query's definition of "live" matches the schema's: every leg of the
// chain (offer window, route status, merchant status, network flag) must
// hold, and any one of them failing hides the offer.
//
// Everything runs inside one transaction that is rolled back, so the suite
// leaves no rows behind and cases cannot see each other's chains.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// The store is wired against whatever can run its queries; both the pool
// and a transaction must satisfy that, checked here at compile time.
var (
	_ store.DBTX = (*pgxpool.Pool)(nil)
	_ store.DBTX = (pgx.Tx)(nil)
)

// offerChain describes one fully seeded network -> merchant -> route ->
// offer chain. The zero value would be rejected by the schema, so every
// case states its chain in full and the mutation under test is visible in
// the table rather than hidden in a helper.
type offerChain struct {
	networkActive  bool
	routeStatus    string
	merchantStatus string
	rateKind       string // "percent" or "fixed"
	validFrom      time.Duration
	validTo        time.Duration // zero means an open-ended band
}

func TestGetLiveOfferAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the catalogue store")
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

	queries := store.New(tx)
	at := time.Now().UTC()

	live := offerChain{
		networkActive:  true,
		routeStatus:    "active",
		merchantStatus: "active",
		rateKind:       "percent",
		validFrom:      -time.Hour,
	}

	t.Run("a live percent band is returned whole", func(t *testing.T) {
		id, suffix := seedOfferChain(t, tx, at, live)
		got, err := queries.GetLiveOffer(ctx, store.GetLiveOfferParams{ID: id, At: stamp(at)})
		if err != nil {
			t.Fatalf("GetLiveOffer: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %v, want %v", got.ID, id)
		}
		if got.RateKind != "percent" || !got.RateBps.Valid || got.RateBps.Int32 != 400 {
			t.Errorf("rate = (%q, %+v), want a percent band of 400 bps", got.RateKind, got.RateBps)
		}
		// A percent band carries no fixed amount and no currency (0011,
		// offer_rate_kind_fields); both must come back as SQL NULL.
		if got.RateFixedMinor.Valid || got.Currency.Valid {
			t.Errorf("percent band leaked fixed-rate fields: %+v / %+v", got.RateFixedMinor, got.Currency)
		}
		if got.MemberShareBps != 5000 {
			t.Errorf("MemberShareBps = %d, want 5000", got.MemberShareBps)
		}
		// The redirect's ingredients (FR-021): the network's identity and
		// click-reference parameter, and the band's deeplink template.
		if got.NetworkID != "livetest_"+suffix || got.ClickRefParam != "clickref" {
			t.Errorf("network = (%q, %q), want (livetest_%s, clickref)", got.NetworkID, got.ClickRefParam, suffix)
		}
		if got.MerchantSlug != "live-test-"+suffix {
			t.Errorf("MerchantSlug = %q, want live-test-%s", got.MerchantSlug, suffix)
		}
		if got.DeeplinkTemplate == "" {
			t.Error("DeeplinkTemplate came back empty")
		}
	})

	t.Run("a live fixed band keeps integer minor units and their currency", func(t *testing.T) {
		fixed := live
		fixed.rateKind = "fixed"
		id, _ := seedOfferChain(t, tx, at, fixed)
		got, err := queries.GetLiveOffer(ctx, store.GetLiveOfferParams{ID: id, At: stamp(at)})
		if err != nil {
			t.Fatalf("GetLiveOffer: %v", err)
		}
		// C-6: minor units as an integer, beside an explicit currency, and
		// no basis-points field pretending to apply.
		if !got.RateFixedMinor.Valid || got.RateFixedMinor.Int64 != 250 {
			t.Errorf("RateFixedMinor = %+v, want 250 minor units", got.RateFixedMinor)
		}
		if !got.Currency.Valid || got.Currency.String != "EUR" {
			t.Errorf("Currency = %+v, want EUR", got.Currency)
		}
		if got.RateBps.Valid {
			t.Errorf("fixed band leaked rate_bps: %+v", got.RateBps)
		}
	})

	// Every way a chain goes dead. Each case seeds its own full chain with
	// exactly one leg broken, and the offer must be absent - the endpoint
	// turns that absence into its 409, so a row here would be a click
	// issued against a band nobody publishes.
	dead := []struct {
		name  string
		chain func() offerChain
	}{
		{"an expired band", func() offerChain {
			c := live
			c.validFrom = -2 * time.Hour
			c.validTo = -time.Hour
			return c
		}},
		{"a band not yet in force", func() offerChain {
			c := live
			c.validFrom = time.Hour
			return c
		}},
		{"a paused route", func() offerChain {
			c := live
			c.routeStatus = "paused"
			return c
		}},
		{"a merchant that left", func() offerChain {
			c := live
			c.merchantStatus = "left_network"
			return c
		}},
		{"an inactive network", func() offerChain {
			c := live
			c.networkActive = false
			return c
		}},
	}
	for _, tc := range dead {
		t.Run(tc.name+" is not live", func(t *testing.T) {
			id, _ := seedOfferChain(t, tx, at, tc.chain())
			_, err := queries.GetLiveOffer(ctx, store.GetLiveOfferParams{ID: id, At: stamp(at)})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("GetLiveOffer error = %v, want pgx.ErrNoRows", err)
			}
		})
	}

	t.Run("a band with a future close is still live", func(t *testing.T) {
		closing := live
		closing.validTo = time.Hour
		id, _ := seedOfferChain(t, tx, at, closing)
		if _, err := queries.GetLiveOffer(ctx, store.GetLiveOfferParams{ID: id, At: stamp(at)}); err != nil {
			t.Fatalf("GetLiveOffer: %v", err)
		}
	})

	t.Run("an unknown offer id is absent", func(t *testing.T) {
		_, err := queries.GetLiveOffer(ctx, store.GetLiveOfferParams{
			ID: pgtype.UUID{Bytes: [16]byte{0xff}, Valid: true},
			At: stamp(at),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetLiveOffer error = %v, want pgx.ErrNoRows", err)
		}
	})
}

// seedOfferChain inserts one chain as described and returns the offer's id
// and the unique suffix its human-readable identifiers carry.
func seedOfferChain(t *testing.T, tx pgx.Tx, at time.Time, c offerChain) (pgtype.UUID, string) {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)

	networkID := "livetest_" + suffix
	if _, err := tx.Exec(ctx,
		`insert into cashback.network
		     (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		 values ($1, $2, 'clickref', 31, 5, $3)`,
		networkID, "Live Test Network "+suffix, c.networkActive); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	var merchantID pgtype.UUID
	if err := tx.QueryRow(ctx,
		`insert into cashback.merchant (slug, country, source_language_code, status)
		 values ($1, 'DE', 'de', $2) returning id`,
		"live-test-"+suffix, c.merchantStatus,
	).Scan(&merchantID); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}

	var routeID pgtype.UUID
	if err := tx.QueryRow(ctx,
		`insert into cashback.merchant_network
		     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		 values ('fixture', $1, $2, $3, now(), '{"id":"fixture"}'::jsonb, $4, true) returning id`,
		merchantID, networkID, "ext-"+suffix, c.routeStatus,
	).Scan(&routeID); err != nil {
		t.Fatalf("seed merchant_network: %v", err)
	}

	// Exactly the fields for the chosen rate kind, as
	// offer_rate_kind_fields demands; a nil validTo stays an open band.
	rateBps, rateFixedMinor, currency := any(int32(400)), any(nil), any(nil)
	if c.rateKind == "fixed" {
		rateBps, rateFixedMinor, currency = any(nil), any(int64(250)), any("EUR")
	}
	var validTo *time.Time
	if c.validTo != 0 {
		to := at.Add(c.validTo)
		validTo = &to
	}
	var offerID pgtype.UUID
	if err := tx.QueryRow(ctx,
		`insert into cashback.offer
		     (merchant_network_id, rate_kind, rate_bps, rate_fixed_minor, currency,
		      member_share_bps, valid_from, valid_to, deeplink_template)
		 values ($1, $2, $3, $4, $5, 5000, $6, $7, 'https://example.test/deeplink?ref={ref}')
		 returning id`,
		routeID, c.rateKind, rateBps, rateFixedMinor, currency, at.Add(c.validFrom), validTo,
	).Scan(&offerID); err != nil {
		t.Fatalf("seed offer: %v", err)
	}
	return offerID, suffix
}

// randomSuffix keeps every chain's unique identifiers (network id, merchant
// slug, external merchant id) from colliding across cases and runs, even
// though the transaction is rolled back either way.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("reading randomness: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

// stamp wraps a moment as the query parameter the generated code expects.
func stamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}
