// Package db provides Postgres connectivity and schema migrations. The
// embedded migration files are the single source of truth for the schema;
// sqlc (Go types) and supabase gen types (TypeScript types) both read them.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect opens a pgx connection pool and verifies it with a ping.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// LatestMigration reports the highest schema version this build carries.
//
// It exists to make a rollback DECIDABLE FROM OUTSIDE the process. An image
// whose migrations stop at 38 cannot run against a database at 39 - the api
// migrates on boot and a schema does not roll back with an image - and the
// deployment host has no other way to learn that before it swaps the
// containers over. deploy/hetzner/bin/apivo-reconcile asks each image this
// before it attempts a rollback.
func LatestMigration() (uint, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("db: load migrations: %w", err)
	}
	defer func() { _ = src.Close() }()
	return latestVersion(src)
}

// latestVersion walks a source to its last version. Shared with Migrate so
// that the number reported to a host and the number used to explain a failed
// migration cannot disagree.
func latestVersion(src source.Driver) (uint, error) {
	version, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("db: this build embeds no migrations at all: %w", err)
	}
	for {
		next, err := src.Next(version)
		if err != nil {
			// A source signals "there is no next one" with a not-exist
			// error rather than a sentinel, so anything else is a real
			// failure to read the embedded set and must not be reported as
			// the end of it.
			if errors.Is(err, fs.ErrNotExist) {
				return version, nil
			}
			return 0, fmt.Errorf("db: reading the migration after %d: %w", version, err)
		}
		version = next
	}
}

// AppliedVersion reports the schema version recorded in the database, and
// whether the migration that last touched it left the schema dirty.
//
// Zero means no migration has ever run here, which is not an error: it is a
// database waiting for its first one.
func AppliedVersion(databaseURL string) (uint, bool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return 0, false, fmt.Errorf("db: parse config: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	defer func() { _ = sqlDB.Close() }()

	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		return 0, false, fmt.Errorf("db: migration driver: %w", err)
	}
	current, dirty, err := driver.Version()
	if err != nil {
		return 0, false, fmt.Errorf("db: reading the applied schema version: %w", err)
	}
	// NilVersion is negative, and means the migrations table exists with
	// nothing in it - or does not exist yet.
	if current < 0 {
		return 0, dirty, nil
	}
	return uint(current), dirty, nil
}

// Migrate applies all pending schema migrations. It is safe to call on every
// start: an up-to-date schema is a no-op.
func Migrate(databaseURL string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("db: load migrations: %w", err)
	}
	// Parsed as a POOL config and opened from the connection half of it,
	// rather than handed to sql.Open whole.
	//
	// pool_max_conns and its siblings are pgxpool's own DSN parameters:
	// pgxpool.ParseConfig understands them, and the database/sql driver does
	// not - it forwards whatever it does not recognise to the server as a
	// runtime setting, which answers FATAL: unrecognized configuration
	// parameter. So the one DSN this process is given has to be read the
	// same way in both places, or raising the pool size makes the migration
	// that runs first refuse to connect at all. That is not hypothetical:
	// registering the network sweeps takes the pool requirement past pgx's
	// default of four (T057), and pool_max_conns in DATABASE_URL is the only
	// way to raise it.
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("db: parse config: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("db: migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("db: migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", explainIfBehind(src, driver, err))
	}
	return nil
}

// explainIfBehind replaces golang-migrate's account of its own internals with
// an account of the situation, for the one failure where the two are furthest
// apart.
//
// A database ahead of the running build reports:
//
//	no migration found for version 39: read down for version 39 migrations: file does not exist
//
// which describes a source driver looking for a down file, and says nothing
// about what actually happened: this image was rolled back past a migration.
// The api migrates on boot, so a deploy that migrated and then failed its
// health check leaves every earlier image unable to start - and an operator
// reading that line has no way to reach that conclusion from it.
//
// Anything else is returned untouched. A message that guessed would be worse
// than golang-migrate's, which is at least accurate about what it tried.
func explainIfBehind(src source.Driver, driver database.Driver, err error) error {
	latest, lerr := latestVersion(src)
	if lerr != nil {
		return err
	}
	current, _, verr := driver.Version()
	if verr != nil || current < 0 || uint(current) <= latest {
		return err
	}
	return fmt.Errorf(
		"the database is at schema version %d and this build carries migrations only up to %d, so it has been rolled back past a migration - and a schema does not roll back with an image. Roll forward to a build that carries version %d, or take the schema down deliberately before deploying this one: %w",
		current, latest, current, err)
}
