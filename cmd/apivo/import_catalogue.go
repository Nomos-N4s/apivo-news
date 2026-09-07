// The `import-catalogue` command: one catalogue import, now, by hand.
//
// The scheduled import runs every six hours, and its first run lands at a
// random point in the first tenth of that after the api starts - which, on
// an environment that redeploys on every merge to main, is a first run that
// keeps moving out of reach. An operator who has just connected a network,
// or just watched a retailer join one, should not be waiting on a timer.
// This is the same import, under the same lock, printing what it wrote
// instead of logging it.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// importCatalogueName is the subcommand as an operator types it.
const importCatalogueName = "import-catalogue"

// importedRoutesShown caps how many publishable routes the report lists by
// name. A fixture has one; a real network has hundreds, and a report that
// scrolled them all past would bury the numbers above them that say whether
// the run was reasonable.
const importedRoutesShown = 25

// importCatalogueCommand runs the subcommand: the whole catalogue of the
// network this deployment polls, imported once, in one transaction.
//
// The network comes from the environment and not from a flag, for the
// reason connect-network's does: NETWORKS is what the running process
// resolves its adapter from, so an operator cannot import a catalogue this
// deployment would never issue a click through. There are no other flags,
// because the import has no other decisions in it - what it writes is
// decided by the brand and the source language the api already reads.
func importCatalogueCommand(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet(importCatalogueName, flag.ContinueOnError)
	flags.SetOutput(stdout)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", importCatalogueName, rest)
	}

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	network, wired := theConfiguredNetwork(cfg.Cashback)
	if !wired {
		return fmt.Errorf("%s names no network, and a network is what a catalogue is imported from", config.NetworksKey)
	}
	if missing := network.MissingKeys(); len(missing) > 0 {
		return fmt.Errorf("%s: %s", importCatalogueName,
			config.UnusableNetwork{Driver: network.Driver, Missing: missing})
	}

	pool, err := platformdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The adapter the scheduled job reads with, resolved exactly as the
	// server resolves it. A deployment whose publisher account is not
	// connected yet has nobody to read on behalf of, and is told what to
	// run first.
	adapter, _, err := connectNetwork(ctx, network, pool)
	var off *ingestionOff
	switch {
	case errors.As(err, &off):
		return fmt.Errorf("%s: %s - run %s first, because a catalogue is read on behalf of a publisher account",
			importCatalogueName, off.reason, connectNetworkName)
	case err != nil:
		return err
	}

	// Warnings and worse only, as the seed does: the import's INFO lines
	// describe a scheduled job, and this is a person at a terminal who is
	// about to be told what happened in prose.
	quiet := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	imports, missing, err := newCatalogueImport(quiet, cfg, adapter, pool)
	switch {
	case err != nil:
		return err
	case len(missing) > 0:
		return fmt.Errorf("%s: %s unset - the import needs the language the network writes its catalogue in and the brand that publishes the routes (ADR-0004), the same two keys the scheduled import waits for",
			importCatalogueName, strings.Join(missing, ", "))
	}

	// The scheduler's own lock, so this run and a scheduled one cannot
	// overlap: two imports would each stamp their own start instant, and
	// the slower would withdraw everything the faster had just written.
	lock, held, err := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}).TryLock(ctx, catalogue.ImportJobName)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", importCatalogueName, err)
	case !held:
		return fmt.Errorf("%s: the api is importing this catalogue right now - let it finish and read its log, or run this again in a minute",
			importCatalogueName)
	}
	// Released on a context that outlives a cancelled one: a stranded lock
	// would silently stop the scheduled import for as long as this
	// process's session lasted.
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	result, err := imports.Once(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", importCatalogueName, err)
	}
	routes, err := importedRoutes(ctx, pool, imports.Network().String())
	if err != nil {
		return err
	}
	return reportImport(stdout, imports.Network().String(), result, routes)
}

// importedRoute is one route the catalogue now holds, reduced to what the
// next command needs: the id publish-offer takes, the slug members see, and
// whether a band on it would be clickable.
type importedRoute struct {
	externalID string
	slug       string
	status     string
}

// importedRoutes reads back what the network's catalogue now holds, active
// routes first, so the report describes rows that exist rather than rows
// the run assumed.
func importedRoutes(ctx context.Context, pool *pgxpool.Pool, network string) ([]importedRoute, error) {
	rows, err := pool.Query(ctx, `
		select mn.external_merchant_id, m.slug, mn.status
		  from cashback.merchant_network mn
		  join cashback.merchant m on m.id = mn.merchant_id
		 where mn.network_id = $1
		 order by mn.status <> 'active', mn.external_merchant_id`, network)
	if err != nil {
		return nil, fmt.Errorf("%s: reading back the routes: %w", importCatalogueName, err)
	}
	defer rows.Close()

	var routes []importedRoute
	for rows.Next() {
		var route importedRoute
		if err := rows.Scan(&route.externalID, &route.slug, &route.status); err != nil {
			return nil, fmt.Errorf("%s: reading back the routes: %w", importCatalogueName, err)
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: reading back the routes: %w", importCatalogueName, err)
	}
	return routes, nil
}

// reportImport prints what the run did and what the catalogue now holds.
//
// The numbers come first because they are what an operator reads to decide
// whether the run was reasonable, and the one shape that is not - retailers
// withdrawn and none added - is called out rather than left to be noticed,
// because from here it is indistinguishable from the world having changed.
func reportImport(stdout io.Writer, network string, result catalogue.ImportResult, routes []importedRoute) error {
	lines := []string{
		fmt.Sprintf("imported the %s catalogue: %d retailer(s) seen, %d new, %d departed",
			strconv.Quote(network), result.Seen, result.Created, result.Departed),
		fmt.Sprintf("  started at      %s", result.StartedAt.UTC().Format(time.RFC3339)),
	}
	if result.WithdrewAndAddedNone() {
		lines = append(lines, "  LOOK            this run withdrew retailers and added none, which is what a lost programme approval or an edited filter looks like from here")
	}
	if result.Republished > 0 || result.LostPublication > 0 {
		lines = append(lines, fmt.Sprintf("  published route %d retailer(s) moved to a surviving route, %d left with nothing to publish through",
			result.Republished, result.LostPublication))
	}
	// A retailer that could publish and does not is a state this run could
	// not fix - a route revived by hand, or one on a network this import
	// does not read - and it is named rather than counted, because the
	// operator's next step is to look at that retailer.
	if n := len(result.PublishingNothing); n > 0 {
		lines = append(lines, fmt.Sprintf("  LOOK            %d retailer(s) have a publishable route and publish nothing:", n))
		for i, r := range result.PublishingNothing {
			if i == importedRoutesShown {
				lines = append(lines, fmt.Sprintf("                  ... and %d more", n-i))
				break
			}
			lines = append(lines, fmt.Sprintf("                  %-40s %d publishable route(s)", r.Slug, r.PublishableRoutes))
		}
	}

	var active []importedRoute
	for _, route := range routes {
		if route.status == "active" {
			active = append(active, route)
		}
	}
	lines = append(lines, fmt.Sprintf("  routes          %d at this network, %d active and publishable", len(routes), len(active)))
	for i, route := range active {
		if i == importedRoutesShown {
			lines = append(lines, fmt.Sprintf("                  ... and %d more (docs/RUNBOOK.md has the query that lists them all)", len(active)-i))
			break
		}
		lines = append(lines, fmt.Sprintf("                  %-24s %s", route.externalID, route.slug))
	}
	if len(active) > 0 {
		lines = append(lines, fmt.Sprintf("  publish a rate  apivo %s -merchant <id above> -rate-bps <bps> -deeplink <template>", publishOfferName))
	}
	_, err := fmt.Fprintln(stdout, strings.Join(lines, "\n"))
	return err
}
