package main

// Contract tests for serve()'s zero-sum arm, mirroring the translation
// wiring tests' style: with cashback enabled the continuous C-1 check is
// registered and actually runs, the exit-route driver points it at the
// schema 0022 creates, and with the product off neither the check nor its
// scheduler exists in the process at all - which is also what proves the
// binary serves with cashback disabled.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// cashbackEnv is the smallest environment that mounts the cashback product:
// the in-process ledger and the fixture network, so no sidecar or
// credential is needed to prove the wiring.
func cashbackEnv(dbURL string) map[string]string {
	return map[string]string{
		"DATABASE_URL":     dbURL,
		"HTTP_ADDR":        "127.0.0.1:0",
		"POLL_INTERVAL":    "0",
		"CASHBACK_ENABLED": "true",
		"LEDGER_DRIVER":    "memory",
		"NETWORKS":         "fixture",
		// Required even for the fixture, which needs no credential: the
		// cursors live on a network_account row and this names which one.
		// Without it CashbackConfig.Mountable is false and cashback does
		// not mount at all, which every caller of this helper assumes it
		// does.
		"NETWORK_FIXTURE_ACCOUNT_ID": "publisher-1",
	}
}

// waitForLog polls the buffer until the fragment appears, failing after
// the deadline. The check's first run lands within the scheduler's startup
// jitter - a tenth of the wallet package's cadence - so thirty seconds is
// margin, not hope.
func waitForLog(t *testing.T, out *syncBuffer, fragment string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), fragment) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared within the deadline; output: %q", fragment, out.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestRunStartsTheZeroSumCheckWhenCashbackIsEnabled proves serve() wires
// the check for real: the startup line names the job and its cadence, and
// the scheduler completes an actual run before shutdown - the check is
// running, not merely announced.
func TestRunStartsTheZeroSumCheckWhenCashbackIsEnabled(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	out, stop := runServing(t, cashbackEnv(isolatedDatabase(t, dbURL)))
	defer stop()

	if !strings.Contains(out.String(), "continuous ledger zero-sum check started") {
		t.Errorf("serve() with cashback enabled never announced the zero-sum check; output: %q", out.String())
	}

	// The isolated database holds no co-located ledger, so the first run
	// is vacuously clean and completes - which is exactly what shows the
	// scheduler is driving the check rather than holding a registration.
	waitForLog(t, out, "job completed")
	if !strings.Contains(out.String(), "ledger-zero-sum") {
		t.Errorf("the completed run does not carry the job's name; output: %q", out.String())
	}
}

// TestRunPinsTheZeroSumCheckToTheExitRouteLedger is the wiring half of the
// exit-route case: under LEDGER_DRIVER=postgres the check must read the
// `ledger` schema 0022 creates, not 0020's `blnk` default, which this
// database does not carry. One account planted before serving gives the
// ledger's balances view exactly one currency, so a check reading the
// right schema says it verified one currency - and a check left on the
// default would sum nothing and say that instead, the vacuous pass this
// wiring exists to prevent. The clean line is DEBUG by design, so the test
// asks for that level.
func TestRunPinsTheZeroSumCheckToTheExitRouteLedger(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	isoURL := isolatedDatabase(t, dbURL)
	// serve() migrates before it connects, but the account must exist
	// before the check's first tick, so the test migrates first - Migrate
	// is idempotent - and plants the row itself.
	if err := platformdb.Migrate(isoURL); err != nil {
		t.Fatalf("migrating the isolated database: %v", err)
	}
	seed, err := pgxpool.New(context.Background(), isoURL)
	if err != nil {
		t.Fatalf("connecting to seed the exit-route ledger: %v", err)
	}
	defer seed.Close()
	if _, err := seed.Exec(context.Background(),
		`insert into ledger.account (id, currency, kind) values ('house:wiring:eur', 'EUR', 'house')`); err != nil {
		t.Fatalf("planting the account the check must see: %v", err)
	}

	env := cashbackEnv(isoURL)
	env["LEDGER_DRIVER"] = "postgres"
	env["LOG_LEVEL"] = "debug"
	out, stop := runServing(t, env)
	defer stop()

	waitForLog(t, out, "the ledger nets to zero in every currency (C-1)")
	logged := out.String()
	if !strings.Contains(logged, "currencies=1") {
		t.Errorf("the verified run does not vouch for the planted currency; output: %q", logged)
	}
	if strings.Contains(logged, "summed no currencies") {
		t.Errorf("a check wired to the exit route reported itself vacuous; output: %q", logged)
	}
}

// TestRunServesWithoutTheZeroSumCheck pins the only off state there is:
// with the product off the check simply does not exist - not even a line
// about it, because with cashback absent there is no C-1 to watch. There
// is deliberately no other switch, so this is the whole of the off
// behaviour. And it serves: the check watches the site, it must never
// take it down.
func TestRunServesWithoutTheZeroSumCheck(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	out, stop := runServing(t, map[string]string{
		"DATABASE_URL":  dbURL,
		"HTTP_ADDR":     "127.0.0.1:0",
		"POLL_INTERVAL": "0",
	})
	defer stop()

	if logged := out.String(); strings.Contains(logged, "zero-sum") {
		t.Errorf("serve() without cashback mentioned the zero-sum check; output: %q", logged)
	}
}
