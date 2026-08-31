package clickout_test

// The click recorder against the real, migrated schema (T063).
//
// The unit tests above fake the store, so they can say what the package
// refuses but nothing about whether the database agrees. Two things only a
// real one can answer: that a promised band survives the jsonb round trip
// unchanged - it is what governs the credit years later (FR-013) - and that
// a re-used reference is reported as a taken reference rather than as some
// generic write failure, which depends on the constraint Postgres actually
// names.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// tag keeps one case's identifiers from colliding with another's.
func tag(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// clickable seeds a member and a live offer, and answers both ids.
func clickable(ctx context.Context, t *testing.T, tx pgx.Tx) (account, offer uuid.UUID) {
	t.Helper()
	id := tag(t)
	networkID := "clickfix_" + id

	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Clicking Member', 'reader') returning id`,
		"member-"+id+"@example.test").Scan(&account); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Click Fixture Network', 'clickref', 31, 300, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var merchant, route uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', 'de', 'active') returning id`, "click-fixture-"+id).Scan(&merchant); err != nil {
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
	return account, offer
}

func TestTheClickRecorderAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the click recorder")
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
	// Rolled back: a click row can never be deleted (C-3), so a case that
	// committed would be one every later run carries.
	defer func() { _ = tx.Rollback(ctx) }()

	each := func(name string, scenario func(t *testing.T, tx pgx.Tx, clicks *clickout.Clicks)) {
		t.Run(name, func(t *testing.T) {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			defer func() { _ = sub.Rollback(ctx) }()
			clicks, err := clickout.NewClicks(store.New(sub))
			if err != nil {
				t.Fatalf("NewClicks(): %v", err)
			}
			scenario(t, sub, clicks)
		})
	}

	// A FIXED band, deliberately: it is the kind whose snapshot carries an
	// amount and a currency, so a jsonb round trip that lost either would
	// show up here and nowhere in the percent case.
	fixed, err := money.New(250, "EUR")
	if err != nil {
		t.Fatalf("building the fixed rate: %v", err)
	}
	promised := clickout.Promise{
		Rate:        catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: fixed},
		MemberShare: 6000,
	}

	each("a recorded click is found again by the reference it was issued", func(t *testing.T, tx pgx.Tx, clicks *clickout.Clicks) {
		account, offer := clickable(ctx, t, tx)
		ref, err := clickout.NewMinter().Mint()
		if err != nil {
			t.Fatalf("Mint(): %v", err)
		}

		recorded, err := clicks.Record(ctx, clickout.NewClick{
			Ref: ref, AccountID: account, OfferID: offer, Promised: promised,
			Context: clickout.NewContextDigest("ua/1.0", "203.0.113.7"),
		})
		if err != nil {
			t.Fatalf("Record(): %v", err)
		}
		if recorded.ClickedAt.IsZero() {
			t.Error("the recorded click carries no instant; clicked_at is the row's own clock and is read back")
		}

		found, err := clicks.ByRef(ctx, networks.NewClickRef(ref.Ref()))
		if err != nil {
			t.Fatalf("ByRef(): %v", err)
		}
		if found.ID != recorded.ID || found.AccountID != account || found.OfferID != offer {
			t.Errorf("ByRef() = %+v, want the click just recorded %+v", found, recorded)
		}
		// The whole point of the snapshot: what the member was promised
		// comes back exactly, through jsonb, minor units and currency
		// intact (FR-013, C-6).
		if found.Promised != promised {
			t.Errorf("the promise reads %+v, want %+v", found.Promised, promised)
		}
		if !found.ClickedAt.Equal(recorded.ClickedAt) {
			t.Errorf("clicked_at reads %s, want %s", found.ClickedAt, recorded.ClickedAt)
		}
		if found.Context != recorded.Context {
			t.Errorf("the context digest reads %q, want %q", found.Context, recorded.Context)
		}
	})

	each("a reference already issued is refused as one", func(t *testing.T, tx pgx.Tx, clicks *clickout.Clicks) {
		account, offer := clickable(ctx, t, tx)
		ref, err := clickout.NewMinter().Mint()
		if err != nil {
			t.Fatalf("Mint(): %v", err)
		}
		first := clickout.NewClick{Ref: ref, AccountID: account, OfferID: offer, Promised: promised}
		if _, err := clicks.Record(ctx, first); err != nil {
			t.Fatalf("the first click: %v", err)
		}

		// The mapping the unit test can only assert against a synthetic
		// error: this is the constraint name Postgres itself reports.
		if _, err := clicks.Record(ctx, first); !errors.Is(err, clickout.ErrReferenceTaken) {
			t.Fatalf("recording the same reference twice = %v, want one wrapping %v", err, clickout.ErrReferenceTaken)
		}
	})

	each("a reference no click carries finds nothing", func(t *testing.T, _ pgx.Tx, clicks *clickout.Clicks) {
		ref, err := clickout.NewMinter().Mint()
		if err != nil {
			t.Fatalf("Mint(): %v", err)
		}
		// Ordinary rather than a failure: networks echo references from
		// other publishers and from stale links, and the caller queues that
		// transaction as unattributed (FR-034).
		if _, err := clicks.ByRef(ctx, networks.NewClickRef(ref.Ref())); !errors.Is(err, clickout.ErrNoSuchClick) {
			t.Fatalf("ByRef() for an unissued reference = %v, want one wrapping %v", err, clickout.ErrNoSuchClick)
		}
	})
}
