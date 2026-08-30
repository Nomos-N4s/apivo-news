// Package db provides Postgres connectivity and schema migrations. The
// embedded migration files are the single source of truth for the schema;
// sqlc (Go types) and supabase gen types (TypeScript types) both read them.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
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
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}
