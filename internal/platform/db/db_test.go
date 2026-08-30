package db_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

func TestMigrateRejectsInvalidURL(t *testing.T) {
	t.Parallel()
	if err := db.Migrate("this is not a connection string"); err == nil {
		t.Fatal("Migrate() with invalid URL: want error, got nil")
	}
}

func TestConnectRejectsInvalidURL(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.Connect(ctx, "this is not a connection string"); err == nil {
		t.Fatal("Connect() with invalid URL: want error, got nil")
	}
}

func TestMigrateRejectsUnreachableServer(t *testing.T) {
	t.Parallel()
	// Parses as a URL, so the failure happens at connection time inside the
	// migration driver rather than at DSN parse time.
	if err := db.Migrate("postgres://u:p@192.0.2.1:5432/x?connect_timeout=1"); err == nil {
		t.Fatal("Migrate() to unreachable server: want error, got nil")
	}
}

func TestConnectRejectsUnreachableServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Reserved TEST-NET-1 address: never routable, fails fast.
	if _, err := db.Connect(ctx, "postgres://u:p@192.0.2.1:5432/x?connect_timeout=1"); err == nil {
		t.Fatal("Connect() to unreachable server: want error, got nil")
	}
}

// TestMigrateSurfacesFailingMigration is an integration test: a scratch
// database is sabotaged with a conflicting object so the first migration
// fails, and Migrate must report it rather than swallow it.
func TestMigrateSurfacesFailingMigration(t *testing.T) {
	t.Parallel()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Skipf("DATABASE_URL is not a URL (%v); cannot derive a scratch database", err)
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	const scratch = "apivo_migrate_fail"
	if _, err := admin.Exec(ctx, "drop database if exists "+scratch+" with (force)"); err != nil {
		t.Fatalf("dropping scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create database "+scratch); err != nil {
		t.Fatalf("creating scratch database: %v", err)
	}

	u.Path = "/" + scratch
	scratchURL := u.String()

	saboteur, err := pgxpool.New(ctx, scratchURL)
	if err != nil {
		t.Fatalf("connecting to scratch database: %v", err)
	}
	if _, err := saboteur.Exec(ctx, `create table language (nope int)`); err != nil {
		saboteur.Close()
		t.Fatalf("sabotaging scratch database: %v", err)
	}
	saboteur.Close()

	if err := db.Migrate(scratchURL); err == nil {
		t.Fatal("Migrate() against sabotaged database: want error, got nil")
	}
}

// TestMigrateAcceptsThePoolParametersConnectAccepts holds the one DSN
// property that has to be true in both halves of this package. pool_max_conns
// is pgxpool's own parameter: Connect understands it, and the database/sql
// driver does not - it forwards what it does not recognise to the server as a
// runtime setting, which answers FATAL.
//
// Migrate runs FIRST at startup, so if it could not read the same DSN, adding
// pool_max_conns would take the whole process down before anything else was
// tried - and pool_max_conns is the only way to raise a pool that the
// scheduler's capacity check may demand (T057).
func TestMigrateAcceptsThePoolParametersConnectAccepts(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	withPool := url + "&pool_max_conns=8"
	if err := db.Migrate(withPool); err != nil {
		t.Fatalf("Migrate() refused a DSN carrying pool_max_conns: %v", err)
	}
	pool, err := db.Connect(context.Background(), withPool)
	if err != nil {
		t.Fatalf("Connect() refused the same DSN: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 8 {
		t.Errorf("the pool allows MaxConns=%d, want the 8 the DSN asked for", got)
	}
}
