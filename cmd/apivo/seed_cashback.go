// The `seed cashback` command (T130): the local development catalogue,
// behind `make cashback-seed`.
//
// A file of its own for the reason connect_network.go is one - it opens the
// database and writes rows - and one more besides. Every other write path in
// this binary is a thing production does. This one is not, and the whole
// design below is about making that difference impossible to lose track of:
// it refuses any driver but the fixture, it writes catalogue rows through
// the real import rather than beside it, and the only rows it writes
// directly are the ones no other path in the repository can write at all.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/brand"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

const (
	// seedName is the subcommand as an operator types it, and cashbackTopic
	// the only thing it can seed. Two words rather than one because a seed
	// command that grows a second product should read as `seed news`, not
	// as a flag on a name that promised everything.
	seedName      = "seed"
	cashbackTopic = "cashback"

	// seedBackfill is how far back the seeded publisher account's history
	// starts. Far enough that the fixture's recorded transactions - dated
	// August 2026 - fall inside a window the sweeps will actually ask for,
	// and no further: a backfill start cannot be moved afterwards (0023).
	seedBackfill = 180 * 24 * time.Hour

	// seedDeeplinkHost is where a seeded band sends a member. A reserved
	// example domain (RFC 2606), so a developer who clicks through reaches
	// a page that exists and is obviously not a retailer, and no seeded
	// click can ever reach a real shop.
	seedDeeplinkHost = "https://example.com/shop/"
)

// seedCommand runs `seed cashback [ACCOUNT]`: the rows a developer needs
// before a click-out can be made at all, and none they could get any other
// way.
//
// ACCOUNT is optional and is a Supabase Auth user id, exactly as for news.
// Given one, the command opts that member into cashback, which is the last
// thing standing between a fresh database and a working money loop.
//
// Safe to run again. Every step below either finds what it needs or makes
// it, so a run interrupted halfway is repaired by running it once more
// rather than by dropping a schema.
func seedCommand(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s takes a topic: %s %s", seedName, seedName, cashbackTopic)
	}
	topic, rest := args[0], args[1:]
	if topic != cashbackTopic {
		return fmt.Errorf("%s cannot seed %s; it seeds %s", seedName, strconv.Quote(topic), strconv.Quote(cashbackTopic))
	}
	if len(rest) > 1 {
		return fmt.Errorf("%s %s takes at most one account id, got %q", seedName, cashbackTopic, rest)
	}

	// Ranged rather than indexed: rest holds nought or one account, and a
	// range says that without a second bounds check for a reader - or a
	// static analyser - to have to re-derive.
	var member uuid.UUID
	for _, id := range rest {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("%s is not an account id: it is the Supabase Auth user id, a UUID: %w",
				strconv.Quote(id), err)
		}
		member = parsed
	}

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	seeded, err := seedInputs(cfg)
	if err != nil {
		return err
	}

	pool, err := platformdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	return runCashbackSeed(ctx, pool, seeded, member, stdout)
}

// seedRequest is everything the seed needs that only configuration knows,
// resolved and checked before the database is opened - so a misconfigured
// run fails in a second and writes nothing.
type seedRequest struct {
	network        networks.Documented
	accountID      string
	brandID        string
	sourceLanguage string
	termsVersion   string
	currency       string
}

// seedInputs resolves and refuses. Each refusal below is a row this command
// would otherwise write wrongly, and none of them can be defaulted:
//
//   - a driver other than the fixture. This is the important one, and it is
//     not caution: a seeded route is indistinguishable from an imported one
//     once written, so on a deployment pointed at a real network the next
//     scheduled import would find these routes absent from the network's
//     answer and reconcile them to left_network - after members had already
//     clicked rates no network ever agreed to pay.
//   - no publisher account. It is what the network row's account hangs off,
//     and the sweeps key their jobs by.
//   - no brand. merchant_network.brand_id has no default, deliberately
//     (0011), and a brand literal in code is what the constitution's
//     rebrandability rule forbids - so the brand comes from BRAND_DIR or
//     this command does not run.
//   - no source language. It is the language the retailer's name is
//     recorded in, and merchant_copy's whole design is that a fallback is
//     labelled rather than guessed.
func seedInputs(cfg config.Config) (seedRequest, error) {
	if cfg.Cashback.Network.Driver != config.NetworkDriverFixture {
		return seedRequest{}, fmt.Errorf(
			"%s %s refuses NETWORK_DRIVER=%s: it writes retailers and rate bands nobody imported, and against a real network the next catalogue import would mark them left_network - after members had clicked a rate that network never agreed to pay. Set NETWORK_DRIVER=%s",
			seedName, cashbackTopic, strconv.Quote(cfg.Cashback.Network.Driver), strconv.Quote(config.NetworkDriverFixture))
	}
	if cfg.Cashback.Network.AccountID == "" {
		return seedRequest{}, errors.New("NETWORK_ACCOUNT_ID names no publisher account, and it is the account this would seed against")
	}
	if cfg.Cashback.Network.SourceLanguage == "" {
		return seedRequest{}, errors.New("NETWORK_SOURCE_LANGUAGE is unset, and it is the language the seeded retailers' names would be recorded in")
	}
	if cfg.BrandDir == "" {
		return seedRequest{}, errors.New("BRAND_DIR is unset, and merchant_network.brand_id has no default: which brand publishes a route is a decision, not a fallback (ADR-0004)")
	}
	defined, err := brand.LoadDir(cfg.BrandDir)
	if err != nil {
		return seedRequest{}, fmt.Errorf("BRAND_DIR=%s: %w", cfg.BrandDir, err)
	}
	// Validate() guarantees the terms document and the default currency,
	// so a brand that loaded cannot be missing either. The lookup is here
	// rather than at the opt-in below so that a run with no account id -
	// which never touches participation - still fails on a brand that
	// could not opt anybody in.
	terms, ok := defined.Document(brand.DocumentTerms)
	if !ok {
		return seedRequest{}, fmt.Errorf("BRAND_DIR=%s: the brand defines no %s document, and a participation records which version a member accepted",
			cfg.BrandDir, strconv.Quote(brand.DocumentTerms))
	}
	documented, err := documentedNetwork(cfg.Cashback.Network.Driver)
	if err != nil {
		return seedRequest{}, err
	}
	return seedRequest{
		network:        documented,
		accountID:      cfg.Cashback.Network.AccountID,
		brandID:        defined.ID,
		sourceLanguage: cfg.Cashback.Network.SourceLanguage,
		termsVersion:   terms.Version,
		currency:       defined.Defaults.Currency,
	}, nil
}

// runCashbackSeed does the three writes, in the order a deployment does
// them: connect the account, import the catalogue, then publish rates
// against what the import found.
//
// Three transactions rather than one, and deliberately. Each step is
// independently re-runnable and each is a different kind of fact - a
// connection, a catalogue, a price - so a failure in the third leaves the
// first two standing and the fix is to run the command again rather than to
// start from an empty schema.
func runCashbackSeed(ctx context.Context, pool *pgxpool.Pool, req seedRequest, member uuid.UUID, stdout io.Writer) error {
	connection, err := seedConnection(ctx, pool, req)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "network %s, publisher account %s (%s)\n",
		strconv.Quote(connection.Account.Network().String()),
		strconv.Quote(connection.Account.ExternalID()),
		madeOrFound(connection.AccountCreated, "connected", "already there")); err != nil {
		return err
	}

	routes, err := seedCatalogue(ctx, pool, req, connection.Account, stdout)
	if err != nil {
		return err
	}

	bands, err := seedRateBands(ctx, pool, routes)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "rate bands      %d written (%d route(s) already had one)\n",
		bands.written, bands.skipped); err != nil {
		return err
	}
	if err := reportLiveOffer(ctx, pool, connection.Account.Network().String(), stdout); err != nil {
		return err
	}

	if member != uuid.Nil {
		if err := seedParticipation(ctx, pool, req, member, stdout); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(stdout,
		"member          none opted in; pass an account id to opt one in: %s %s <supabase-auth-user-id>\n",
		seedName, cashbackTopic); err != nil {
		return err
	}
	return nil
}

// seedConnection writes the network and publisher-account rows, through the
// same domain call `connect-network` uses. Not a copy of it: the two rows
// are one fact and the rule that keeps them together belongs in one place.
func seedConnection(ctx context.Context, pool *pgxpool.Pool, req seedRequest) (networks.Connection, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return networks.Connection{}, fmt.Errorf("%s %s: %w", seedName, cashbackTopic, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	connection, err := networks.ConnectPublisherAccount(ctx, tx, networks.ConnectRequest{
		Network:             req.network,
		ExternalPublisherID: req.accountID,
		CredentialRef:       credentialRefKey,
		BackfillFrom:        time.Now().UTC().Add(-seedBackfill),
		Active:              true,
	})
	if err != nil {
		return networks.Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return networks.Connection{}, fmt.Errorf("%s %s: %w", seedName, cashbackTopic, err)
	}
	return connection, nil
}

// seedCatalogue runs one real catalogue import and returns the routes that
// came out of it.
//
// Through the import rather than beside it, which is the choice this whole
// command turns on. Rows written by hand would be rows no importer has ever
// produced - a different shape of raw_payload, a status nobody mapped, a
// retrieved_at that means nothing - and the next scheduled import would
// depart them, because MarkRoutesNotSeen finds exactly the routes the
// network did not return. Importing instead means a developer's seeded
// catalogue is the same catalogue the job maintains, and running the job
// afterwards re-affirms it rather than emptying it.
func seedCatalogue(ctx context.Context, pool *pgxpool.Pool, req seedRequest, account networks.PublisherAccount, stdout io.Writer) ([]seededRoute, error) {
	adapter, err := networkAdapter(config.NetworkDriverFixture, account)
	if err != nil {
		return nil, err
	}
	importer, err := catalogue.NewImporter(req.brandID, req.sourceLanguage)
	if err != nil {
		return nil, err
	}
	// Warnings and worse only: the import's own INFO lines describe a
	// scheduled job, and this is a person at a terminal who is about to be
	// told what happened in prose.
	quiet := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	imports, err := catalogue.NewImports(quiet, pool, adapter, importer)
	if err != nil {
		return nil, err
	}
	if err := imports.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("%s %s: importing the fixture catalogue: %w", seedName, cashbackTopic, err)
	}
	return seededRoutes(ctx, pool, account.Network().String(), stdout)
}

// seededRoute is one route the import left behind, reduced to what a rate
// band needs: which row to hang off, and what to call the shop it points at.
type seededRoute struct {
	id         pgtype.UUID
	externalID string
	status     string
}

// seededRoutes reads back what the import wrote, so the rates below are
// published against rows that exist rather than rows this command assumed.
func seededRoutes(ctx context.Context, pool *pgxpool.Pool, networkID string, stdout io.Writer) ([]seededRoute, error) {
	rows, err := pool.Query(ctx,
		`select id, external_merchant_id, status
		   from cashback.merchant_network
		  where network_id = $1
		  order by external_merchant_id`, networkID)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading back the imported routes: %w", seedName, cashbackTopic, err)
	}
	defer rows.Close()

	var routes []seededRoute
	var active int
	for rows.Next() {
		var route seededRoute
		if err := rows.Scan(&route.id, &route.externalID, &route.status); err != nil {
			return nil, fmt.Errorf("%s %s: reading back the imported routes: %w", seedName, cashbackTopic, err)
		}
		if route.status == "active" {
			active++
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s %s: reading back the imported routes: %w", seedName, cashbackTopic, err)
	}
	_, err = fmt.Fprintf(stdout, "catalogue       %d route(s) imported, %d active and publishable\n", len(routes), active)
	return routes, err
}

// seedCounts is what the rate-band step did, so a re-run can say "already
// there" rather than silently writing nothing.
type seedCounts struct{ written, skipped int }

// seedRateBands publishes rates against the active routes.
//
// The only rows this command writes directly, and the reason is worth
// stating rather than hiding: THERE IS NO OTHER WAY TO WRITE ONE. A rate
// band is inserted nowhere in this repository outside tests - the catalogue
// import does not carry rates (the port's ReportedMerchant has no rate
// field) and no operator endpoint sets one yet. That is a real gap in the
// product, and a local seed is not the place to close it; what this does is
// keep the gap visible by being the only caller, rather than papering over
// it with a query the domain would then appear to own.
//
// Three bands on EVERY active route, not one each, because a band is a
// thing with a lifetime: one that closed last month, one live now, and one
// that opens next month. A click-out reads the live one and snapshots it
// onto the click (FR-013), and having neighbours either side is what makes
// that snapshot mean something to look at.
//
// Every active route rather than the first, so that a recording which grows
// a second live retailer prices that one too. Each route gets its own three;
// a route is where a band lives, so "one live band" is a fact about a route
// and never about the network.
func seedRateBands(ctx context.Context, pool *pgxpool.Pool, routes []seededRoute) (seedCounts, error) {
	var counts seedCounts
	now := time.Now().UTC()

	for _, route := range routes {
		if route.status != "active" {
			continue
		}
		existing, err := routeHasBand(ctx, pool, route.id)
		if err != nil {
			return counts, err
		}
		if existing {
			counts.skipped++
			continue
		}
		template := seedDeeplinkHost + route.externalID
		bands := []struct {
			rateBPS   int32
			shareBPS  int32
			validFrom time.Time
			validTo   *time.Time
			condition string
		}{
			{300, 7000, now.Add(-60 * 24 * time.Hour), timePtr(now.Add(-30 * 24 * time.Hour)), "Seeded band, already closed: what a click before last month would have snapshotted."},
			{500, 7000, now.Add(-30 * 24 * time.Hour), nil, "Seeded band, live now. Minimum basket of nothing; this retailer does not exist."},
			{250, 7500, now.Add(30 * 24 * time.Hour), nil, "Seeded band, opens next month: not live, and a click today must not read it."},
		}
		for _, band := range bands {
			if _, err := pool.Exec(ctx,
				`insert into cashback.offer
				     (merchant_network_id, rate_kind, rate_bps, member_share_bps,
				      conditions, valid_from, valid_to, deeplink_template)
				 values ($1, 'percent', $2, $3, $4, $5, $6, $7)`,
				route.id, band.rateBPS, band.shareBPS, band.condition,
				band.validFrom, band.validTo, template); err != nil {
				return counts, fmt.Errorf("%s %s: publishing a rate band on route %s: %w",
					seedName, cashbackTopic, strconv.Quote(route.externalID), err)
			}
			counts.written++
		}
	}
	return counts, nil
}

// routeHasBand is the idempotency check, and it is per route rather than
// global: a second run after a new retailer appeared in the catalogue should
// price that retailer and leave the rest alone.
func routeHasBand(ctx context.Context, pool *pgxpool.Pool, route pgtype.UUID) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx,
		`select exists (select 1 from cashback.offer where merchant_network_id = $1)`,
		route).Scan(&exists); err != nil {
		return false, fmt.Errorf("%s %s: looking for existing rate bands: %w", seedName, cashbackTopic, err)
	}
	return exists, nil
}

// timePtr is the nullable valid_to, spelled once.
func timePtr(t time.Time) *time.Time { return &t }

// seedParticipation opts the member in, which is the one remaining thing
// between a seeded catalogue and a click-out that is allowed to happen.
//
// The same statement the endpoint uses: insert, or re-join if they had
// left. So a developer who left through the API and then re-seeds ends up
// in the state the API would have produced, and 0017's guard is what
// refuses any other move. Zero rows back is not a failure - it is the
// member already being opted in, which is exactly what a second run should
// find.
func seedParticipation(ctx context.Context, pool *pgxpool.Pool, req seedRequest, member uuid.UUID, stdout io.Writer) error {
	// Checked first, because the alternative is a foreign-key violation
	// naming a constraint. The account id IS the Supabase Auth user id
	// (the same rule news follows), so this command cannot invent one -
	// what it can do is say which of the two things went wrong.
	var known bool
	if err := pool.QueryRow(ctx,
		`select exists (select 1 from public.account where id = $1)`, member).Scan(&known); err != nil {
		return fmt.Errorf("%s %s: looking for account %s: %w", seedName, cashbackTopic, member, err)
	}
	if !known {
		return fmt.Errorf("no account %s exists, and the id must be the Supabase Auth user id rather than one this command invents: sign in once, then run %s %s with the id that appears in public.account",
			member, seedName, cashbackTopic)
	}

	tag, err := pool.Exec(ctx,
		`insert into cashback.participation
		     (account_id, brand_id, terms_version, default_currency, status)
		 values ($1, $2, $3, $4, 'active')
		 on conflict (account_id) do update
		    set opted_in_at = now(),
		        terms_version = excluded.terms_version,
		        default_currency = excluded.default_currency,
		        status = 'active',
		        left_at = null
		  where participation.status = 'left'`,
		member, req.brandID, req.termsVersion, req.currency)
	if err != nil {
		return fmt.Errorf("%s %s: opting account %s in: %w", seedName, cashbackTopic, member, err)
	}
	// Zero rows is the member already being active, and the terms and
	// currency above were NOT applied to them - 0017 freezes those on an
	// active row. Saying so is the difference between a second run that
	// reports what it found and one that claims what it did not do.
	if tag.RowsAffected() == 0 {
		_, err = fmt.Fprintf(stdout, "member          %s was already opted in; nothing changed\n", member)
		return err
	}
	_, err = fmt.Fprintf(stdout, "member          %s opted in, terms %s, %s\n",
		member, strconv.Quote(req.termsVersion), req.currency)
	return err
}

// reportLiveOffer names a band a click-out would actually be issued
// against, because "3 rate bands written" is not something a developer can
// do anything with and an offer id is. It re-uses the schema's own
// definition of live - the window open, and every leg of the chain active -
// rather than restating it, so a band this prints is a band the click-out
// endpoint will accept.
func reportLiveOffer(ctx context.Context, pool *pgxpool.Pool, networkID string, stdout io.Writer) error {
	var offer, slug string
	err := pool.QueryRow(ctx,
		`select o.id::text, m.slug
		   from cashback.offer o
		   join cashback.merchant_network mn on mn.id = o.merchant_network_id
		   join cashback.merchant m on m.id = mn.merchant_id
		   join cashback.network n on n.id = mn.network_id
		  where mn.network_id = $1
		    and o.valid_from <= now()
		    and coalesce(o.valid_to, 'infinity'::timestamptz) > now()
		    and mn.status = 'active' and m.status = 'active' and n.active
		  order by o.valid_from desc
		  limit 1`, networkID).Scan(&offer, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := fmt.Fprintln(stdout, "live offer      none: no route is publishable, so no click-out can be issued yet")
		return err
	}
	if err != nil {
		return fmt.Errorf("%s %s: looking for a live rate band: %w", seedName, cashbackTopic, err)
	}
	_, err = fmt.Fprintf(stdout, "live offer      %s (%s) - POST /api/v1/cashback/clickouts with this offer id\n", offer, slug)
	return err
}
