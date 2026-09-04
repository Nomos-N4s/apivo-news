// The `connect-network` command (T145): the supported way to write the two
// rows a deployment cannot ingest without.
//
// A file of its own because it is the only subcommand that opens the
// database, and the only one whose whole job is a single write an operator
// would otherwise do by hand against production.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// connectNetworkName is the subcommand as an operator types it.
const connectNetworkName = "connect-network"

// backfillFromLayouts are the ways a date may be written on the command
// line: a plain day, or a full instant for an operator who means a
// particular one. A day is read as UTC midnight, which is the reading that
// cannot silently differ between the operator's laptop and the container.
var backfillFromLayouts = []string{time.RFC3339, "2006-01-02"}

// credentialRefKey is what the account row records as the place this
// deployment reads its credential from. It is a KEY INTO CONFIGURATION and
// never a credential (ADR-0003), and it is this binary's own key rather than
// a flag because a row naming a key this binary does not read would describe
// an account nothing could authenticate as.
const credentialRefKey = "NETWORK_API_KEY"

// connectNetworkCommand runs the subcommand: it connects the publisher account this
// deployment is configured for, and is safe to run again.
//
// The network and the account come from the environment rather than from
// flags, deliberately. They are NETWORK_DRIVER and NETWORK_ACCOUNT_ID, the
// same two values the running process resolves its adapter from, so an
// operator cannot connect an account this deployment will not then poll -
// which is the failure this command exists to remove, arrived at from the
// other side.
//
// What is a flag is what is not configuration: where history starts, which
// is a one-time decision about the past, and whether to be live, which is an
// intent expressed at the moment of running.
func connectNetworkCommand(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet(connectNetworkName, flag.ContinueOnError)
	flags.SetOutput(stdout)
	backfillFrom := flags.String("backfill-from", "",
		"where this account's history starts (2026-06-01 or an RFC3339 instant). Required the first time; ignored afterwards, because moving it would leave a span never re-read.")
	inactive := flags.Bool("inactive", false,
		"connect without turning the network on. Without it both rows are made active, which is what lets members click through.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", connectNetworkName, rest)
	}

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	documented, err := documentedNetwork(cfg.Cashback.Network.Driver)
	if err != nil {
		return err
	}
	if cfg.Cashback.Network.AccountID == "" {
		return errors.New("NETWORK_ACCOUNT_ID names no publisher account, and it is the account this would connect")
	}
	start, err := parseBackfillFrom(*backfillFrom)
	if err != nil {
		return err
	}

	pool, err := platformdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// One transaction, because the two rows are one fact: a network
	// committed without its account leaves a deployment configured for a
	// network it has no account at, which is the state this command exists
	// to remove.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", connectNetworkName, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	connection, err := networks.ConnectPublisherAccount(ctx, tx, networks.ConnectRequest{
		Network:             documented,
		ExternalPublisherID: cfg.Cashback.Network.AccountID,
		CredentialRef:       credentialRefKey,
		BackfillFrom:        start,
		Active:              !*inactive,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: %w", connectNetworkName, err)
	}

	return reportConnection(stdout, connection, start)
}

// parseBackfillFrom reads the flag, allowing it to be absent - a re-run does
// not have to restate a date the command could not change anyway, and
// [networks.ConnectPublisherAccount] is what refuses a NEW account without
// one, at the point it knows the account is new.
func parseBackfillFrom(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range backfillFromLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("-backfill-from %s is not a date: write 2026-06-01 or an RFC3339 instant",
		strconv.Quote(value))
}

// reportConnection prints what the rows now say, which is not always what was
// asked for. The one place those differ is the history start on an account
// that already had one, and an operator who typed a date has to be told
// plainly that it did not move rather than left to assume it did.
func reportConnection(stdout io.Writer, c networks.Connection, asked time.Time) error {
	state := "active"
	if !c.Active {
		state = "inactive"
	}
	if _, err := fmt.Fprintf(stdout, "%s publisher account %s at %s (%s)\n",
		madeOrFound(c.AccountCreated, "connected", "updated"),
		strconv.Quote(c.Account.ExternalID()), strconv.Quote(c.Account.Network().String()), state); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "  network row     %s\n", madeOrFound(c.NetworkCreated, "created", "already there")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "  account row     %s (%s)\n",
		madeOrFound(c.AccountCreated, "created", "already there"), c.Account.ID()); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "  credential key  %s (the value is never stored)\n", credentialRefKey); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "  history from    %s\n", c.BackfillFrom.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if !asked.IsZero() && !asked.Equal(c.BackfillFrom) {
		// Not a warning about a mistake: moving it is what 0023 forbids,
		// because the span between the old start and the new one would never
		// be re-read and every transaction in it would sit pending forever.
		_, err := fmt.Fprintf(stdout,
			"  note            -backfill-from %s was NOT applied: this account is already polling from %s, and moving that would leave the span between them never re-read\n",
			asked.UTC().Format(time.RFC3339), c.BackfillFrom.UTC().Format(time.RFC3339))
		return err
	}
	return nil
}

// madeOrFound picks the word for what a run did, so a re-run reads as a
// re-run rather than as a first connect.
func madeOrFound(created bool, made, found string) string {
	if created {
		return made
	}
	return found
}

// documentedNetwork resolves NETWORK_DRIVER to what that network publishes
// about itself - the facts a cashback.network row is seeded with.
//
// It reads [shippedNetworks], the same map [networkAdapter] reads, because
// this used to be a second switch and the two disagreed: this one knew awin
// and that one did not, so a deployment could be seeded for a network the
// binary then refused to start against. Seedable and servable are now one
// answer by construction.
func documentedNetwork(driver string) (networks.Documented, error) {
	entry, err := lookupNetwork(driver)
	if err != nil {
		return networks.Documented{}, err
	}
	return entry.documented(), nil
}
