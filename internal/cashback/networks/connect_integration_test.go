// The tests for connect.go, against a real schema: that connecting is safe to
// run again, that a re-run does not undo an operator's edits or move a
// polling account's history floor, and that both rows carry the same
// activation.
//
// Against the real schema and not a fake, because every rule this file
// asserts is enforced by a check constraint or a unique index, and a fake
// would agree with the code rather than with Postgres.

package networks_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// connectTestTx migrates and opens a transaction the test throws away, or
// skips.
func connectTestTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise connecting an account")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return ctx, tx
}

// aNetworkDeclaration is one adapter's published facts, with an id of its
// own so two cases never meet. network.id is constrained to ^[a-z][a-z0-9_]*$.
func aNetworkDeclaration(t *testing.T) networks.Documented {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return networks.Documented{
		ID:                 networks.NetworkID("awin_" + hex.EncodeToString(suffix)),
		DisplayName:        "Awin",
		ClickRefParam:      "clickref",
		MaxQueryWindowDays: 31,
		RateLimitPerMinute: 20,
	}
}

// aConnectRequest is what an operator connecting an Awin account passes.
func aConnectRequest(t *testing.T) networks.ConnectRequest {
	t.Helper()
	return networks.ConnectRequest{
		Network:             aNetworkDeclaration(t),
		ExternalPublisherID: "123456",
		CredentialRef:       "NETWORK_FIXTURE_API_KEY",
		BackfillFrom:        time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
		Active:              true,
	}
}

func TestConnectingWritesBothRowsADeploymentNeeds(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	got, err := networks.ConnectPublisherAccount(ctx, tx, req)
	if err != nil {
		t.Fatalf("ConnectPublisherAccount(): %v", err)
	}
	if !got.NetworkCreated || !got.AccountCreated {
		t.Errorf("a first connect reported networkCreated=%v accountCreated=%v, want both", got.NetworkCreated, got.AccountCreated)
	}
	if got.Account.ExternalID() != req.ExternalPublisherID {
		t.Errorf("the account is %q, want %q", got.Account.ExternalID(), req.ExternalPublisherID)
	}
	if got.Account.Network() != req.Network.ID {
		t.Errorf("the account is at %q, want %q", got.Account.Network(), req.Network.ID)
	}
	if !got.BackfillFrom.Equal(req.BackfillFrom) {
		t.Errorf("BackfillFrom = %s, want %s", got.BackfillFrom, req.BackfillFrom)
	}

	// And the deployment can now find it, which is the whole point: this is
	// the lookup that logged "no publisher account is connected" forever.
	connected, err := networks.FindPublisherAccount(ctx, tx, req.Network.ID, req.ExternalPublisherID)
	if err != nil {
		t.Fatalf("the account this command just connected cannot be found: %v", err)
	}
	if !connected.Active {
		t.Errorf("the connected account is inactive, and the poller refuses one")
	}
	if !connected.BackfillFrom.Equal(req.BackfillFrom) {
		t.Errorf("the found account starts at %s, want %s", connected.BackfillFrom, req.BackfillFrom)
	}

	// The network row is active too. cashback.offer's read joins on
	// n.active, so an active account at an inactive network polls happily
	// while every offer on it is unclickable.
	var active bool
	if err := tx.QueryRow(ctx, `select active from cashback.network where id = $1`, req.Network.ID.String()).Scan(&active); err != nil {
		t.Fatalf("reading the network row: %v", err)
	}
	if !active {
		t.Errorf("the network row is inactive, so no member could click through any offer on it")
	}
}

func TestConnectingTwiceIsNotAnError(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}
	again, err := networks.ConnectPublisherAccount(ctx, tx, req)
	if err != nil {
		t.Fatalf("connecting the same account again failed: %v", err)
	}
	if again.NetworkCreated || again.AccountCreated {
		t.Errorf("a re-run reported networkCreated=%v accountCreated=%v, want neither", again.NetworkCreated, again.AccountCreated)
	}
}

// TestARerunDoesNotUndoAnOperatorsEdits is the failure that looks like
// nothing happened. The limits and the click-reference parameter are on the
// row precisely so they can be corrected without a release, and a connect
// that reset them to the documented defaults would silently put a deployment
// back on numbers an operator had already found to be wrong.
func TestARerunDoesNotUndoAnOperatorsEdits(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		update cashback.network
		   set rate_limit_per_minute = 60, max_query_window_days = 7, click_ref_param = 'pref1', display_name = 'Awin (raised)'
		 where id = $1`, req.Network.ID.String()); err != nil {
		t.Fatalf("the operator edit failed: %v", err)
	}

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the re-run failed: %v", err)
	}

	var (
		rate  int
		days  int
		param string
		name  string
	)
	if err := tx.QueryRow(ctx, `
		select rate_limit_per_minute, max_query_window_days, click_ref_param, display_name
		  from cashback.network where id = $1`, req.Network.ID.String()).Scan(&rate, &days, &param, &name); err != nil {
		t.Fatalf("reading the network row: %v", err)
	}
	if rate != 60 {
		t.Errorf("rate_limit_per_minute = %d, want the operator's 60", rate)
	}
	if days != 7 {
		t.Errorf("max_query_window_days = %d, want the operator's 7", days)
	}
	if param != "pref1" {
		t.Errorf("click_ref_param = %q, want the operator's %q", param, "pref1")
	}
	if name != "Awin (raised)" {
		t.Errorf("display_name = %q, want the operator's", name)
	}
}

// TestARerunNeverMovesAPollingAccountsHistoryFloor is 0023's warning made a
// test. The trailing re-read walks from backfill_from until its own cursor
// exists, about a hundred days later, and it is the only path from pending to
// confirmed. Moving that floor forward would leave the span between the old
// start and the new one never re-read, and every transaction in it pending
// forever - with nothing logged and nothing wrong anywhere.
func TestARerunNeverMovesAPollingAccountsHistoryFloor(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	first, err := networks.ConnectPublisherAccount(ctx, tx, req)
	if err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}

	moved := req
	moved.BackfillFrom = req.BackfillFrom.AddDate(0, 2, 0)
	again, err := networks.ConnectPublisherAccount(ctx, tx, moved)
	if err != nil {
		t.Fatalf("the re-run failed: %v", err)
	}
	if !again.BackfillFrom.Equal(first.BackfillFrom) {
		t.Errorf("the re-run moved the history floor to %s, want it left at %s", again.BackfillFrom, first.BackfillFrom)
	}
	// And it reports what the row actually says rather than what was asked
	// for, so an operator can see the flag did nothing.
	if again.BackfillFrom.Equal(moved.BackfillFrom) {
		t.Errorf("the re-run reported the requested floor rather than the row's")
	}
}

// TestTheSameCommandPausesANetwork: active is written on every run, which is
// what makes one operation both connect and pause. Without it an operator who
// needed to stop a network would be back to hand-written SQL, which is the
// state this command exists to remove.
func TestTheSameCommandPausesANetwork(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}

	paused := req
	paused.Active = false
	got, err := networks.ConnectPublisherAccount(ctx, tx, paused)
	if err != nil {
		t.Fatalf("pausing failed: %v", err)
	}
	if got.Active {
		t.Errorf("the account is still active after a run that asked for inactive")
	}

	var networkActive bool
	if err := tx.QueryRow(ctx, `select active from cashback.network where id = $1`, req.Network.ID.String()).Scan(&networkActive); err != nil {
		t.Fatalf("reading the network row: %v", err)
	}
	if networkActive {
		t.Errorf("the network row is still active, so every offer on it stays clickable")
	}
}

// TestConnectingNeverTouchesACursor: the cursors are the poller's, advanced
// only inside the transaction that persisted a window (FR-031). A connect
// that reset one would skip every window between.
func TestConnectingNeverTouchesACursor(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	first, err := networks.ConnectPublisherAccount(ctx, tx, req)
	if err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}

	cursor := req.BackfillFrom.AddDate(0, 1, 0)
	trailing := req.BackfillFrom.AddDate(0, 0, 5)
	if _, err := tx.Exec(ctx, `
		update cashback.network_account set cursor_at = $2, trailing_cursor_at = $3 where id = $1`,
		first.Account.ID(), cursor, trailing); err != nil {
		t.Fatalf("seeding the cursors: %v", err)
	}

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the re-run failed: %v", err)
	}

	var gotCursor, gotTrailing time.Time
	if err := tx.QueryRow(ctx, `
		select cursor_at, trailing_cursor_at from cashback.network_account where id = $1`,
		first.Account.ID()).Scan(&gotCursor, &gotTrailing); err != nil {
		t.Fatalf("reading the cursors: %v", err)
	}
	if !gotCursor.Equal(cursor) {
		t.Errorf("cursor_at = %s, want %s", gotCursor, cursor)
	}
	if !gotTrailing.Equal(trailing) {
		t.Errorf("trailing_cursor_at = %s, want %s", gotTrailing, trailing)
	}
}

// TestARerunCanMoveTheCredentialKey: credential_ref names WHERE a credential
// lives and is never the credential itself (ADR-0003), so moving an account
// to a different key is an ordinary re-run rather than a database edit.
func TestARerunCanMoveTheCredentialKey(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); err != nil {
		t.Fatalf("the first connect failed: %v", err)
	}

	moved := req
	moved.CredentialRef = "NETWORK_FIXTURE_API_KEY_SECONDARY"
	if _, err := networks.ConnectPublisherAccount(ctx, tx, moved); err != nil {
		t.Fatalf("the re-run failed: %v", err)
	}

	var ref string
	if err := tx.QueryRow(ctx, `
		select credential_ref from cashback.network_account
		 where network_id = $1 and external_publisher_id = $2`,
		req.Network.ID.String(), req.ExternalPublisherID).Scan(&ref); err != nil {
		t.Fatalf("reading the credential key: %v", err)
	}
	if ref != moved.CredentialRef {
		t.Errorf("credential_ref = %q, want %q", ref, moved.CredentialRef)
	}
}

func TestARequestThatCouldNotProduceAWorkingAccountIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil func(*networks.ConnectRequest)
	}{
		{name: "no publisher id", spoil: func(r *networks.ConnectRequest) { r.ExternalPublisherID = "  " }},
		{name: "no key naming where the credential lives", spoil: func(r *networks.ConnectRequest) { r.CredentialRef = "" }},
		{name: "a declaration the network table would refuse", spoil: func(r *networks.ConnectRequest) { r.Network.ClickRefParam = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := networks.ConnectRequest{
				Network: networks.Documented{
					ID: "awin", DisplayName: "Awin", ClickRefParam: "clickref",
					MaxQueryWindowDays: 31, RateLimitPerMinute: 20,
				},
				ExternalPublisherID: "123456",
				CredentialRef:       "NETWORK_FIXTURE_API_KEY",
				BackfillFrom:        time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
				Active:              true,
			}
			tt.spoil(&req)
			if err := req.Validate(); !errors.Is(err, networks.ErrCannotConnect) {
				t.Fatalf("Validate() = %v, want one wrapping ErrCannotConnect", err)
			}
		})
	}
}

// TestANewAccountMustSayWhereItsHistoryStarts, and a re-run need not. The
// first window has to start somewhere and nothing in the system can work out
// where (0023); an account that already has one keeps it, so pausing a
// network or moving a credential key does not have to restate a date the
// command could not change anyway.
func TestANewAccountMustSayWhereItsHistoryStarts(t *testing.T) {
	ctx, tx := connectTestTx(t)
	req := aConnectRequest(t)
	req.BackfillFrom = time.Time{}

	if _, err := networks.ConnectPublisherAccount(ctx, tx, req); !errors.Is(err, networks.ErrCannotConnect) {
		t.Fatalf("connecting a new account with no history start = %v, want one wrapping ErrCannotConnect", err)
	}

	started := req
	started.BackfillFrom = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	first, err := networks.ConnectPublisherAccount(ctx, tx, started)
	if err != nil {
		t.Fatalf("connecting with a history start failed: %v", err)
	}

	// And now it is not needed: this is the pause case.
	paused := req
	paused.Active = false
	again, err := networks.ConnectPublisherAccount(ctx, tx, paused)
	if err != nil {
		t.Fatalf("pausing an account without restating its history start failed: %v", err)
	}
	if !again.BackfillFrom.Equal(first.BackfillFrom) {
		t.Errorf("the re-run left the history floor at %s, want %s", again.BackfillFrom, first.BackfillFrom)
	}
	if again.Active {
		t.Errorf("the account is still active after a run that asked for inactive")
	}
}
