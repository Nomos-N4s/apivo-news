package main

// Contract tests for the affiliate-network arm of serve(), in the style of
// the zero-sum and translation wiring tests: what the composition root
// builds, what it declines to build and says so about, and what it refuses
// to start over at all.
//
// The decision cases run against a transaction that is rolled back, because
// what they turn on is a ROW - which account is connected, whether it is
// switched on, whether anybody said where it starts - and a served process
// is a slow and indirect way to vary one.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// networkWiringTx opens a transaction on the shared database and rolls it
// back, so a case may seed whatever account it needs without leaving one.
func networkWiringTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the network wiring")
	}
	if err := platformdb.Migrate(url); err != nil {
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

// networkWiringAccount seeds the fixture network and one publisher account
// at it, in whatever state the case needs.
func networkWiringAccount(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, active bool, start any) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Wiring Network', 'clickref', 31, 360, true)
		on conflict (id) do nothing`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active, backfill_from)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', $2, $3)`,
		networkID, active, start); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
}

func networkWiringConfig(driver, accountID string) config.NetworkConfig {
	return config.NetworkConfig{Driver: driver, AccountID: accountID}
}

// TestNewNetworkSweepsSaysWhatIsMissing holds the difference between a
// deployment that is not finished and one that is broken. Every case here is
// the first kind: it reports ingestionOff, which serve() turns into an ERROR
// line and carries on from, because the fix is an operator action against the
// database or one more environment key - never a reason to take the public
// news site down.
func TestNewNetworkSweepsSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	ctx, tx := networkWiringTx(t)
	networkWiringAccount(ctx, t, tx, config.NetworkDriverFixture, true, time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))

	cases := map[string]struct {
		cfg  config.NetworkConfig
		says string
	}{
		"no adapter is named": {
			cfg: networkWiringConfig("", "publisher-1"), says: config.NetworksKey,
		},
		// Unreachable through serve since T215 - CashbackConfig.Mountable
		// declines to build cashback at all while a configured network has
		// no account id - and asserted anyway, because this function is
		// called with one network's config rather than with the decision
		// that admitted it, and must not start trusting a gate it cannot
		// see. The key it names is the network's own.
		"no publisher account is named": {
			cfg: networkWiringConfig(config.NetworkDriverFixture, ""), says: "NETWORK_FIXTURE_ACCOUNT_ID",
		},
		"the named account is not connected": {
			cfg: networkWiringConfig(config.NetworkDriverFixture, "publisher-nobody-connected"), says: "is connected",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, _, err := connectNetwork(ctx, tc.cfg, tx)
			var off *ingestionOff
			if !errors.As(err, &off) {
				t.Fatalf("connectNetwork() = %v, want one wrapping ingestionOff", err)
			}
			if !strings.Contains(off.Error(), tc.says) {
				t.Errorf("the reason is %q, want it to name %q", off.Error(), tc.says)
			}
			if adapter != nil {
				t.Error("a deployment that is not connected was handed an adapter anyway")
			}
		})
	}
}

// TestNewNetworkSweepsRefusesAnAdapterThisBinaryDoesNotHave is the other
// kind, and the distinction is the whole point of the type. An operator has
// said which network this deployment integrates; a binary that does not have
// it cannot be made to by configuration, and starting anyway would leave a
// deployment that believes it is connected to a network nothing can reach.
func TestNewNetworkSweepsRefusesAnAdapterThisBinaryDoesNotHave(t *testing.T) {
	t.Parallel()
	ctx, tx := networkWiringTx(t)
	// The row exists, so the refusal is about the adapter and nothing else.
	networkWiringAccount(ctx, t, tx, "reference_network", true, time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))

	_, _, err := connectNetwork(ctx, networkWiringConfig("reference_network", "publisher-1"), tx)
	var off *ingestionOff
	if errors.As(err, &off) {
		t.Fatalf("an adapter this binary does not have was reported as a deployment that is merely unfinished: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "reference_network") {
		t.Fatalf("connectNetwork() = %v, want a refusal naming the driver", err)
	}
}

// TestNewNetworkSweepsBuildsThePairAndReportsTheRowsGaps covers the happy
// path and the two states the row can be in that the POLLER refuses rather
// than the wiring. Both are fixed with one UPDATE and no restart, which is
// what registering anyway buys - and both get a startup line, so an operator
// does not have to work out from an error stream why their sweeps fail.
func TestNewNetworkSweepsBuildsThePairAndReportsTheRowsGaps(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]struct {
		active bool
		start  any
		says   string
	}{
		"a connected, active account with a start": {active: true, start: start},
		"an account nobody has switched on":        {active: false, start: start, says: "NOT ACTIVE"},
		"an account with no start":                 {active: true, start: nil, says: "how far back to read"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, tx := networkWiringTx(t)
			networkWiringAccount(ctx, t, tx, config.NetworkDriverFixture, tc.active, tc.start)

			var logged strings.Builder
			log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			adapter, connected, err := connectNetwork(ctx,
				networkWiringConfig(config.NetworkDriverFixture, "publisher-1"), tx)
			if err != nil {
				t.Fatalf("connectNetwork(): %v", err)
			}
			sweeps, err := newNetworkSweeps(ctx, log, adapter, connected, tx)
			if err != nil {
				t.Fatalf("newNetworkSweeps(): %v", err)
			}
			if sweeps == nil {
				t.Fatal("a connected account produced no sweeps")
			}
			if got := sweeps.Account().ExternalID(); got != "publisher-1" {
				t.Errorf("the sweeps poll %q, want the connected account", got)
			}

			// Registering is the point: the names have to be ones a
			// scheduler accepts, and both of them have to be there.
			jobs := scheduler.New(slog.New(slog.DiscardHandler), &networkWiringLocker{}, scheduler.Config{})
			if err := sweeps.Register(jobs); err != nil {
				t.Fatalf("Register(): %v", err)
			}
			for _, name := range []string{
				networks.ForwardJobName(sweeps.Account()),
				networks.TrailingJobName(sweeps.Account()),
			} {
				if _, err := jobs.RunOnce(ctx, name); err != nil && strings.Contains(err.Error(), "no job named") {
					t.Errorf("%q was not registered", name)
				}
			}

			if tc.says == "" {
				if strings.Contains(logged.String(), "level=ERROR") {
					t.Errorf("a complete account produced an ERROR line: %s", logged.String())
				}
				return
			}
			if !strings.Contains(logged.String(), tc.says) {
				t.Errorf("the startup lines do not say %q: %s", tc.says, logged.String())
			}
		})
	}
}

// networkWiringLocker hands out every lock, so RunOnce reaches the job.
type networkWiringLocker struct{}

func (networkWiringLocker) TryLock(context.Context, string) (scheduler.Lock, bool, error) {
	return networkWiringLock{}, true, nil
}

type networkWiringLock struct{}

func (networkWiringLock) Release(context.Context) error { return nil }

// TestTheSweepsNeedABiggerPoolThanTheCheckAlone pins the arithmetic serve()
// depends on, and the reason this is worth a test of its own: registering the
// sweeps takes the pool requirement from four connections to eight, and pgx
// defaults MaxConns to max(4, NumCPU). A deployment that turns ingestion on
// without raising pool_max_conns is refused at startup with the numbers in
// the error - which is the designed behaviour, not a bug, and it must stay
// discoverable rather than becoming a deadlock at three in the morning.
func TestTheSweepsNeedABiggerPoolThanTheCheckAlone(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the pool arithmetic")
	}

	ctx := context.Background()
	for _, tc := range []struct {
		conns   int
		jobs    int
		refused bool
	}{
		{conns: 4, jobs: 1},
		{conns: 4, jobs: 3, refused: true},
		{conns: 8, jobs: 3},
	} {
		pool, err := pgxpool.New(ctx, url+"&pool_max_conns="+strconv.Itoa(tc.conns))
		if err != nil {
			t.Fatalf("connecting with pool_max_conns=%d: %v", tc.conns, err)
		}
		err = scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}).CheckCapacity(tc.jobs)
		pool.Close()
		if tc.refused && err == nil {
			t.Errorf("%d job(s) on a pool of %d was accepted; the jobs would deadlock against the request handlers", tc.jobs, tc.conns)
		}
		if !tc.refused && err != nil {
			t.Errorf("%d job(s) on a pool of %d was refused: %v", tc.jobs, tc.conns, err)
		}
	}
}

// TestRunRegistersTheNetworkSweeps is the whole arm through a served
// process: a connected account, and a startup line naming both jobs and the
// cadences they will run on.
func TestRunRegistersTheNetworkSweeps(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	isoURL := isolatedDatabase(t, dbURL)
	if err := platformdb.Migrate(isoURL); err != nil {
		t.Fatalf("migrating the isolated database: %v", err)
	}
	seed, err := pgxpool.New(context.Background(), isoURL)
	if err != nil {
		t.Fatalf("connecting to seed the account: %v", err)
	}
	defer seed.Close()
	if _, err := seed.Exec(context.Background(), `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Fixture Network', 'clickref', 31, 360, true)`, config.NetworkDriverFixture); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if _, err := seed.Exec(context.Background(), `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active, backfill_from)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', true, $2)`,
		config.NetworkDriverFixture, time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}

	env := cashbackEnv(isoURL)
	// Three jobs need eight connections, and the default is four.
	env["DATABASE_URL"] = isoURL + "&pool_max_conns=8"
	out, stop := runServing(t, env)
	defer stop()

	logged := out.String()
	if !strings.Contains(logged, "affiliate network sweeps registered") {
		t.Fatalf("serve() never announced the sweeps; output: %q", logged)
	}
	for _, want := range []string{
		"network-poll:fixture:publisher-1",
		"network-trailing-poll:fixture:publisher-1",
		"forward_interval=15m0s",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the startup line does not carry %q; output: %q", want, logged)
		}
	}
	if strings.Contains(logged, "NO AFFILIATE NETWORK IS BEING POLLED") {
		t.Errorf("a wired deployment reported itself unwired; output: %q", logged)
	}
}

// TestRunSaysWhenNoNetworkIsConnected is the state this arm exists to make
// impossible to miss: cashback mounted, and no network connected - so
// nothing ingests what any network reports AND no click-out can be issued.
// It is loud and it is not fatal - the site stays up - which is the same
// trade serve() makes for an unset JWKS_URL.
func TestRunSaysWhenNoNetworkIsConnected(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	// The fixture network fully configured, against a database with no
	// cashback.network_account row in it: the shape every environment is in
	// until an operator runs connect-network.
	//
	// It used to be "no account named", and that state no longer reaches
	// here: an unnamed account makes CashbackConfig.Mountable false, so
	// cashback is not built and there is nothing to report a missing
	// connection about (TestRunDoesNotMountCashbackOverAnUnusableNetwork
	// covers that one). What is left is the case an operator actually
	// meets - configuration complete, the database not yet written to.
	out, stop := runServing(t, cashbackEnv(isolatedDatabase(t, dbURL)))
	defer stop()

	logged := out.String()
	if !strings.Contains(logged, "NO AFFILIATE NETWORK IS CONNECTED") {
		t.Fatalf("an unwired deployment said nothing about it; output: %q", logged)
	}
	if !strings.Contains(logged, "publisher-1") {
		t.Errorf("the line does not name the account it looked for; output: %q", logged)
	}
	// Once, not twice. The poller and the click-out endpoint both need the
	// connection, and two ERROR lines about one missing account read as two
	// different problems.
	if got := strings.Count(logged, "NO AFFILIATE NETWORK IS CONNECTED"); got != 1 {
		t.Errorf("the line appears %d times, want once", got)
	}
	// And it serves anyway: ingestion is not the site.
	if !strings.Contains(logged, "starting") {
		t.Errorf("a deployment with no network configured did not serve; output: %q", logged)
	}
}

// TestRunDoesNotMountCashbackOverAnUnusableNetwork is the founder's decision
// of 2026-09-04, asserted end to end: a network named in NETWORKS whose keys
// are not set turns CASHBACK off and leaves the rest of the deployment
// serving.
//
// Both halves matter and neither is obvious from the other. Mounting
// cashback over a network that cannot poll would let members click and buy
// and never be credited, with nothing failing anywhere. Failing the PROCESS
// instead - which is what putting these keys in CashbackConfig.Missing would
// do - would take the public news site down over a typo in one network's
// credential, and the risk of that grows with every network added.
func TestRunDoesNotMountCashbackOverAnUnusableNetwork(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	env := cashbackEnv(isolatedDatabase(t, dbURL))
	// A second network, named and unconfigured. The first one is complete,
	// which is the point: ALL of them must be usable, not merely one, or a
	// deployment that names two and polls one is not the deployment its
	// operator described.
	env["NETWORKS"] = config.NetworkDriverFixture + ",reference_network"

	out, stop := runServing(t, env)
	defer stop()

	logged := out.String()
	for _, want := range []string{
		// The cause, by name, with the key an operator would set.
		"reference_network",
		"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID",
		"NETWORK_REFERENCE_NETWORK_API_KEY",
		// And the consequence, said separately, because "one network is
		// misconfigured" and "the cashback product is not running" are
		// different sizes of news.
		"CASHBACK IS NOT MOUNTED",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the startup log does not carry %q; output: %q", want, logged)
		}
	}
	// The usable network is not polled either - it is not "the ones that
	// work", it is none of them.
	if strings.Contains(logged, "affiliate network sweeps registered") {
		t.Errorf("sweeps were registered over a half-configured deployment; output: %q", logged)
	}
	// And the deployment serves regardless. This is the half that keeps a
	// credential typo from taking apivo.news down.
	if !strings.Contains(logged, "starting") {
		t.Errorf("a deployment with one unusable network did not serve; output: %q", logged)
	}
}
