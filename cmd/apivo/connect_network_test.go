package main

// The tests for connect_network.go, driven through run() the way an operator
// drives it: the argv, the environment, and what is printed. What the write
// itself does is the networks package's own contract test; what is proved
// here is that the command reaches it with the deployment's own network and
// account, and that a re-run reads as a re-run.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/awin"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// connectEnv is the smallest environment the command needs: the cashback
// product on, and the network and account it would connect.
func connectEnv(dbURL, driver, accountID string) map[string]string {
	env := cashbackEnv(dbURL)
	// The list, and the block named after the one entry in it. cashbackEnv
	// already configures the fixture, so a different driver replaces both -
	// leaving its block behind would leave a block NETWORKS does not name,
	// which nothing reads.
	env["NETWORKS"] = driver
	delete(env, "NETWORK_FIXTURE_ACCOUNT_ID")
	prefix := "NETWORK_" + strings.ToUpper(driver) + "_"
	env[prefix+"ACCOUNT_ID"] = accountID
	if driver != "fixture" {
		// Every adapter but the fixture needs a credential before the
		// command will connect an account at all; the value never leaves
		// the environment.
		env[prefix+"API_KEY"] = "not-a-real-token"
	}
	return env
}

// connectCommandDatabase is the name of the scratch database these tests
// run against, and the reason they have one at all.
//
// Every other integration test in this repository works inside a transaction
// it rolls back. This command cannot: committing is the whole of what it
// does - it exists to write rows a deployment then reads in another process -
// so a test that drove it against the shared database would leave a
// cashback.network row behind. That row is keyed by the driver name, so it is
// the SAME row the poller tests seed, and the first one to run after this
// package would fail on a duplicate key. A database of its own is the only
// arrangement in which what this command does is harmless.
const connectCommandDatabase = "apivo_connect_network_cmd"

var (
	connectDBOnce sync.Once
	connectDBURL  string
	connectDBErr  error
)

// connectTestDB skips unless a database is reachable, creates the scratch
// database if it is not there, migrates it, and hands back its URL and a pool
// for checking what the command wrote.
//
// The database is created once and left behind rather than dropped: it is
// named, so a second run reuses it, and the alternative - dropping it from a
// cleanup - races every other test in this package that is still using it.
func connectTestDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise connect-network")
	}

	connectDBOnce.Do(func() { connectDBURL, connectDBErr = ensureScratchDatabase(base, connectCommandDatabase) })
	if connectDBErr != nil {
		t.Fatalf("preparing the scratch database: %v", connectDBErr)
	}

	pool, err := pgxpool.New(context.Background(), connectDBURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return connectDBURL, pool
}

// ensureScratchDatabase creates a scratch database beside the one
// DATABASE_URL names and migrates it. Creating one that is already there is
// not an error worth failing on, which is what makes a second run cheap.
//
// Named rather than fixed, because every command in this package that
// COMMITS needs one of these and they must not share: two commands writing
// committed rows into one database is the collision each of them has its own
// database to avoid.
func ensureScratchDatabase(base, name string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		return "", err
	}
	defer admin.Close()
	// create database cannot run inside a transaction and has no IF NOT
	// EXISTS, so an already-created one comes back as an error. It is not
	// fatal on its own - the migrate below is what proves the database is
	// usable - but it is carried into that failure, because "database does
	// not exist" and "the role may not create one" are different problems
	// and only one of them is fixed by re-running.
	_, createErr := admin.Exec(ctx, `create database "`+name+`"`)

	scratch := *parsed
	scratch.Path = "/" + name
	scratchURL := scratch.String()
	if err := platformdb.Migrate(scratchURL); err != nil {
		if createErr != nil {
			return "", fmt.Errorf("migrating %s: %w (creating it said: %w)", name, err, createErr)
		}
		return "", err
	}
	return scratchURL, nil
}

// remakeScratchDatabase is ensureScratchDatabase for a command whose rows
// must not survive a run: it drops the database first, then makes it again.
//
// connect-network does NOT want this - its own database is named, reused,
// and each case tidies up after itself - but a command that writes rows it
// then asserts the SHAPE of does. Left standing, the second run finds the
// first run's rows already there, takes every "already there" branch, and
// asserts on rows no code in that process wrote. The drop happens inside
// the caller's sync.Once, before any test has a connection open.
func remakeScratchDatabase(base, name string) (string, error) {
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		return "", err
	}
	if _, err := admin.Exec(ctx, `drop database if exists "`+name+`" with (force)`); err != nil {
		admin.Close()
		return "", fmt.Errorf("dropping %s: %w", name, err)
	}
	admin.Close()
	return ensureScratchDatabase(base, name)
}

// forgetAccount removes what a case wrote. The rows are committed - that is
// the point of the command - so each case tidies up after itself even inside
// its own database, or a re-run would find its first connect already done.
func forgetAccount(t *testing.T, pool *pgxpool.Pool, accountID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`delete from cashback.network_account where external_publisher_id = $1`, accountID); err != nil {
		t.Fatalf("cleaning up the account: %v", err)
	}
}

func TestConnectNetworkWritesTheRowsADeploymentThenReads(t *testing.T) {
	dbURL, pool := connectTestDB(t)
	const driver, accountID = "fixture", "connect-cmd-first"
	t.Cleanup(func() { forgetAccount(t, pool, accountID) })

	var out bytes.Buffer
	err := run(context.Background(),
		[]string{"connect-network", "-backfill-from", "2026-06-01"},
		envFrom(connectEnv(dbURL, driver, accountID)), &out)
	if err != nil {
		t.Fatalf("connect-network: %v", err)
	}

	printed := out.String()
	for _, want := range []string{accountID, driver, "active", "2026-06-01T00:00:00Z", "NETWORK_FIXTURE_API_KEY"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the command printed %q, which does not mention %q", printed, want)
		}
	}

	var active bool
	var external string
	if err := pool.QueryRow(context.Background(), `
		select external_publisher_id, active from cashback.network_account
		 where network_id = $1 and external_publisher_id = $2`, driver, accountID).Scan(&external, &active); err != nil {
		t.Fatalf("the row the command reported is not there: %v", err)
	}
	if !active {
		t.Errorf("the account is inactive, and the poller refuses one")
	}
}

// TestConnectNetworkIsSafeToRunAgain: an operator who cannot re-run a
// command runs a DELETE first, which is the thing this command exists to
// stop. The second run must also read as a second run.
func TestConnectNetworkIsSafeToRunAgain(t *testing.T) {
	dbURL, pool := connectTestDB(t)
	const driver, accountID = "fixture", "connect-cmd-again"
	t.Cleanup(func() { forgetAccount(t, pool, accountID) })

	env := envFrom(connectEnv(dbURL, driver, accountID))
	if err := run(context.Background(), []string{"connect-network", "-backfill-from", "2026-06-01"}, env, io.Discard); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"connect-network", "-backfill-from", "2026-06-01"}, env, &out); err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	if printed := out.String(); !strings.Contains(printed, "already there") {
		t.Errorf("a re-run printed %q, which reads like a first connect", printed)
	}
}

// TestConnectNetworkSaysWhenTheHistoryStartDidNotMove is the one place what
// was asked for and what the row says differ, and an operator who typed a
// date must not be left assuming it was applied.
func TestConnectNetworkSaysWhenTheHistoryStartDidNotMove(t *testing.T) {
	dbURL, pool := connectTestDB(t)
	const driver, accountID = "fixture", "connect-cmd-floor"
	t.Cleanup(func() { forgetAccount(t, pool, accountID) })

	env := envFrom(connectEnv(dbURL, driver, accountID))
	if err := run(context.Background(), []string{"connect-network", "-backfill-from", "2026-06-01"}, env, io.Discard); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"connect-network", "-backfill-from", "2026-08-01"}, env, &out); err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	printed := out.String()
	if !strings.Contains(printed, "NOT applied") {
		t.Errorf("the command printed %q, which does not say the date was ignored", printed)
	}
	if !strings.Contains(printed, "2026-06-01T00:00:00Z") {
		t.Errorf("the command printed %q, which does not say what the account actually polls from", printed)
	}
}

// TestConnectNetworkCanPauseWithoutRestatingTheHistoryStart: -inactive is
// how a network is turned off, and it must not require a date the command
// would refuse to apply anyway.
func TestConnectNetworkCanPauseWithoutRestatingTheHistoryStart(t *testing.T) {
	dbURL, pool := connectTestDB(t)
	const driver, accountID = "fixture", "connect-cmd-pause"
	t.Cleanup(func() { forgetAccount(t, pool, accountID) })

	env := envFrom(connectEnv(dbURL, driver, accountID))
	if err := run(context.Background(), []string{"connect-network", "-backfill-from", "2026-06-01"}, env, io.Discard); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"connect-network", "-inactive"}, env, &out); err != nil {
		t.Fatalf("pausing failed: %v", err)
	}
	if printed := out.String(); !strings.Contains(printed, "inactive") {
		t.Errorf("the command printed %q, which does not say the account is now inactive", printed)
	}

	var active bool
	if err := pool.QueryRow(context.Background(), `
		select active from cashback.network_account
		 where network_id = $1 and external_publisher_id = $2`, driver, accountID).Scan(&active); err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	if active {
		t.Errorf("the account is still active after -inactive")
	}
}

func TestConnectNetworkRefusesWhatItCannotConnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "a driver this binary has no adapter for",
			args: []string{"connect-network", "-backfill-from", "2026-06-01"},
			env:  connectEnv("postgres://nobody@127.0.0.1:1/none", "tradedoubler", "123"),
			want: "not an adapter this binary has",
		},
		{
			name: "no account to connect",
			args: []string{"connect-network", "-backfill-from", "2026-06-01"},
			env:  connectEnv("postgres://nobody@127.0.0.1:1/none", "fixture", ""),
			// Named for its own network, which is the point of the
			// per-driver blocks: an operator running two must be told which
			// account is missing, not that "the" account is.
			want: "NETWORK_FIXTURE_ACCOUNT_ID is unset",
		},
		{
			name: "a date that is not one",
			args: []string{"connect-network", "-backfill-from", "last June"},
			env:  connectEnv("postgres://nobody@127.0.0.1:1/none", "fixture", "123"),
			want: "is not a date",
		},
		{
			name: "an argument that is not a flag",
			args: []string{"connect-network", "awin"},
			env:  connectEnv("postgres://nobody@127.0.0.1:1/none", "fixture", "123"),
			want: "takes no arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := run(context.Background(), tt.args, envFrom(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run(%q) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
}

// TestConnectNetworkKnowsTheAwinDeclaration: the driver an operator will
// actually use has to resolve, and to Awin's own published numbers.
// TestConnectNetworkKnowsTheAwinDeclaration. Awin's declaration is still
// exactly what it was, and is still worth holding to the numbers Awin
// publishes - but it is read from the package rather than through
// documentedNetwork, because connect-network can no longer seed it.
//
// It used to. documentedNetwork knew awin while networkAdapter did not, so
// `connect-network --driver awin` wrote a cashback.network row and the server
// then refused to start against it. The registry ends that by construction:
// one entry answers both questions, and *awin.Client cannot be an entry
// because it has no FetchTransactions and no Limits.
//
// Importing awin here does not wire it. The reachability rule in
// internal/arch/network_isolation_test.go skips _test.go files, deliberately:
// a test is not a deployment, and an adapter reachable only from a test is
// one no operator can select.
func TestConnectNetworkKnowsTheAwinDeclaration(t *testing.T) {
	t.Parallel()

	documented := awin.Documented()
	if documented.ID != "awin" {
		t.Errorf("the declaration names %q, want %q", documented.ID, "awin")
	}
	if documented.RateLimitPerMinute != 20 {
		t.Errorf("RateLimitPerMinute = %d, want the 20 Awin publishes", documented.RateLimitPerMinute)
	}
	if documented.MaxQueryWindowDays != 31 {
		t.Errorf("MaxQueryWindowDays = %d, want the 31 Awin documents", documented.MaxQueryWindowDays)
	}
}

// TestConnectNetworkRefusesAwinUntilItImplementsThePort is the other half, and
// the one that is new. The declaration existing is not the same as the driver
// being connectable, and conflating the two is what produced a deployment
// seeded for a network it could not poll.
func TestConnectNetworkRefusesAwinUntilItImplementsThePort(t *testing.T) {
	t.Parallel()

	if _, err := documentedNetwork(config.NetworkDriverAwin); err == nil {
		t.Error("connect-network would seed an awin row, and the server cannot build an awin adapter to poll it")
	}
}
