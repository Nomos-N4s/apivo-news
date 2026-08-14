// Command apivo is the epiloYES backend: a single binary containing every
// module of the modular monolith. It is also the composition root - the one
// place where modules are wired together.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
)

func main() {
	if err := run(context.Background(), os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "apivo:", err)
		os.Exit(1)
	}
}

// run wires the platform together and serves until ctx is cancelled or a
// termination signal arrives. It is separated from main so the wiring is
// testable; main only handles the exit code.
func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := logging.New(stdout, cfg.LogLevel, cfg.Env)

	if err := platformdb.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := platformdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := platformhttp.New(log, cfg.HTTPAddr, readiness(pool))
	log.InfoContext(ctx, "starting", "addr", cfg.HTTPAddr, "env", cfg.Env)
	return srv.Run(ctx)
}

// readiness reports the process ready when the database answers a ping.
func readiness(pool *pgxpool.Pool) platformhttp.ReadinessCheck {
	return func(ctx context.Context) error {
		return pool.Ping(ctx)
	}
}
