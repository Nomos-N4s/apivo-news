package main

import (
	"bytes"
	"context"
	"io"
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

func TestRunRejectsMissingConfig(t *testing.T) {
	t.Parallel()
	err := run(context.Background(), envFrom(nil), io.Discard)
	if err == nil {
		t.Fatal("run() without DATABASE_URL: want error, got nil")
	}
}

func TestRunRejectsUnusableDatabase(t *testing.T) {
	t.Parallel()
	env := map[string]string{"DATABASE_URL": "this is not a connection string"}
	err := run(context.Background(), envFrom(env), io.Discard)
	if err == nil {
		t.Fatal("run() with unusable DATABASE_URL: want error, got nil")
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
	}

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, envFrom(env), out) }()

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
