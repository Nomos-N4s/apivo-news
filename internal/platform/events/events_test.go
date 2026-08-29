package events_test

// Shared scaffolding for the outbox and dispatcher suites.
//
// The integration tests run against a real Postgres, keyed on
// DATABASE_URL exactly as the schema suites in internal/platform/db are.
// The pure validation, registration and checkpoint tests run without one.
//
// Two databases are in play. The outbox tests use the suite's own
// database inside transactions that are always rolled back, like every
// sibling suite - domain_event is append-only, so a committed test row
// would be there forever. The dispatcher tests need commits: their
// subject matter is events whose occurred_at actually advances between
// appends and whose delivery crosses transaction boundaries, so they run
// against a scratch database that is dropped and recreated per run.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL != "" {
		if err := db.Migrate(baseURL); err != nil {
			fmt.Fprintln(os.Stderr, "migrating test database:", err)
			os.Exit(1)
		}
		cfg, err := pgxpool.ParseConfig(baseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parsing test database URL:", err)
			os.Exit(1)
		}
		// Every subtest holds a transaction, and hence a connection, for
		// its whole run; size the pool so parallel subtests do not queue
		// behind their own connections (see internal/platform/db).
		if want := int32(runtime.GOMAXPROCS(0)) + 4; cfg.MaxConns < want {
			cfg.MaxConns = want
		}
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connecting test database:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	if dispatchPool != nil {
		dispatchPool.Close()
	}
	os.Exit(code)
}

// beginTx opens a transaction that is always rolled back, keeping tests
// independent and the append-only stream clean.
func beginTx(t *testing.T) pgx.Tx {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the outbox")
	}
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// The dispatcher suites share one scratch database, built on first use.
// The error is stored rather than reported inside the once, so every
// test that needs the database sees the same verdict.
var (
	dispatchOnce sync.Once
	dispatchPool *pgxpool.Pool
	dispatchErr  error
)

// dispatcherDB returns a pool on the shared scratch database, creating
// and migrating it on the first call. Tests keep to their own event
// types (randomSuffix), so the shared stream never couples them: a
// dispatcher passes over every type it has no handler for.
func dispatcherDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the dispatcher")
	}
	dispatchOnce.Do(func() {
		dispatchPool, dispatchErr = newScratchPool(baseURL, "apivo_dispatcher")
	})
	if dispatchErr != nil {
		t.Fatalf("building the dispatcher scratch database: %v", dispatchErr)
	}
	return dispatchPool
}

// newScratchPool drops, recreates and migrates a database beside the test
// one, and returns a pool on it.
func newScratchPool(baseURL, name string) (*pgxpool.Pool, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL is not a URL: %w", err)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, "drop database if exists "+name+" with (force)"); err != nil {
		return nil, fmt.Errorf("dropping scratch database: %w", err)
	}
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		return nil, fmt.Errorf("creating scratch database: %w", err)
	}

	u.Path = "/" + name
	if err := db.Migrate(u.String()); err != nil {
		return nil, fmt.Errorf("migrating scratch database: %w", err)
	}
	return pgxpool.New(ctx, u.String())
}
