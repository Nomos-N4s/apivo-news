package db

// Down migrations rot. Nothing in normal operation ever runs one - Migrate
// only goes up - so a forgotten DROP, or a dependency the author never
// re-tested, surfaces for the first time during an incident rollback, which
// is the worst possible moment to learn about it. This file keeps the whole
// set honest: every migration is driven up, all the way back down, and up
// again against a scratch database, and the set itself must be well-formed.
//
// It deliberately lives in package db rather than db_test with its
// siblings: the migrations are discovered by walking the embedded FS
// (migrationsFS, unexported), so a newly committed migration is covered
// immediately, with no version range for anyone to remember to extend.

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFileName matches the naming scheme the iofs source driver
// parses: version, snake_case title, direction. A file that does not match
// is not a stray - it is a migration the runner would silently ignore - so
// the test fails on it rather than skipping it.
var migrationFileName = regexp.MustCompile(`^([0-9]+)_[a-z0-9_]+\.(up|down)\.sql$`)

// Objects a fully rolled-back database may still carry, enumerated straight
// from the catalogs rather than by checking for known names, so a leftover
// from a future migration is caught without this test changing.
//
// Three things are deliberately tolerated:
//
//   - schema_migrations: the version table belongs to the migration runner,
//     not to any migration, and survives a rollback to version 0 by design.
//   - extension-owned objects (the pg_depend 'e' exclusion): 0001 installs
//     pgcrypto and documents why rollback keeps it - extensions are shared
//     infrastructure a migration reuses, never owns.
//   - roles: they are cluster-wide, and 0010's down documents why the drop
//     is best-effort. A database-scoped emptiness check cannot say anything
//     about them.
const leftoverSchemasQuery = `
select nspname
  from pg_namespace
 where nspname <> 'public'
   and nspname <> 'information_schema'
   and nspname not like 'pg\_%'
 order by nspname`

const leftoverObjectsQuery = `
select what from (
    select case c.relkind
               when 'r' then 'table '
               when 'p' then 'partitioned table '
               when 'v' then 'view '
               when 'm' then 'materialized view '
               when 'S' then 'sequence '
               else 'foreign table '
           end || c.relname as what
      from pg_class c
      join pg_namespace n on n.oid = c.relnamespace
     where n.nspname = 'public'
       and c.relkind in ('r', 'p', 'v', 'm', 'S', 'f')
       and c.relname <> 'schema_migrations'
       and not exists (
               select 1
                 from pg_depend d
                where d.classid = 'pg_class'::regclass
                  and d.objid = c.oid
                  and d.refclassid = 'pg_extension'::regclass
                  and d.deptype = 'e')
    union all
    select case p.prokind when 'p' then 'procedure ' else 'function ' end
               || p.proname
      from pg_proc p
      join pg_namespace n on n.oid = p.pronamespace
     where n.nspname = 'public'
       and not exists (
               select 1
                 from pg_depend d
                where d.classid = 'pg_proc'::regclass
                  and d.objid = p.oid
                  and d.refclassid = 'pg_extension'::regclass
                  and d.deptype = 'e')
    union all
    select case t.typtype
               when 'd' then 'domain '
               when 'e' then 'enum '
               when 'r' then 'range '
               else 'multirange '
           end || t.typname
      from pg_type t
      join pg_namespace n on n.oid = t.typnamespace
     where n.nspname = 'public'
       and t.typtype in ('d', 'e', 'r', 'm')
       and not exists (
               select 1
                 from pg_depend d
                where d.classid = 'pg_type'::regclass
                  and d.objid = t.oid
                  and d.refclassid = 'pg_extension'::regclass
                  and d.deptype = 'e')
    union all
    select 'trigger ' || tg.tgname || ' on ' || c.relname
      from pg_trigger tg
      join pg_class c on c.oid = tg.tgrelid
      join pg_namespace n on n.oid = c.relnamespace
     where n.nspname = 'public'
       and not tg.tgisinternal
    union all
    -- ALTER DEFAULT PRIVILEGES outlives the schema it was declared in
    -- unless the down migration un-declares it in the same shape (0010's
    -- down says exactly this); a fresh database has no entries at all.
    select 'default privileges for ' || pg_get_userbyid(d.defaclrole)
      from pg_default_acl d
) leftovers
order by what`

// embeddedMigrationVersions walks the embedded migration files and returns
// the sorted versions, requiring every version to carry exactly one up and
// one down file. An up without a down is a schema change that cannot be
// rolled back, which is the rot this whole file exists to catch.
func embeddedMigrationVersions(t *testing.T) []uint64 {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	directions := make(map[uint64]map[string]int)
	for _, entry := range entries {
		name := entry.Name()
		match := migrationFileName.FindStringSubmatch(name)
		if match == nil {
			t.Errorf("%s does not match <version>_<title>.<up|down>.sql: the migration runner would not see it", name)
			continue
		}
		version, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			t.Errorf("%s: parsing version: %v", name, err)
			continue
		}
		if directions[version] == nil {
			directions[version] = make(map[string]int)
		}
		directions[version][match[2]]++
	}
	versions := make([]uint64, 0, len(directions))
	for version, dirs := range directions {
		for _, direction := range []string{"up", "down"} {
			if n := dirs[direction]; n != 1 {
				t.Errorf("version %04d has %d %s files, want exactly one", version, n, direction)
			}
		}
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions
}

// newRoundTripMigrator builds the same migrate instance Migrate uses, but
// hands it back so the test can drive Down and Version - operations the
// production entry point deliberately does not expose.
func newRoundTripMigrator(t *testing.T, databaseURL string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("opening scratch database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
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

// wantVersion asserts the runner sits cleanly at the given version. Dirty
// is checked before the number because it is the more urgent fact: a dirty
// database means a migration failed partway and left the schema in a state
// no version number describes.
func wantVersion(t *testing.T, m *migrate.Migrate, want uint64, when string) {
	t.Helper()
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version %s: %v", when, err)
	}
	if dirty {
		t.Fatalf("database is dirty %s: a migration failed partway through", when)
	}
	if uint64(version) != want {
		t.Fatalf("version %s: want %04d, got %04d", when, want, version)
	}
}

// wantNoLeftovers enumerates, from the catalogs, everything a migration
// could have left behind in a database rolled all the way down. Checking
// for the absence of known names would go stale the moment a migration adds
// an object under a new name; asking the catalogs what is actually there
// cannot.
func wantNoLeftovers(t *testing.T, scratchURL string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, scratchURL)
	if err != nil {
		t.Fatalf("connecting to scratch database: %v", err)
	}
	defer pool.Close()

	collect := func(query string) []string {
		rows, err := pool.Query(ctx, query)
		if err != nil {
			t.Fatalf("querying catalogs: %v", err)
		}
		leftovers, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("reading catalog rows: %v", err)
		}
		return leftovers
	}

	for _, schema := range collect(leftoverSchemasQuery) {
		t.Errorf("schema %s survived the rollback: a down migration does not drop what its up created", schema)
	}
	for _, object := range collect(leftoverObjectsQuery) {
		t.Errorf("%s survived the rollback in public: a down migration does not drop what its up created", object)
	}
}

// TestMigrationsRoundTrip proves the embedded migration set is complete in
// both directions: the files pair up with no gaps, and a real database can
// be migrated up, rolled back to nothing, and migrated up again.
func TestMigrationsRoundTrip(t *testing.T) {
	t.Parallel()

	// Needs no database, so it runs - and keeps the set honest - even where
	// the round trip below is skipped.
	t.Run("the set is contiguous and paired", func(t *testing.T) {
		versions := embeddedMigrationVersions(t)
		if len(versions) == 0 {
			t.Fatal("no migrations found in the embedded FS")
		}
		for i, version := range versions {
			if want := uint64(i + 1); version != want {
				t.Fatalf("migration versions are not contiguous: want %04d next, found %04d", want, version)
			}
		}
	})

	t.Run("up, down to zero, up again", func(t *testing.T) {
		baseURL := os.Getenv("DATABASE_URL")
		if baseURL == "" {
			t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
		}
		u, err := url.Parse(baseURL)
		if err != nil {
			t.Skipf("DATABASE_URL is not a URL (%v); cannot derive a scratch database", err)
		}

		versions := embeddedMigrationVersions(t)
		if len(versions) == 0 {
			t.Fatal("no migrations found in the embedded FS")
		}
		last := versions[len(versions)-1]

		ctx := context.Background()
		admin, err := pgxpool.New(ctx, baseURL)
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		defer admin.Close()

		// Force-dropped first: a previous run that failed mid-rollback
		// leaves a half-migrated database behind, and this test must start
		// from nothing to mean anything.
		const scratch = "apivo_migrate_roundtrip"
		if _, err := admin.Exec(ctx, "drop database if exists "+scratch+" with (force)"); err != nil {
			t.Fatalf("dropping scratch database: %v", err)
		}
		if _, err := admin.Exec(ctx, "create database "+scratch); err != nil {
			t.Fatalf("creating scratch database: %v", err)
		}

		u.Path = "/" + scratch
		scratchURL := u.String()

		m := newRoundTripMigrator(t, scratchURL)

		if err := m.Up(); err != nil {
			t.Fatalf("first Up: %v", err)
		}
		wantVersion(t, m, last, "after the first Up")

		// Down, not Migrate(1): the first version's down must run too,
		// because it is the one that removes the shared trigger function
		// everything else leans on.
		if err := m.Down(); err != nil {
			t.Fatalf("Down to zero: %v", err)
		}
		if _, _, err := m.Version(); !errors.Is(err, migrate.ErrNilVersion) {
			t.Fatalf("version after Down: want ErrNilVersion, got %v", err)
		}

		// "It ran" is not "it worked": a down migration that drops half of
		// what its up created still exits cleanly, so emptiness is asserted
		// against the catalogs rather than inferred from the version.
		wantNoLeftovers(t, scratchURL)

		if err := m.Up(); err != nil {
			t.Fatalf("Up after Down: %v", err)
		}
		wantVersion(t, m, last, "after the second Up")

		// And the rebuilt schema must be indistinguishable from the first
		// one as far as the runner is concerned: nothing left to apply.
		if err := m.Up(); !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("Up on an up-to-date database: want ErrNoChange, got %v", err)
		}
	})
}
