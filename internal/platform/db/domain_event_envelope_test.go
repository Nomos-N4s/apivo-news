package db_test

// The envelope columns are added to a table that is APPEND ONLY and has
// been since 0001. The claim 0018 makes is not "the triggers were disabled
// carefully" but "nothing was written at all":
//
//	ADD COLUMN with a constant default does not rewrite the table
//	(PostgreSQL 11+), and DDL does not fire row-level triggers, so every
//	pre-existing row keeps its physical identity and append-only holds
//	continuously - before, during and after.
//
// A claim about physical identity has to be checked physically, so the
// first test builds a scratch database, migrates it to 0017, writes a row,
// records the table's relfilenode and the row's ctid, migrates to 0018, and
// requires both to be unchanged. A rewrite would move the row; the ctid
// would move with it.
//
// The second claim is that the two existing writers keep working
// UNMODIFIED. That is checked against the committed writer files rather
// than against a copy of them, so editing either one to name a new column
// fails here.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// Migration versions either side of the envelope change.
const (
	versionBeforeEnvelope = 17
	versionWithEnvelope   = 18
)

// scratchDatabase creates an empty database beside the test one and returns
// its URL. A separate database is required because the test needs to stand
// at migration 0017, and the suite's own database is fully migrated.
func scratchDatabase(t *testing.T, name string) string {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Skipf("DATABASE_URL is not a URL (%v); cannot derive a scratch database", err)
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, "drop database if exists "+name+" with (force)"); err != nil {
		t.Fatalf("dropping scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("creating scratch database: %v", err)
	}

	u.Path = "/" + name
	return u.String()
}

// migratorFor opens a migrator over the embedded migration directory,
// wired exactly as db.Migrate wires it, but able to stop at a version.
func migratorFor(t *testing.T, databaseURL string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(os.DirFS("migrations"), ".")
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("opening scratch database: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("migration driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

// TestDomainEventEnvelopeDoesNotRewriteTheTable is the physical assertion:
// the envelope columns arrive without a single existing row being touched.
func TestDomainEventEnvelopeDoesNotRewriteTheTable(t *testing.T) {
	t.Parallel()
	scratchURL := scratchDatabase(t, "apivo_envelope_rewrite")
	ctx := context.Background()

	m := migratorFor(t, scratchURL)
	if err := m.Migrate(versionBeforeEnvelope); err != nil {
		t.Fatalf("migrating to %d: %v", versionBeforeEnvelope, err)
	}

	conn, err := pgx.Connect(ctx, scratchURL)
	if err != nil {
		t.Fatalf("connecting to scratch database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var eventID string
	if err := conn.QueryRow(ctx,
		`insert into domain_event (type, payload)
		 values ('article.approved', '{"article_id":"before"}'::jsonb) returning id`).Scan(&eventID); err != nil {
		t.Fatalf("writing an event under 0017 semantics: %v", err)
	}

	var beforeFilenode int64
	var beforeCtid string
	if err := conn.QueryRow(ctx,
		`select pg_relation_filenode('domain_event')::bigint,
		        (select ctid::text from domain_event where id = $1)`, eventID).
		Scan(&beforeFilenode, &beforeCtid); err != nil {
		t.Fatalf("reading the physical identity before the migration: %v", err)
	}

	if err := m.Migrate(versionWithEnvelope); err != nil {
		t.Fatalf("migrating to %d: %v", versionWithEnvelope, err)
	}

	var afterFilenode int64
	var afterCtid string
	if err := conn.QueryRow(ctx,
		`select pg_relation_filenode('domain_event')::bigint,
		        (select ctid::text from domain_event where id = $1)`, eventID).
		Scan(&afterFilenode, &afterCtid); err != nil {
		t.Fatalf("reading the physical identity after the migration: %v", err)
	}

	if afterFilenode != beforeFilenode {
		t.Fatalf("domain_event was rewritten: relfilenode moved from %d to %d, so the migration did touch the audit stream",
			beforeFilenode, afterFilenode)
	}
	if afterCtid != beforeCtid {
		t.Fatalf("the pre-existing event moved from ctid %s to %s: the row was rewritten, which is exactly what append-only forbids",
			beforeCtid, afterCtid)
	}

	// The row reads back with the new columns filled from the catalog
	// default, without ever having been updated. No backfill is the point.
	var version int
	var producer string
	var subject, idempotencyKey *string
	if err := conn.QueryRow(ctx,
		`select version, producer, subject::text, idempotency_key from domain_event where id = $1`, eventID).
		Scan(&version, &producer, &subject, &idempotencyKey); err != nil {
		t.Fatalf("reading the migrated event: %v", err)
	}
	if version != 1 {
		t.Errorf("pre-existing event has version %d, want 1", version)
	}
	if producer != "news" {
		t.Errorf("pre-existing event has producer %q, want news - it was written before any other producer existed", producer)
	}
	if subject != nil || idempotencyKey != nil {
		t.Error("pre-existing event gained a subject or an idempotency key: the migration invented routing it cannot know")
	}
}

// TestDomainEventStaysAppendOnlyAfterTheEnvelope closes the loop: whatever
// the migration did, the guarantee it must not have broken is still there.
func TestDomainEventStaysAppendOnlyAfterTheEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "update the payload", stmt: `update domain_event set payload = '{}'::jsonb where id = $1`},
		{name: "update the new producer column", stmt: `update domain_event set producer = 'cashback' where id = $1`},
		{name: "update the new version column", stmt: `update domain_event set version = 2 where id = $1`},
		{name: "claim an idempotency key after the fact", stmt: `update domain_event set idempotency_key = 'late' where id = $1`},
		{name: "delete the event", stmt: `delete from domain_event where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			var eventID string
			if err := tx.QueryRow(ctx,
				`insert into domain_event (type, payload, producer, version, subject, idempotency_key)
				 values ('cashback.entry.credited', '{"k":"v"}'::jsonb, 'cashback', 1, gen_random_uuid(), $1)
				 returning id`, "key-"+randomSuffix(t)).Scan(&eventID); err != nil {
				t.Fatalf("seed domain_event: %v", err)
			}
			_, err := tx.Exec(ctx, tt.stmt, eventID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestDomainEventRedeliveryIsANoOp asserts the point of the idempotency
// key: appending the same event twice fails on the partial unique index
// rather than duplicating the event, which is what makes at-least-once
// delivery harmless (D10).
func TestDomainEventRedeliveryIsANoOp(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	key := "cashback.entry.credited:" + randomSuffix(t)
	const appendEvent = `insert into domain_event (type, payload, producer, subject, idempotency_key)
	                 values ('cashback.entry.credited', '{"k":"v"}'::jsonb, 'cashback', gen_random_uuid(), $1)`
	if _, err := tx.Exec(ctx, appendEvent, key); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// In a savepoint: the redelivery is expected to fail, and an aborted
	// transaction would refuse every statement after it (25P02) - including
	// the unkeyed pair and the second producer below, which are the other
	// halves of what this test is about.
	wantPgCode(t, execInSavepoint(t, tx, appendEvent, key), codeUniqueViolation)

	// Two events with no key do not collide: the index is partial, so the
	// writers that have no delivery to deduplicate are simply not in it.
	const unkeyed = `insert into domain_event (type, payload) values ('item.retrieved', '{}'::jsonb)`
	if _, err := tx.Exec(ctx, unkeyed); err != nil {
		t.Fatalf("first unkeyed event: %v", err)
	}
	if _, err := tx.Exec(ctx, unkeyed); err != nil {
		t.Fatalf("second unkeyed event was rejected: the idempotency index is not partial: %v", err)
	}

	// And the scoping half: the same key from a DIFFERENT producer is a
	// different event, because the key is producer-chosen. Without this,
	// two products picking the same obvious string - and
	// "entry.credited:2026-08" is not an exotic choice - would silently
	// block each other's appends with something that looks exactly like a
	// redelivery.
	if _, err := tx.Exec(ctx,
		`insert into domain_event (type, payload, producer, subject, idempotency_key)
		 values ('news.article.approved', '{"k":"v"}'::jsonb, 'news', gen_random_uuid(), $1)`,
		key); err != nil {
		t.Fatalf("another producer was blocked by the same idempotency key: the index is not scoped by producer: %v", err)
	}
}

// domainEventInsertColumns reads the column list an existing writer uses,
// from the committed file itself. Reading the file rather than a copy of it
// is the point: 0018's promise is that these two writers keep working
// UNMODIFIED, and a promise about committed code has to be checked against
// committed code.
func domainEventInsertColumns(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the writer at %s: %v", path, err)
	}
	match := regexp.MustCompile(`insert into domain_event\s*\(([^)]*)\)`).FindSubmatch(source)
	if match == nil {
		t.Fatalf("%s no longer writes to domain_event; 0018's promise was about this writer", path)
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(string(match[1]), ",", " ")), ", ")
}

// TestExistingDomainEventWritersKeepWorkingUnmodified asserts what the
// defaults were chosen for. Both writers name only (type, payload); an
// insert naming only those columns is accepted, and the envelope is filled
// with the values that are correct for the news product.
func TestExistingDomainEventWritersKeepWorkingUnmodified(t *testing.T) {
	t.Parallel()

	writers := []struct {
		name string
		path string
	}{
		{name: "ingestion", path: "../../ingestion/store.go"},
		{name: "editorial", path: "../../editorial/queries/approval.sql"},
	}

	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			columns := domainEventInsertColumns(t, w.path)
			if columns != "type, payload" {
				t.Fatalf("%s now writes domain_event (%s): 0018 promised this writer would keep working unmodified, and that promise is about the column list it names",
					w.path, columns)
			}

			tx := beginTx(t)
			ctx := context.Background()
			var version int
			var producer string
			var subject, key *string
			err := tx.QueryRow(ctx,
				`insert into domain_event (`+columns+`) values ($1, $2::jsonb)
				 returning version, producer, subject::text, idempotency_key`,
				"item.retrieved", `{"source_item_id":"x"}`).
				Scan(&version, &producer, &subject, &key)
			if err != nil {
				t.Fatalf("the unmodified writer's insert was rejected: %v", err)
			}
			if version != 1 || producer != "news" {
				t.Fatalf("the unmodified writer produced version %d and producer %q, want 1 and news", version, producer)
			}
			if subject != nil || key != nil {
				t.Error("the unmodified writer produced a subject or an idempotency key it never supplied")
			}
		})
	}
}
