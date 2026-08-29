package db

// Down migrations rot. Nothing in normal operation ever runs one - Migrate
// only goes up - so a forgotten DROP, or a dependency the author never
// re-tested, surfaces for the first time during an incident rollback, which
// is the worst possible moment to learn about it. This file keeps the whole
// set honest: every migration is driven up ONE AT A TIME and then back down
// one at a time against a scratch database, and after each down step the
// catalog must be exactly what that migration's up found when it started.
//
// Stepping is the whole point, and the reason this is not simply "migrate
// down and check the database is empty". Emptiness at the bottom cannot see
// a forgotten DROP inside a schema a LATER down migration drops wholesale:
// 0010's down drops the `cashback` schema with CASCADE, so a table 0013 or
// 0017 forgot to drop would vanish with the schema and an emptiness check
// would still pass. Comparing at each step asks the only question that
// cannot be answered by something further down the stack - did THIS
// migration's down undo THIS migration's up - and it catches the reverse
// defect too, a down that removes something it did not create.
//
// It deliberately lives in package db rather than db_test with its
// siblings: the migrations are discovered by walking the embedded FS
// (migrationsFS, unexported), so a newly committed migration is covered
// immediately, with no version range for anyone to remember to extend.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// runnerFileName is what the migration runner itself parses, copied from
// golang-migrate's source.Regex: a version, an underscore, ANY identifier,
// the direction, and ANY extension. A file that fails this is invisible to
// the runner - iofs skips whatever it cannot parse, without a word - so it
// is a migration that will never run.
var runnerFileName = regexp.MustCompile(`^([0-9]+)_(.*)\.(up|down)\.(.*)$`)

// migrationFileName is this repository's convention, which is deliberately
// narrower than the runner's: lower snake_case, and `.sql`. A name that
// satisfies the runner but not this WOULD run; it is just inconsistent with
// every other file in the directory. The two are checked separately because
// they are different defects and only one of them is silent.
var migrationFileName = regexp.MustCompile(`^[0-9]+_[a-z0-9_]+\.(up|down)\.sql$`)

// catalogSnapshotQuery renders the whole database as sorted text, one line
// per object, so two states can be compared by set difference. Enumerating
// the catalogs rather than checking for known names is what keeps this
// honest as migrations are added: nothing here needs to change when a
// migration introduces an object nobody anticipated.
//
// It goes well past "does the table exist": columns with their types,
// defaults and nullability, indexes, constraints, triggers, routine bodies,
// enum labels, ACLs and default privileges. A down migration that restores
// a trigger function with the wrong body is the same class of rot as a
// forgotten DROP, and shows up here as the same kind of difference.
//
// Three things are deliberately outside the snapshot, each for a stated
// reason:
//
//   - schema_migrations: the version table belongs to the migration runner,
//     not to any migration. It is created before the first up runs and
//     survives the last down, which is exactly the behaviour that would make
//     it look like a leftover here.
//   - extension-owned objects (the pg_depend 'e' exclusions), and the
//     extensions themselves: 0001 installs pgcrypto and documents why
//     rollback keeps it - extensions are shared infrastructure a migration
//     reuses, never owns.
//   - roles: they are cluster-wide, and 0010's down documents why the drop
//     is best-effort. A database-scoped snapshot cannot say anything about
//     them. Grants TO those roles are in scope, because those live in this
//     database.
const catalogSnapshotQuery = `
with extension_owned as (
    select classid, objid
      from pg_depend
     where refclassid = 'pg_extension'::regclass
       and deptype = 'e'
),
target as (
    select oid, nspname, nspacl, nspowner
      from pg_namespace
     where nspname <> 'information_schema'
       and nspname not like 'pg\_%'
)
select item from (
    select 'schema ' || n.nspname
           || ' granted=' || coalesce(
                  (select string_agg(g.grantee::regrole::text || ':' || g.privilege_type, ',' order by g.grantee::regrole::text || ':' || g.privilege_type)
                     from aclexplode(n.nspacl) g
                    where g.grantee <> n.nspowner), '(owner only)') as item
      from target n

    union all
    select 'relation ' || n.nspname || '.' || c.relname
           || ' kind=' || c.relkind::text
           || ' granted=' || coalesce(
                  (select string_agg(g.grantee::regrole::text || ':' || g.privilege_type, ',' order by g.grantee::regrole::text || ':' || g.privilege_type)
                     from aclexplode(c.relacl) g
                    where g.grantee <> c.relowner), '(owner only)')
      from pg_class c
      join target n on n.oid = c.relnamespace
     where c.relkind in ('r', 'p', 'v', 'm', 'S', 'f')
       and c.relname <> 'schema_migrations'
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_class'::regclass and e.objid = c.oid)

    union all
    select 'column ' || n.nspname || '.' || c.relname || '.' || a.attname
           || ' type=' || format_type(a.atttypid, a.atttypmod)
           || ' notnull=' || a.attnotnull::text
           || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '(none)')
           || ' identity=' || coalesce(nullif(a.attidentity::text, ''), '(none)')
           || ' generated=' || coalesce(nullif(a.attgenerated::text, ''), '(none)')
      from pg_attribute a
      join pg_class c on c.oid = a.attrelid
      join target n on n.oid = c.relnamespace
      left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
     where c.relkind in ('r', 'p', 'v', 'm', 'f')
       and c.relname <> 'schema_migrations'
       and a.attnum > 0
       and not a.attisdropped
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_class'::regclass and e.objid = c.oid)

    union all
    select 'view ' || n.nspname || '.' || c.relname
           || ' definition=' || md5(pg_get_viewdef(c.oid))
      from pg_class c
      join target n on n.oid = c.relnamespace
     where c.relkind in ('v', 'm')
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_class'::regclass and e.objid = c.oid)

    union all
    select 'index ' || n.nspname || '.' || i.relname
           || ' ' || pg_get_indexdef(x.indexrelid)
      from pg_index x
      join pg_class i on i.oid = x.indexrelid
      join pg_class c on c.oid = x.indrelid
      join target n on n.oid = i.relnamespace
     where c.relname <> 'schema_migrations'
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_class'::regclass and e.objid = i.oid)

    union all
    select 'constraint ' || n.nspname || '.'
           || coalesce(c.relname, t.typname, '(schema)') || '.' || k.conname
           || ' ' || pg_get_constraintdef(k.oid)
      from pg_constraint k
      join target n on n.oid = k.connamespace
      left join pg_class c on c.oid = k.conrelid
      left join pg_type t on t.oid = k.contypid
     where coalesce(c.relname, '') <> 'schema_migrations'
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_constraint'::regclass and e.objid = k.oid)

    union all
    select 'trigger ' || n.nspname || '.' || c.relname || '.' || g.tgname
           || ' ' || pg_get_triggerdef(g.oid)
      from pg_trigger g
      join pg_class c on c.oid = g.tgrelid
      join target n on n.oid = c.relnamespace
     where c.relname <> 'schema_migrations'
       and not g.tgisinternal

    union all
    select 'routine ' || n.nspname || '.' || p.proname
           || '(' || pg_get_function_identity_arguments(p.oid) || ')'
           || ' kind=' || p.prokind::text
           || ' returns=' || pg_get_function_result(p.oid)
           || ' body=' || md5(coalesce(p.prosrc, ''))
      from pg_proc p
      join target n on n.oid = p.pronamespace
     where not exists (select 1 from extension_owned e
                        where e.classid = 'pg_proc'::regclass and e.objid = p.oid)

    union all
    select 'type ' || n.nspname || '.' || t.typname || ' typtype=' || t.typtype::text
      from pg_type t
      join target n on n.oid = t.typnamespace
     where t.typtype in ('d', 'e', 'r', 'm')
       and not exists (select 1 from extension_owned e
                        where e.classid = 'pg_type'::regclass and e.objid = t.oid)

    union all
    select 'enum label ' || n.nspname || '.' || t.typname || '.' || l.enumlabel
      from pg_enum l
      join pg_type t on t.oid = l.enumtypid
      join target n on n.oid = t.typnamespace

    union all
    -- ALTER DEFAULT PRIVILEGES outlives the schema it was declared in
    -- unless the down migration un-declares it in the same shape (0010's
    -- down says exactly this); a fresh database has no entries at all.
    select 'default privileges ' || coalesce(n.nspname, '(database-wide)')
           || ' for ' || pg_get_userbyid(d.defaclrole)
           || ' on ' || d.defaclobjtype::text
           || ' = ' || coalesce(d.defaclacl::text, '')
      from pg_default_acl d
      left join pg_namespace n on n.oid = d.defaclnamespace
) catalog
order by item`

// embeddedMigrationVersions walks the embedded migration files and returns
// the sorted versions, requiring every version to carry exactly one up and
// one down file. An up without a down is a schema change that cannot be
// rolled back, which is the rot this whole file exists to catch.
//
// Pairing is judged by what the RUNNER can parse, not by this repository's
// narrower convention, because a file the runner accepts is a file that
// runs whether or not it is named the way the rest of the directory is.
func embeddedMigrationVersions(t *testing.T) []uint64 {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	directions := make(map[uint64]map[string]int)
	for _, entry := range entries {
		name := entry.Name()
		match := runnerFileName.FindStringSubmatch(name)
		if match == nil {
			t.Errorf("%s does not match <version>_<title>.<up|down>.<ext>: the migration runner cannot parse it, and iofs skips what it cannot parse without a word, so this file would never run", name)
			continue
		}
		if !migrationFileName.MatchString(name) {
			// It would run. It is simply not named the way everything
			// else here is, and a directory read in version order is
			// only readable while the names are uniform.
			t.Errorf("%s violates this repository's naming convention <version>_<lower_snake_case>.<up|down>.sql: the runner would still run it, so this is an inconsistency rather than an invisible file", name)
		}
		version, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			t.Errorf("%s: parsing version: %v", name, err)
			continue
		}
		if directions[version] == nil {
			directions[version] = make(map[string]int)
		}
		directions[version][match[3]]++
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

// newScratchDatabase creates an empty database of its own and returns a URL
// pointing at it. It is dropped again when the test ends.
//
// The name carries a random suffix because this suite is not the only thing
// that may be pointed at a server: two invocations against one Postgres -
// a developer's run beside a CI run, or the same package run twice - would
// otherwise drop and recreate each other's database mid-round-trip and
// report the wreckage as migration rot.
func newScratchDatabase(t *testing.T, baseURL *url.URL) string {
	t.Helper()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("naming the scratch database: %v", err)
	}
	name := fmt.Sprintf("apivo_migrate_roundtrip_%d_%s", os.Getpid(), hex.EncodeToString(suffix))
	quoted := pgx.Identifier{name}.Sanitize()

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseURL.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	// Force-dropped first: a previous run that failed mid-rollback leaves a
	// half-migrated database behind, and this test must start from nothing
	// to mean anything. With a random name that only fires on a collision,
	// but being wrong about it would be silent.
	if _, err := admin.Exec(ctx, "drop database if exists "+quoted+" with (force)"); err != nil {
		t.Fatalf("dropping scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatalf("creating scratch database: %v", err)
	}
	t.Cleanup(func() {
		// A fresh connection, because the pool above is closed by now. A
		// failure here is logged rather than failed: a leaked scratch
		// database is untidy, and reporting it as a migration defect
		// would be worse.
		cleanupCtx := context.Background()
		cleaner, err := pgxpool.New(cleanupCtx, baseURL.String())
		if err != nil {
			t.Logf("dropping scratch database %s: connecting: %v", name, err)
			return
		}
		defer cleaner.Close()
		if _, err := cleaner.Exec(cleanupCtx, "drop database if exists "+quoted+" with (force)"); err != nil {
			t.Logf("dropping scratch database %s: %v", name, err)
		}
	})

	scratch := *baseURL
	scratch.Path = "/" + name
	return scratch.String()
}

// newRoundTripMigrator builds the same migrate instance Migrate uses, but
// hands it back so the test can drive Steps and Version - operations the
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

// catalogSnapshot renders the scratch database as a sorted list of object
// descriptions.
func catalogSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, catalogSnapshotQuery)
	if err != nil {
		t.Fatalf("snapshotting the catalogs: %v", err)
	}
	snapshot, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("reading the catalog snapshot: %v", err)
	}
	return snapshot
}

// wantSameCatalog reports every way two snapshots differ. The two
// directions are reported separately because they are different defects: an
// object that survived is a forgotten DROP, and one that went missing is a
// down migration reaching past its own migration's work.
func wantSameCatalog(t *testing.T, want, got []string, survived, lost string) {
	t.Helper()
	wanted := make(map[string]struct{}, len(want))
	for _, object := range want {
		wanted[object] = struct{}{}
	}
	present := make(map[string]struct{}, len(got))
	for _, object := range got {
		present[object] = struct{}{}
	}
	for _, object := range got {
		if _, ok := wanted[object]; !ok {
			t.Errorf("%s: %s", survived, object)
		}
	}
	for _, object := range want {
		if _, ok := present[object]; !ok {
			t.Errorf("%s: %s", lost, object)
		}
	}
}

// TestMigrationsRoundTrip proves the embedded migration set is complete in
// both directions: the files pair up with no gaps, and every migration's
// down puts a real database back exactly as its up found it.
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

	t.Run("each down restores the catalog its up found", func(t *testing.T) {
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
		scratchURL := newScratchDatabase(t, u)
		pool, err := pgxpool.New(ctx, scratchURL)
		if err != nil {
			t.Fatalf("connecting to scratch database: %v", err)
		}
		defer pool.Close()

		m := newRoundTripMigrator(t, scratchURL)

		// Up one version at a time, keeping the catalog each migration
		// INHERITED. That snapshot is the only correct answer to "what
		// should this migration's down leave behind", and it has to be
		// taken on the way up: once the whole stack is applied it cannot
		// be reconstructed.
		inherited := make([][]string, len(versions))
		for i, version := range versions {
			inherited[i] = catalogSnapshot(ctx, t, pool)
			if err := m.Steps(1); err != nil {
				t.Fatalf("up to %04d: %v", version, err)
			}
			wantVersion(t, m, version, fmt.Sprintf("after %04d's up", version))
		}
		fullyMigrated := catalogSnapshot(ctx, t, pool)

		// Down one version at a time, newest first, comparing after every
		// step. "It ran" is not "it worked": a down migration that drops
		// half of what its up created still exits cleanly.
		for i := len(versions) - 1; i >= 0; i-- {
			version := versions[i]
			if err := m.Steps(-1); err != nil {
				t.Fatalf("down from %04d: %v", version, err)
			}
			if i > 0 {
				wantVersion(t, m, versions[i-1], fmt.Sprintf("after %04d's down", version))
			}
			wantSameCatalog(t, inherited[i], catalogSnapshot(ctx, t, pool),
				fmt.Sprintf("%04d's down left behind what %04d's up created", version, version),
				fmt.Sprintf("%04d's down removed something that existed before %04d ran", version, version))
		}
		// The last down took the runner past its first version, which is
		// where a database that has never been migrated sits.
		if _, _, err := m.Version(); !errors.Is(err, migrate.ErrNilVersion) {
			t.Fatalf("version after the last down: want ErrNilVersion, got %v", err)
		}

		if err := m.Up(); err != nil {
			t.Fatalf("Up after the rollback: %v", err)
		}
		wantVersion(t, m, last, "after the second Up")

		// And the rebuilt schema must be the same schema, not merely one
		// the runner is willing to call up to date.
		wantSameCatalog(t, fullyMigrated, catalogSnapshot(ctx, t, pool),
			"the rebuilt schema carries an object the first pass did not",
			"the rebuilt schema is missing an object the first pass had")

		// Nothing left to apply, as far as the runner is concerned.
		if err := m.Up(); !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("Up on an up-to-date database: want ErrNoChange, got %v", err)
		}
	})
}
