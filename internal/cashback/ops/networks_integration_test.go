package ops_test

// The connected-networks read against the real schema (T228).
//
// The unit tests above prove the wire. Only this can prove the grouping,
// and the grouping is where a bug would hide: the query LEFT JOINs, so a
// network with no publisher account arrives as a row whose account half is
// entirely null, and one pass has to fold many rows into one network without
// either dropping that network or inventing an account for it.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// aNetwork seeds one network row, named uniquely so parallel cases inside
// one transaction cannot collide on the primary key.
func aNetwork(ctx context.Context, t *testing.T, tx pgx.Tx, active bool) string {
	t.Helper()
	id := "net" + uuid.NewString()[:8]
	if _, err := tx.Exec(ctx, `
		insert into cashback.network
			(id, display_name, click_ref_param, max_query_window_days,
			 rate_limit_per_minute, reporting_lag_minutes, active)
		values ($1, 'A Network', 'subid1', 31, 60, 240, $2)`, id, active); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	return id
}

// aPublisherAccount hangs one account off a network.
func aPublisherAccount(ctx context.Context, t *testing.T, tx pgx.Tx, network, publisher string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account
			(network_id, external_publisher_id, credential_ref, cursor_at,
			 trailing_cursor_at, backfill_from, reports_currency, active)
		values ($1, $2, $3, $4, $5, $6, 'EUR', true) returning id`,
		network, publisher, "config:networks."+network+".credential",
		time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
	).Scan(&id); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	return id
}

// find returns the network with this id from a read, or fails.
func find(t *testing.T, networks []ops.ConnectedNetwork, id string) ops.ConnectedNetwork {
	t.Helper()
	for _, network := range networks {
		if network.ID == id {
			return network
		}
	}
	t.Fatalf("network %q is not in the read; a seeded network must always be listed", id)
	return ops.ConnectedNetwork{}
}

// TestTheConnectedNetworksReadAgainstSchema.
func TestTheConnectedNetworksReadAgainstSchema(t *testing.T) {
	ctx, tx := destinationsTx(t)
	store, err := ops.NewPGStore(tx)
	if err != nil {
		t.Fatalf("building the store: %v", err)
	}

	t.Run("a network with two accounts is one network carrying both", func(t *testing.T) {
		network := aNetwork(ctx, t, tx, true)
		first := aPublisherAccount(ctx, t, tx, network, "AAA-1")
		second := aPublisherAccount(ctx, t, tx, network, "BBB-2")

		networks, err := store.ConnectedNetworks(ctx)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := find(t, networks, network)
		if len(got.Accounts) != 2 {
			t.Fatalf("accounts = %d, want 2 — the grouping dropped one", len(got.Accounts))
		}
		// Ordered by publisher, so the pair is stable between runs.
		if got.Accounts[0].ExternalPublisherID != "AAA-1" || got.Accounts[1].ExternalPublisherID != "BBB-2" {
			t.Errorf("accounts = %q, %q; want them ordered by publisher",
				got.Accounts[0].ExternalPublisherID, got.Accounts[1].ExternalPublisherID)
		}
		if got.Accounts[0].ID != first || got.Accounts[1].ID != second {
			t.Error("the account ids do not match what was seeded")
		}
		if got.ReportingLagMinutes != 240 {
			t.Errorf("reporting_lag_minutes = %d, want 240", got.ReportingLagMinutes)
		}
	})

	t.Run("a network nobody has connected is listed with no accounts", func(t *testing.T) {
		network := aNetwork(ctx, t, tx, false)

		networks, err := store.ConnectedNetworks(ctx)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := find(t, networks, network)
		if len(got.Accounts) != 0 {
			t.Fatalf("accounts = %d, want 0 — the LEFT JOIN's null row became an account", len(got.Accounts))
		}
		if got.Active {
			t.Error("the network reads as active; it was seeded inactive")
		}
	})

	t.Run("neither driver_shipped nor credential_present is answered here", func(t *testing.T) {
		network := aNetwork(ctx, t, tx, true)
		aPublisherAccount(ctx, t, tx, network, "CCC-3")

		networks, err := store.ConnectedNetworks(ctx)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := find(t, networks, network)
		// Both are facts about the build and the environment. The store
		// leaves them false and the composition root fills them in; a store
		// that guessed either would be a store deciding what this binary
		// ships.
		if got.DriverShipped {
			t.Error("the store answered driver_shipped; that is the composition root's to know")
		}
		if len(got.Accounts) == 1 && got.Accounts[0].CredentialPresent {
			t.Error("the store answered credential_present; that is the composition root's to know")
		}
	})

	t.Run("the credential ref travels and no credential could", func(t *testing.T) {
		network := aNetwork(ctx, t, tx, true)
		aPublisherAccount(ctx, t, tx, network, "DDD-4")

		networks, err := store.ConnectedNetworks(ctx)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := find(t, networks, network)
		if len(got.Accounts) != 1 {
			t.Fatalf("accounts = %d, want 1", len(got.Accounts))
		}
		// The column holds a KEY NAME by construction - ADR-0003 keeps
		// credentials out of the database entirely - so this is the whole
		// of what the read could ever expose.
		if want := "config:networks." + network + ".credential"; got.Accounts[0].CredentialRef != want {
			t.Errorf("credential_ref = %q, want %q", got.Accounts[0].CredentialRef, want)
		}
	})
}
