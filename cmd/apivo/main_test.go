package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// syncBuffer is a goroutine-safe writer capturing run()'s log output so the
// test can observe startup progress instead of guessing with sleeps.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// healthzServer starts a test HTTP server answering GET /healthz with the
// given status code and returns its host:port.
func healthzServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRunRejectsMissingConfig(t *testing.T) {
	t.Parallel()
	err := run(context.Background(), nil, envFrom(nil), io.Discard)
	if err == nil {
		t.Fatal("run() without DATABASE_URL: want error, got nil")
	}
}

func TestRunRejectsUnusableDatabase(t *testing.T) {
	t.Parallel()
	env := map[string]string{"DATABASE_URL": "this is not a connection string"}
	err := run(context.Background(), nil, envFrom(env), io.Discard)
	if err == nil {
		t.Fatal("run() with unusable DATABASE_URL: want error, got nil")
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown command", args: []string{"frobnicate"}, want: "unknown command"},
		{name: "healthcheck with extra arguments", args: []string{"healthcheck", "extra"}, want: "takes no arguments"},
		{name: "version with extra arguments", args: []string{"version", "extra"}, want: "takes no arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := run(context.Background(), tt.args, envFrom(nil), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run() with args %q: want error containing %q, got %v", tt.args, tt.want, err)
			}
		})
	}
}

// TestRunVersion drives the subcommand end to end through run(). An unstamped
// test binary must report "dev" - the honest name for anything the release
// pipeline did not cut - and need neither configuration nor a database.
func TestRunVersion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, envFrom(nil), &out); err != nil {
		t.Fatalf("run(version): %v", err)
	}
	if got, want := out.String(), "apivo version dev\n"; got != want {
		t.Errorf("run(version) printed %q, want %q", got, want)
	}
}

// TestRunHealthcheck drives the subcommand end to end through run(). It
// deliberately sets no DATABASE_URL: a liveness probe must work without the
// serving configuration.
func TestRunHealthcheck(t *testing.T) {
	t.Parallel()
	addr := healthzServer(t, http.StatusOK)
	env := map[string]string{"HTTP_ADDR": addr}
	if err := run(context.Background(), []string{"healthcheck"}, envFrom(env), io.Discard); err != nil {
		t.Errorf("run(healthcheck) against healthy server: %v", err)
	}
}

func TestHealthcheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{name: "healthy server", addr: healthzServer(t, http.StatusOK), wantErr: false},
		{name: "unhealthy status", addr: healthzServer(t, http.StatusServiceUnavailable), wantErr: true},
		// Port 0 is never a valid destination, so the connection fails
		// deterministically - unlike a freed ephemeral port, which a later
		// parallel listener could be handed back.
		{name: "nothing listening", addr: "127.0.0.1:0", wantErr: true},
		{name: "invalid address", addr: "not an address", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"HTTP_ADDR": tt.addr}
			err := healthcheck(context.Background(), envFrom(env))
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("healthcheck() with HTTP_ADDR=%q: error = %v, wantErr %t", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestProbeAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "explicit HTTP_ADDR", env: map[string]string{"HTTP_ADDR": ":9090"}, want: ":9090"},
		{name: "defaults to :8080", env: nil, want: ":8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := probeAddr(envFrom(tt.env)); got != tt.want {
				t.Errorf("probeAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHealthURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{name: "port only", addr: ":8080", want: "http://localhost:8080/healthz"},
		{name: "wildcard ipv4", addr: "0.0.0.0:9090", want: "http://localhost:9090/healthz"},
		{name: "wildcard ipv6", addr: "[::]:8080", want: "http://localhost:8080/healthz"},
		{name: "explicit host", addr: "127.0.0.1:8081", want: "http://127.0.0.1:8081/healthz"},
		{name: "missing port", addr: "8080", wantErr: true},
		{name: "empty", addr: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := healthURL(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("healthURL(%q): want error, got %q", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("healthURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

// TestReadiness is an integration test: it needs a real Postgres, keyed on
// DATABASE_URL like the schema invariant tests.
func TestReadiness(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	if err := readiness(pool)(ctx); err != nil {
		t.Errorf("readiness with live database: %v", err)
	}
	pool.Close()
	if err := readiness(pool)(ctx); err == nil {
		t.Error("readiness with closed pool: want error, got nil")
	}
}

// TestRunServesAndShutsDown is an integration test: it needs a real Postgres,
// keyed on DATABASE_URL like the schema invariant tests.
func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	env := map[string]string{
		"DATABASE_URL": dbURL,
		"HTTP_ADDR":    "127.0.0.1:0",
		// Polling stays off: this test shares its database with every
		// other suite in the run, and a poller here would fetch their
		// seeded sources and contend for the fleet-wide poll lock the
		// ingestion tests assert on.
		"POLL_INTERVAL": "0",
	}

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, envFrom(env), out) }()

	// Wait until the "starting" log line proves config, migrate and connect
	// all succeeded, then trigger the graceful shutdown.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "starting") {
		if time.Now().After(deadline) {
			t.Fatalf("run() never reached the serving phase; output: %q", out.String())
		}
		select {
		case err := <-done:
			t.Fatalf("run() exited before serving: %v; output: %q", err, out.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run() did not return after context cancellation")
	}
}
