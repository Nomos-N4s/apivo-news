// Package nodocker_test is spike S3 (ADR-0002, task T008).
//
// Question: can cashback be developed and verified on a machine with no
// Docker? ADR-0002 states the constraint plainly — "local development on the
// founder's machine is constrained while Docker Desktop stays broken; CI
// remains the verification of record, and a `ledger=stub` mode must let
// non-ledger work proceed locally."
//
// S3 has two halves, and only one of them can be a Go test:
//
//   - "the full cashback CI job passes with Blnk and Redis as service
//     containers" is answered by the CI run itself. No assertion in this
//     repository can stand in for it, and the PR that lands this spike names
//     the run.
//   - "LEDGER_DRIVER=memory runs the stack" is a claim about configuration,
//     and that is what this file tests: the documented no-Docker environment
//     is accepted, it needs no ledger endpoint, no Redis and no network
//     credentials, and the convenience cannot leak into a deployment.
//
// The suite half — that `go test ./...` is green with every container-keyed
// test skipping rather than failing — is scripts/spikes/no_docker/run.sh,
// which CI runs in a job with no services at all.
//
// This file is test-only: it contributes no statements to the coverage gate.
// Unlike the other spikes it needs nothing at all to run, which is the point.
package nodocker_test

import (
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// noDockerEnv is the environment from the quickstart's "Without Docker"
// section, verbatim, plus the DATABASE_URL the binary requires whatever the
// ledger is. That extra key is the spike's main finding — see
// TestS3MemoryModeStillNeedsPostgres.
func noDockerEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":     "postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable",
		"CASHBACK_ENABLED": "true",
		"LEDGER_DRIVER":    config.LedgerDriverMemory,
		"NETWORK_DRIVER":   config.NetworkDriverFixture,
	}
}

func lookup(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func without(env map[string]string, keys ...string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// TestS3TheNoDockerEnvironmentIsAccepted is the headline: the exact command
// line the quickstart documents for a machine without Docker produces a
// configuration the binary will start on.
func TestS3TheNoDockerEnvironmentIsAccepted(t *testing.T) {
	t.Parallel()

	cfg, err := config.FromEnv(lookup(noDockerEnv()))
	if err != nil {
		t.Fatalf("the documented no-Docker environment was refused: %v", err)
	}
	if !cfg.Cashback.Enabled {
		t.Fatal("cashback is not enabled")
	}
	if cfg.Cashback.LedgerDriver != config.LedgerDriverMemory {
		t.Fatalf("LedgerDriver = %q, want %q", cfg.Cashback.LedgerDriver, config.LedgerDriverMemory)
	}
	if cfg.Cashback.UsesBlnk() {
		t.Fatal("the no-Docker configuration points at the Blnk sidecar")
	}
	if missing := cfg.Cashback.Missing(); len(missing) != 0 {
		t.Fatalf("the no-Docker configuration is incomplete: %v", missing)
	}
}

// TestS3MemoryModeNeedsNoSidecar pins what the memory ledger removes from the
// local loop. Each of these keys names a container the founder cannot run.
func TestS3MemoryModeNeedsNoSidecar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{name: "no ledger endpoint", key: "BLNK_URL"},
		{name: "no ledger credential", key: "BLNK_SECRET_KEY"},
		{name: "no redis", key: "REDIS_URL"},
		{name: "no network account", key: "NETWORK_ACCOUNT_ID"},
		{name: "no network key", key: "NETWORK_API_KEY"},
		{name: "no network secret", key: "NETWORK_API_SECRET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := noDockerEnv()
			if _, present := env[tt.key]; present {
				t.Fatalf("%s is in the no-Docker environment; this test proves it is not needed", tt.key)
			}
			if _, err := config.FromEnv(lookup(env)); err != nil {
				t.Fatalf("the no-Docker environment was refused without %s: %v", tt.key, err)
			}
		})
	}
}

// TestS3MemoryModeStillNeedsPostgres is the finding, written as an assertion
// so it cannot quietly stop being true.
//
// LEDGER_DRIVER=memory removes the ledger and Redis from the local loop. It
// does NOT remove Postgres: the binary requires DATABASE_URL and migrates on
// start. On a machine with no Docker there is no local Postgres either, so
// what runs without Docker is the TEST SUITE, not the server — unless
// DATABASE_URL points at a database somewhere else.
func TestS3MemoryModeStillNeedsPostgres(t *testing.T) {
	t.Parallel()

	_, err := config.FromEnv(lookup(without(noDockerEnv(), "DATABASE_URL")))
	if err == nil {
		t.Fatal("the binary started without DATABASE_URL; this test records that it cannot, and something has changed")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the refusal does not name DATABASE_URL: %v", err)
	}
	t.Logf("S3 finding: the memory ledger removes the ledger and Redis from the local loop, not Postgres. Refusal: %v", err)
}

// TestS3TheConvenienceCannotReachProduction guards the other direction. The
// whole reason the memory ledger exists is that Docker is unavailable
// locally; a deployment that inherited that setting would report balances
// that vanish with the process.
func TestS3TheConvenienceCannotReachProduction(t *testing.T) {
	t.Parallel()

	env := noDockerEnv()
	env["APP_ENV"] = config.EnvProd
	env["DATABASE_URL"] = "postgres://apivo@db.example.test:5432/apivo?sslmode=verify-full"

	_, err := config.FromEnv(lookup(env))
	if err == nil {
		t.Fatal("the in-process ledger was accepted in production")
	}
	if !strings.Contains(err.Error(), "persists nothing") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
}
