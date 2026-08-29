package events_test

// Migration 0021 both ways, against its own scratch database: up with the
// embedded set exactly as a deployment runs it, one step down - which
// must leave nothing of 0021 behind, while the stream it tracks survives
// untouched - and up again, after which the tables must work. While the
// schema is up, the two invariants the migration hands to the database
// are asserted the way the constitution asks: by SQLSTATE, against a real
// Postgres.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// deliveryTables are the objects 0021 creates and its down must remove.
var deliveryTables = []string{"subscriber_checkpoint", "event_delivery", "event_dead_letter"}

// expectSQLState asserts that the database refused a write with the given
// SQLSTATE - the discipline every invariant test in this repository
// follows.
func expectSQLState(t *testing.T, err error, code, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the database accepted it", what)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: got %v, want SQLSTATE %s", what, err, code)
	}
}

// tableExists reports whether the named public table is in the catalog.
func tableExists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(context.Background(),
		`select to_regclass($1)::text`, "public."+table).Scan(&reg); err != nil {
		t.Fatalf("asking the catalog about %s: %v", table, err)
	}
	return reg != nil
}

func TestMigration0021UpDownUp(t *testing.T) {
	t.Parallel()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the migration")
	}
	ctx := context.Background()

	// A scratch database of this test's own: stepping the schema down
	// must not touch the databases the other suites share.
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("DATABASE_URL is not a URL: %v", err)
	}
	const scratchName = "apivo_delivery_migration"
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if _, err := admin.Exec(ctx, "drop database if exists "+scratchName+" with (force)"); err != nil {
		t.Fatalf("dropping the scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create database "+scratchName); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	_ = admin.Close(ctx)
	u.Path = "/" + scratchName
	scratchURL := u.String()

	// Up: the embedded set, exactly as every deployment runs it.
	if err := db.Migrate(scratchURL); err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	pool, err := pgxpool.New(ctx, scratchURL)
	if err != nil {
		t.Fatalf("connecting the scratch database: %v", err)
	}
	defer pool.Close()
	for _, table := range deliveryTables {
		if !tableExists(t, pool, table) {
			t.Fatalf("after migrating up, %s does not exist", table)
		}
	}

	// Two committed stream rows on one lane to hang tracking rows on.
	// They must survive the down: 0021 tracks the stream, it never owns
	// it.
	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	subject := uuid.New()
	first, err := w.Append(ctx, pool, events.Message{Type: "cashback.migration.tested", Subject: subject, Payload: []byte(`{"n": 1}`)})
	if err != nil {
		t.Fatalf("appending the first event: %v", err)
	}
	second, err := w.Append(ctx, pool, events.Message{Type: "cashback.migration.tested", Subject: subject, Payload: []byte(`{"n": 2}`)})
	if err != nil {
		t.Fatalf("appending the second event: %v", err)
	}

	// The completion row - the record that makes redelivery a no-op - is
	// immutable, by trigger, exactly as domain_event is.
	const subscriberName = "migration-subscriber"
	if _, err := pool.Exec(ctx,
		`insert into event_delivery (subscriber, event_id, attempts) values ($1, $2, 1)`,
		subscriberName, first.EventID.String()); err != nil {
		t.Fatalf("inserting a completion row: %v", err)
	}
	_, err = pool.Exec(ctx, `update event_delivery set attempts = 2 where subscriber = $1`, subscriberName)
	expectSQLState(t, err, pgerrcode.RaiseException, "rewriting a completion row")
	_, err = pool.Exec(ctx, `delete from event_delivery where subscriber = $1`, subscriberName)
	expectSQLState(t, err, pgerrcode.RaiseException, "deleting a completion row")

	// At most one parked head per (subscriber, type, subject) lane,
	// enforced by the partial unique index rather than by the dispatcher
	// remembering it...
	parkHead := func(eventID uuid.UUID) error {
		_, err := pool.Exec(ctx,
			`insert into event_dead_letter (subscriber, event_id, event_type, subject, occurred_at, attempts, last_error)
			 values ($1, $2, $3, $4, now(), 1, 'the handler failed')`,
			subscriberName, eventID.String(), first.Type, subject.String())
		return err
	}
	if err := parkHead(first.EventID); err != nil {
		t.Fatalf("parking the lane head: %v", err)
	}
	expectSQLState(t, parkHead(second.EventID), pgerrcode.UniqueViolation, "parking a second head on the same lane")
	// ...and only for parked heads: a requeued row frees the slot, so the
	// next failure on the resumed lane can park.
	if _, err := pool.Exec(ctx,
		`update event_dead_letter set requeued_at = now() where subscriber = $1 and event_id = $2`,
		subscriberName, first.EventID.String()); err != nil {
		t.Fatalf("requeuing the parked head: %v", err)
	}
	if err := parkHead(second.EventID); err != nil {
		t.Fatalf("parking on the lane after its head was requeued: %v", err)
	}

	// Down one step, to 0020. The migration runner drives it exactly as
	// an operator would.
	src, err := iofs.New(os.DirFS("../db/migrations"), ".")
	if err != nil {
		t.Fatalf("loading the migration files: %v", err)
	}
	sqlDB, err := sql.Open("pgx", scratchURL)
	if err != nil {
		t.Fatalf("opening the migration connection: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("building the migration driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("building the migration runner: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(20); err != nil {
		t.Fatalf("migrating down to 0020: %v", err)
	}
	for _, table := range deliveryTables {
		if tableExists(t, pool, table) {
			t.Errorf("after migrating down, %s is still there; the down migration left it behind", table)
		}
	}
	var streamRows int
	if err := pool.QueryRow(ctx,
		`select count(*) from domain_event where type = $1`, first.Type).Scan(&streamRows); err != nil {
		t.Fatalf("counting the stream: %v", err)
	}
	if streamRows != 2 {
		t.Errorf("the down migration disturbed the stream: %d event(s) left, want 2", streamRows)
	}

	// Up again: the schema returns empty and working - a completion row
	// for the surviving stream events inserts cleanly.
	if err := db.Migrate(scratchURL); err != nil {
		t.Fatalf("migrating back up: %v", err)
	}
	for _, table := range deliveryTables {
		if !tableExists(t, pool, table) {
			t.Fatalf("after migrating back up, %s does not exist", table)
		}
	}
	var trackingRows int
	if err := pool.QueryRow(ctx,
		`select (select count(*) from event_delivery) + (select count(*) from event_dead_letter) + (select count(*) from subscriber_checkpoint)`,
	).Scan(&trackingRows); err != nil {
		t.Fatalf("counting the tracking tables: %v", err)
	}
	if trackingRows != 0 {
		t.Errorf("the re-applied schema holds %d row(s); it must return empty", trackingRows)
	}
	if _, err := pool.Exec(ctx,
		`insert into event_delivery (subscriber, event_id, attempts) values ($1, $2, 1)`,
		subscriberName, first.EventID.String()); err != nil {
		t.Fatalf("inserting a completion row after the round trip: %v", err)
	}
}
