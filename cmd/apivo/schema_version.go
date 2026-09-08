package main

import (
	"flag"
	"fmt"
	"io"

	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

const schemaVersionName = "schema-version"

// schemaVersionCommand prints the highest schema version this build carries,
// or - with --applied - the version the database is actually at.
//
// It exists for the deployment host, which has to decide whether a rollback
// can work BEFORE it swaps the containers over. The api migrates on boot, so
// a deploy that migrated and then failed its health check leaves the database
// ahead of every earlier image; rolling back to one of those replaces a
// broken container with an unbootable one. Comparing these two numbers is the
// only way to know that from outside the process.
// See deploy/hetzner/bin/apivo-reconcile.
//
// DATABASE_URL is read straight from the environment rather than through
// config.FromEnv, and that is deliberate. This command has to answer on a
// deployment that is already broken - very often one the api itself refuses
// to start on - and a diagnostic that fails because an unrelated key is
// missing is a diagnostic nobody can use at the moment they need it. It opens
// a connection, reads one row and exits; none of the rules FromEnv enforces
// about how this process should SERVE apply to that.
func schemaVersionCommand(args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet(schemaVersionName, flag.ContinueOnError)
	flags.SetOutput(stdout)
	applied := flags.Bool("applied", false,
		"print the version the DATABASE is at rather than the one this build carries. Needs DATABASE_URL.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", schemaVersionName, rest)
	}

	if !*applied {
		carried, err := platformdb.LatestMigration()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%d\n", carried)
		return err
	}

	url := getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("%s --applied needs DATABASE_URL, and this process was given none", schemaVersionName)
	}
	version, dirty, err := platformdb.AppliedVersion(url)
	if err != nil {
		return err
	}
	// A dirty schema is not a version, it is a half-applied migration, and
	// reporting the number alone would let a caller conclude a rollback is
	// safe when nothing can start against this database at all.
	if dirty {
		return fmt.Errorf("%s: the database is at version %d and DIRTY - a migration failed part way through, and nothing will start against it until somebody forces it to a version they have checked",
			schemaVersionName, version)
	}
	_, err = fmt.Fprintf(stdout, "%d\n", version)
	return err
}
