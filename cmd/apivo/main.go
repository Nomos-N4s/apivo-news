// Command apivo is the epiloYES backend: a single binary containing every
// module of the modular monolith. It is also the composition root - the one
// place where modules are wired together.
//
// With no arguments the binary serves HTTP. Two subcommands exist:
// "apivo healthcheck" probes the serving process once over HTTP and exits
// zero on success - it backs the container HEALTHCHECK, where the distroless
// image offers no shell or curl; "apivo version" prints the stamped release
// version and exits.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/account"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	cataloguestore "github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	clickoutstore "github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/manual"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/blnk"
	walletmemory "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	walletpostgres "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/postgres"
	walletstore "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/brand"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
	"github.com/Nomos-N4s/apivo-news/internal/translation"
	"github.com/Nomos-N4s/apivo-news/internal/translation/providers/openaicompat"
)

// version is the release version stamped into the binary at build time via
//
//	go build -ldflags "-X main.version=v0.1.0"
//
// (the release pipeline derives the value from the annotated tag, issue
// #119). "dev" is not a fallback pretending to be a release: it is the
// honest name for any binary that was not cut by the release pipeline.
var version = "dev"

// healthcheckTimeout bounds the whole healthcheck probe. It stays below the
// HEALTHCHECK --timeout in the Dockerfile so the probe fails with a precise
// error before Docker gives up on it.
const healthcheckTimeout = 3 * time.Second

// editorialPrefix is where the editorial module's route table is mounted.
// Every pattern the module registers lives under it, so mounting the prefix
// mounts the module whole - and unsetting JWKS_URL unmounts it whole.
const editorialPrefix = "/api/v1/editorial/"

// accountPrefix is where the account module's route table is mounted. It
// is gated on the same JWKS as editorial and mounted or unmounted with it:
// both need a verified bearer token, and a deployment that can verify one
// can verify the other.
const accountPrefix = "/api/v1/account/"

// opsPrefix is where the cashback operator module's route table is
// mounted. It is a subtree of the cashback base path, not the whole of it:
// the member endpoints under /api/v1/cashback/ are a different surface with
// a different gate, and the operator module must not claim their 404s.
//
// The module owns the string; naming it here would be a second copy, one
// deployment away from a subtree whose catch-all nothing reaches.
const opsPrefix = ops.Prefix

// clickoutPrefix is the click-out endpoint's path. The module owns the
// string; naming it here would be a second copy, one deployment away from a
// path whose catch-all nothing reaches.
const clickoutPrefix = clickout.Prefix

// merchantPrefix is the member-facing catalogue: one retailer's page and
// every rate published for them. A subtree of the cashback base path rather
// than the whole of it, for the reason opsPrefix is - a catch-all on the
// bare prefix would swallow every other module's 404s - and the module owns
// the string for the reason clickoutPrefix does.
const merchantPrefix = catalogue.MerchantPrefix

// walletPrefix is the member wallet surface, taken from the module that
// serves it for the reason clickoutPrefix is.
const walletPrefix = wallet.Prefix

// withdrawalPrefix is where a member asks to be paid. A sibling of the
// wallet rather than a path beneath it: reading a balance and asking for
// money to leave are different acts behind different rules, and the module
// owns the string for the reason clickoutPrefix does.
const withdrawalPrefix = payout.Prefix

// destinationsPrefix is where a member records and reads the places they may
// be paid. A sibling of the withdrawals tree rather than a path beneath it: a
// destination outlives the withdrawals that name it, and proving one is yours
// is a separate act from asking to be paid at it (FR-051).
const destinationsPrefix = payout.DestinationsPrefix

// exportPrefix is the member's own history as a document. A sibling of the
// wallet for the reason participationPrefix is: FR-003 pairs exporting with
// leaving, and a member who has left still has a record to take.
const exportPrefix = wallet.ExportPrefix

// participationPrefix is the member's opt-in. A sibling of the wallet
// rather than a path beneath it (contracts/http-api.md), and served by the
// same module handler - which is why it is mounted below beside the wallet
// rather than assembled from a second one.
const participationPrefix = wallet.ParticipationPrefix

// readerPrefix is where the content module's route table is mounted. It
// covers the whole API namespace because the module also answers what nobody
// routed under it, which is how every error below /api/v1 stays problem+json.
// More specific patterns registered elsewhere - the OpenAPI document, the
// editorial prefix - still win, as ServeMux precedence requires.
const readerPrefix = "/api/v1/"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "apivo:", err)
		os.Exit(1)
	}
}

// run parses the arguments and dispatches: no arguments serves until ctx is
// cancelled or a termination signal arrives; "healthcheck" probes the serving
// process once and reports the verdict as its error; "version" prints the
// stamped release version. It is separated from main so the wiring is
// testable; main only handles the exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case len(args) == 0:
		return serve(ctx, getenv, stdout)
	case args[0] == connectNetworkName:
		// The one subcommand that takes flags, and the one that writes to
		// the database. Its own arguments are its own to check.
		return connectNetworkCommand(ctx, args[1:], getenv, stdout)
	case args[0] != "healthcheck" && args[0] != "version":
		return fmt.Errorf("unknown command %q", args[0])
	case len(args) > 1:
		return fmt.Errorf("%s takes no arguments, got %q", args[0], args[1:])
	case args[0] == "version":
		_, err := fmt.Fprintf(stdout, "apivo version %s\n", version)
		return err
	default:
		return healthcheck(ctx, getenv)
	}
}

// serve wires the platform together and serves HTTP until ctx is cancelled.
func serve(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
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

	// The one network this deployment integrates, resolved once. Two things
	// need it and neither may look it up separately: the poller acts for the
	// account, and the click-out endpoint builds its redirects with the same
	// adapter (FR-021). A deployment where those disagreed about which
	// network it is connected to would issue clicks one code path could not
	// reconcile. It is only asked for when cashback is on, because with
	// cashback off neither consumer exists.
	var (
		adapter    networks.Network
		connected  networks.ConnectedAccount
		networkOff *ingestionOff
	)
	if cfg.Cashback.Enabled {
		var err error
		adapter, connected, err = connectNetwork(ctx, cfg.Cashback.Network, pool)
		switch {
		case errors.As(err, &networkOff):
			log.ErrorContext(ctx, "NO AFFILIATE NETWORK IS CONNECTED: "+networkOff.reason+
				". Cashback is enabled, so nothing will ingest what a network reports and every click-out will be refused")
		case err != nil:
			return err
		}
	}

	var routes []platformhttp.Route
	// The settlement sweep is built with the cashback surface, because it
	// needs the one ledger that surface shares, and registered with the
	// scheduler further down. Nil until then, and nil for good in a
	// deployment with no authenticated routes or no cashback.
	var settlements *payout.Settlements
	if cfg.JWKSURL == "" {
		// Without a verification endpoint no bearer token can be checked, so
		// the authenticated routes are not mounted at all: a misconfigured
		// deployment exposes nothing, rather than something unguarded.
		//
		// This is loud but not fatal, deliberately. The reader-facing path
		// needs no bearer token, so refusing to start would take the public
		// site down over an editorial-only misconfiguration - a far worse
		// failure than editorial endpoints answering 404. ERROR level and a
		// named consequence make the cause obvious in the first log line
		// rather than something to deduce from a 404 later.
		log.ErrorContext(ctx, "JWKS_URL is not set: every /api/v1/editorial/ and /api/v1/account/ route is UNMOUNTED and will answer 404; reader endpoints are unaffected. Set JWKS_URL to the auth provider JWKS endpoint to enable them")
	} else {
		authenticated, sweep, closeVerifier, err := newAuthenticatedRoutes(ctx, cfg, log, pool, adapter)
		if err != nil {
			return err
		}
		defer closeVerifier()
		routes = append(routes, authenticated...)
		// Held for the scheduler below, which is built after the routes.
		// Nil when cashback is off, and the registration there skips it.
		settlements = sweep
	}

	// The feed poll loop runs beside the HTTP server, on the same pool and
	// under the same lifetime: it stops with ctx like srv.Run does, and the
	// deferred wait below holds serve open until the loop has actually
	// returned - so the pool it polls with is never closed under it (the
	// deferred pool.Close above runs after this defer, LIFO). Interval zero
	// (POLL_INTERVAL=0, the one disable switch) means the loop is never
	// started; unset means the documented 15m default.
	if cfg.PollInterval > 0 {
		poller := ingestion.NewPoller(log, pool, ingestion.PollConfig{Interval: cfg.PollInterval})
		pollCtx, stopPoller := context.WithCancel(ctx)
		pollDone := make(chan struct{})
		go func() {
			defer close(pollDone)
			poller.Run(pollCtx)
		}()
		defer func() {
			stopPoller()
			<-pollDone
		}()
		log.InfoContext(ctx, "feed poll loop started", "interval", cfg.PollInterval)
	} else {
		log.InfoContext(ctx, "POLL_INTERVAL is 0: the feed poll loop is disabled and no source will be polled")
	}

	// The translation pipeline runs beside the poll loop, in the same
	// style and under the same lifetime: the deferred wait holds serve
	// open until the loop has returned, so the pool is never closed under
	// it. It starts ONLY on a complete TRANSLATION_* configuration -
	// provider, model, prices (or an explicit free-of-charge), and both
	// budget caps have no defaults, because the production provider and
	// budget are a founder decision still pending and nothing here may
	// make it by accident. Anything less stays off, with one line naming
	// the state; a complete-but-wrong value (a bad URL, an inconsistent
	// budget) fails startup instead, because an operator who set nine
	// keys believes translation is on.
	switch tc := cfg.Translation; {
	case !tc.Configured() && !tc.AnySet():
		log.InfoContext(ctx, "no translation provider is configured: the translation pipeline is OFF and no item will be translated (provider and budget are a pending founder decision). To enable it set: "+strings.Join(tc.Missing(), ", "))
	case !tc.Configured():
		log.ErrorContext(ctx, "the translation provider configuration is INCOMPLETE: the translation pipeline is OFF and no item will be translated. Missing: "+strings.Join(tc.Missing(), ", "))
	case tc.Interval == 0:
		log.InfoContext(ctx, "TRANSLATION_INTERVAL is 0: the translation pipeline is disabled and no item will be translated")
	default:
		translator, err := openaicompat.New(openaicompat.Config{
			BaseURL:                  tc.BaseURL,
			Model:                    tc.Model,
			APIKey:                   tc.APIKey,
			InputPricePerMillionUSD:  tc.InputUSDPerMTok,
			OutputPricePerMillionUSD: tc.OutputUSDPerMTok,
			FreeOfCharge:             tc.FreeOfCharge,
		})
		if err != nil {
			return err
		}
		pipeline, err := translation.NewPipeline(log, pool, translator, translation.PipelineConfig{
			Interval:      tc.Interval,
			ReaderLocales: translation.AlphaReaderLocales,
			PromptVersion: translation.CurrentPromptVersion,
			Caps: translation.Caps{
				PerArticleMicroUSD: tc.ArticleCeilingMicroUSD,
				MonthlyMicroUSD:    tc.MonthlyCapMicroUSD,
			},
		})
		if err != nil {
			return err
		}
		translateCtx, stopTranslation := context.WithCancel(ctx)
		translateDone := make(chan struct{})
		go func() {
			defer close(translateDone)
			pipeline.Run(translateCtx)
		}()
		defer func() {
			stopTranslation()
			<-translateDone
		}()
		log.InfoContext(ctx, "translation pipeline started",
			"interval", tc.Interval, "model", tc.Model,
			"reader_locales", translation.AlphaReaderLocales,
			"prompt_version", translation.CurrentPromptVersion,
			"article_ceiling_microusd", tc.ArticleCeilingMicroUSD,
			"monthly_cap_microusd", tc.MonthlyCapMicroUSD)
	}

	// The continuous C-1 zero-sum check runs beside the poll and translation
	// loops, in the same style and under the same lifetime: the deferred
	// wait holds serve open until the scheduler has returned, so the pool
	// its job reads with is never closed under it (the deferred pool.Close
	// above runs after this defer, LIFO). It is gated exactly as the
	// product is - CASHBACK_ENABLED off means cashback does not exist in
	// this process, and neither does its watchdog - and on nothing else:
	// the check's cadence is the wallet package's own constant, because
	// the one continuous verification of C-1 (ADR-0002, SC-003) is not a
	// tuning knob and gets no off switch. The check itself is registered
	// in the wallet package (T046): this block gives it a scheduler, a
	// lifetime, and the one fact only the composition root holds - which
	// schema its ledger driver implies.
	if cfg.Cashback.Enabled {
		// LEDGER_DRIVER=postgres keeps the exit-route ledger in this
		// database's own `ledger` schema (0022), which 0020's `blnk`
		// default never resolves: a check left on the default would sum
		// nothing over the one ledger it is guaranteed to share a database
		// with, and pass vacuously while that ledger drifts. The other
		// drivers stay on the default, which is right both when Blnk
		// shares the database and when no ledger is co-located at all.
		var ledgerSchema string
		if cfg.Cashback.LedgerDriver == config.LedgerDriverPostgres {
			ledgerSchema = "ledger"
		}
		locker := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{})
		jobs := scheduler.New(log, locker, scheduler.Config{})
		if err := wallet.NewZeroSumCheck(log, pool, ledgerSchema).Register(jobs); err != nil {
			return err
		}
		registered := 1

		settling, err := registerSettlement(ctx, log, jobs, settlements)
		if err != nil {
			return err
		}
		registered += settling

		// The affiliate-network sweeps, which are opt-in in a way the
		// zero-sum check is not: polling needs a publisher account row that
		// only an operator can create, so a deployment configured ahead of
		// that runs with ingestion off rather than not at all. It says so
		// at ERROR, because "cashback is mounted and nothing is ingesting
		// what the networks report" is a state somebody has to fix.
		switch sweeps, err := newNetworkSweeps(ctx, log, adapter, connected, pool); {
		// Already reported above, where the network was resolved: saying it
		// twice would read as two different problems.
		case networkOff != nil:
		case err != nil:
			return err
		default:
			if err := sweeps.Register(jobs); err != nil {
				return err
			}
			registered += 2
			log.InfoContext(ctx, "affiliate network sweeps registered",
				"account", sweeps.Account().String(),
				"forward_job", networks.ForwardJobName(sweeps.Account()),
				"forward_interval", networks.ForwardInterval,
				"trailing_job", networks.TrailingJobName(sweeps.Account()),
				"trailing_interval", networks.TrailingInterval,
				"trailing_lag", networks.DefaultTrailingLag)
		}

		// The catalogue import, opt-in in the same way and for the same
		// reason: it needs a brand and a stated source language, and a
		// deployment configured ahead of either should serve its existing
		// catalogue rather than refuse to start. It says so at ERROR,
		// because a catalogue nothing refreshes looks exactly like a
		// catalogue right up to the day a member clicks a retailer who left.
		switch imports, missing, err := newCatalogueImport(log, cfg, adapter, pool); {
		// Already reported where the network was resolved.
		case networkOff != nil:
		case err != nil:
			return err
		case len(missing) > 0:
			log.ErrorContext(ctx, "NO CATALOGUE IMPORT IS SCHEDULED, so the retailers members can click will never be refreshed",
				"missing", strings.Join(missing, ", "))
		default:
			if err := imports.Register(jobs); err != nil {
				return err
			}
			registered++
			log.InfoContext(ctx, "catalogue import registered",
				"job", catalogue.ImportJobName,
				"interval", catalogue.ImportInterval,
				"network", adapter.ID().String())
		}

		// The same refusal Run makes first, made here where it can still
		// fail the deployment: from inside the goroutine below, a pool too
		// small for its jobs would be one ERROR line under an already
		// printed "started", and the process would then serve its whole
		// life holding money with C-1 unwatched. Run re-checks with the
		// true count either way, but only this synchronous copy can refuse
		// to start - and the count is computed rather than written down, so
		// a job registered above cannot be forgotten here.
		//
		// Two connections per job plus two reserved is the locker's
		// arithmetic. The zero-sum check alone needs 4; the settlement
		// sweep takes that to 6, the two network sweeps to 10, and the
		// catalogue import to 12. pgx defaults MaxConns to max(4, NumCPU),
		// which is why a deployment with cashback on may have to raise
		// pool_max_conns in DATABASE_URL - the error below says so with the
		// numbers in it.
		if err := locker.CheckCapacity(registered); err != nil {
			return err
		}
		jobsCtx, stopJobs := context.WithCancel(ctx)
		jobsDone := make(chan struct{})
		go func() {
			defer close(jobsDone)
			// With capacity proven above, Run has no way back before
			// jobsCtx ends: nil is a clean stop, and anything else - a
			// shutdown grace that expired with a run still in flight - is
			// a real event on the way down, worth its own line.
			if err := jobs.Run(jobsCtx); err != nil {
				log.ErrorContext(ctx, "the scheduled-job runner stopped", "error", err)
			}
		}()
		defer func() {
			stopJobs()
			<-jobsDone
		}()
		log.InfoContext(ctx, "continuous ledger zero-sum check started",
			"job", wallet.ZeroSumJobName, "interval", wallet.ZeroSumInterval, "ledger_schema", ledgerSchema)
	}

	srv := platformhttp.New(log, cfg.HTTPAddr, version, readiness(pool), routes...)
	// The reader endpoints need no bearer token, so they mount
	// unconditionally - a missing JWKS_URL costs the editorial routes, never
	// the public site.
	srv.Mount(readerPrefix, content.NewHandler(log, pool))
	log.InfoContext(ctx, "starting", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", version)
	return srv.Run(ctx)
}

// newAuthenticatedRoutes wires every module that needs a verified bearer
// token: editorial, account, and - when cashback is on - the operator
// surface and the member click-out.
//
// ONE verifier, shared. Each verifier keeps a background loop refreshing
// the JWKS, so building one per module would mean two loops fetching the
// same document from the same provider on the same schedule, and two
// caches that could disagree about which keys are current. The returned
// func stops the one loop there is.
//
// The first two are mounted together or not at all. They differ in what
// they then require - editorial demands the editor role, account only that
// somebody is signed in - but neither can answer anything without a token
// that verifies, so the condition for mounting is the same condition.
//
// The two cashback surfaces need that token AND the feature flag: with
// CASHBACK_ENABLED off, cashback does not exist in this process, and a queue
// of work nothing is filling is not a surface to serve.
//
// The adapter is whatever [connectNetwork] resolved, and may be nil: a
// deployment with cashback on and no network connected still SERVES the
// click-out endpoint, and refuses each request with the deterministic
// refusal that names the network nothing can build a redirect for. Not
// mounting it instead would answer 404, which tells a frontend the API does
// not exist here when it does and is merely unable to send anybody anywhere.
// registerSettlement puts the payout settlement sweep on the scheduler and
// answers how many jobs that added, for the capacity check.
//
// Extracted so the registration itself is testable. It is the only thing in
// this tree that observes a payment ARRIVING (T146), and a sweep that were
// built and never registered would leave every payout stuck at submitted and
// paid_out reporting zero - with nothing failing, because every service it
// calls still works. The count is returned rather than assumed for the same
// reason the caller computes the rest: a job registered and not counted is a
// pool sized for fewer jobs than are running.
//
// A nil sweep is not an error. It means the authenticated surface was not
// built, or cashback is off, and then there are no payouts to settle.
func registerSettlement(ctx context.Context, log *slog.Logger, jobs *scheduler.Scheduler, sweep *payout.Settlements) (int, error) {
	if sweep == nil {
		return 0, nil
	}
	if err := sweep.Register(jobs); err != nil {
		return 0, err
	}
	log.InfoContext(ctx, "payout settlement sweep registered",
		"job", payout.SettlementJobName, "interval", payout.SettlementInterval)
	return 1, nil
}

func newAuthenticatedRoutes(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, adapter networks.Network) ([]platformhttp.Route, *payout.Settlements, func(), error) {
	verifier, err := identity.NewVerifier(ctx, identity.VerifierConfig{
		JWKSURL:  cfg.JWKSURL,
		Audience: cfg.JWTAudience,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	ids := identity.New(verifier, pool)
	roles := identity.NewAccountRoles(pool)
	stop := func() { _ = verifier.Close(context.Background()) }
	routes := []platformhttp.Route{
		{
			Pattern: editorialPrefix,
			Handler: editorial.NewHandler(log, editorial.NewPGStore(pool), newEditorAuth(ids, roles)),
		},
		{
			Pattern: accountPrefix,
			Handler: account.NewHandler(log, account.NewPGStore(pool), accountAuth{ids: ids}),
		},
	}

	if !cfg.Cashback.Enabled {
		// Named, not silent. An operator who cannot find the queue should
		// read why in the first log line rather than deduce it from a 404.
		log.InfoContext(ctx, "CASHBACK_ENABLED is off: every "+opsPrefix+" route is UNMOUNTED and will answer 404")
		// No sweep either: with no payouts there is nothing in flight to
		// ask a rail about, and a job that woke every five minutes to read
		// an empty table would be one more thing to explain.
		return routes, nil, stop, nil
	}
	opsStore, err := ops.NewPGStore(pool)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	clickouts, err := newClickOuts(pool, adapter, cfg.Cashback.ClickContextHeader)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	clickOutOptions := []clickout.HandlerOption{}
	if cfg.Cashback.ClickContextHeader != "" {
		clickOutOptions = append(clickOutOptions, clickout.WithContextHeader(cfg.Cashback.ClickContextHeader))
	}
	// One ledger for the whole cashback surface. Two would be two ledgers
	// under the memory driver - a wallet showing balances the withdrawal
	// path cannot see - and two clients of the same substrate under the
	// others, which is waste rather than a bug. Built here so both callers
	// take the same one.
	ledger, err := newLedger(cfg, pool)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	wallets, err := newWallets(ledger, pool, cfg.Cashback.PayoutThreshold)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	withdrawals, err := payout.NewWithdrawals(pool, ledger,
		cfg.Cashback.HouseAccounts.NetworkReceivable, cfg.Cashback.PayoutThreshold)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	history, err := wallet.NewHistory(walletstore.New(pool))
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	terms, err := brandTerms(log, cfg.BrandDir)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	approvals, err := newApprovals(pool, terms)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	refusals, err := payout.NewRejections(pool, ledger, cfg.Cashback.HouseAccounts.NetworkReceivable)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	participations, err := wallet.NewParticipations(pool, walletstore.New(pool), terms)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	exports, err := wallet.NewExports(history)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	memberSurface := wallet.NewHandler(log, wallets, history, participations, exports, walletAuth{ids: ids})
	merchants, err := catalogue.NewMerchantReader(cataloguestore.New(pool))
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	catalogueSurface := catalogue.NewHandler(log, merchants, catalogueAuth{ids: ids})
	destinations, err := payout.NewDestinations(pool)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	// No details vault is wired, and that is the honest state of the alpha:
	// a vault is somebody's KMS or a processor's tokenisation endpoint, and
	// picking one here would pick it for every deployment. Passing nil
	// means POST /payout-destinations answers 503 naming nothing else -
	// every other route on the surface, including withdrawing to a
	// destination that already exists, works (payout.ErrNoVault).
	withdrawalSurface, err := payout.NewHandler(log, withdrawals, destinations, nil, payoutAuth{ids: ids})
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	// The settlement sweep, over the SAME rail the approver submits
	// through: asking a different one about a payment is asking about a
	// payment it never made. It reaches Retries only for Abandon - a
	// payment the rail took and could not complete puts the money back the
	// way a refused submission does - and the Abandoner port is what stops
	// it re-sending anything.
	retries, err := payout.NewRetries(pool, newPayoutRail(), ledger,
		cfg.Cashback.HouseAccounts.NetworkReceivable, payoutDescriptor(terms))
	if err != nil {
		stop()
		return nil, nil, nil, err
	}
	settlements, err := payout.NewSettlements(log, pool, newPayoutRail(), retries)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}

	return append(routes,
		platformhttp.Route{
			Pattern: opsPrefix,
			Handler: ops.NewHandler(log, opsStore, approvals, refusals, settlements, newOperatorAuth(ids, roles)),
		},
		// Mounted at the path AND at its subtree, so a stray sub-path is
		// answered in problem+json by the module rather than redirected by
		// ServeMux or handed to whatever else claims the namespace.
		platformhttp.Route{
			Pattern: clickoutPrefix,
			Handler: clickout.NewHandler(log, clickouts, memberAuth{ids: ids}, clickOutOptions...),
		},
		platformhttp.Route{
			Pattern: clickoutPrefix + "/",
			Handler: clickout.NewHandler(log, clickouts, memberAuth{ids: ids}, clickOutOptions...),
		},
		// The member-facing catalogue, at the path AND its subtree for the
		// reason the click-out is.
		platformhttp.Route{Pattern: merchantPrefix, Handler: catalogueSurface},
		platformhttp.Route{Pattern: merchantPrefix + "/", Handler: catalogueSurface},
		// One handler at six patterns: the wallet, the participation and
		// the export trees, each at the path AND its subtree. The module's
		// own mux matches on the full path, so all three route correctly
		// out of one route table, one auth gate and one error convention.
		platformhttp.Route{Pattern: walletPrefix, Handler: memberSurface},
		platformhttp.Route{Pattern: walletPrefix + "/", Handler: memberSurface},
		platformhttp.Route{Pattern: participationPrefix, Handler: memberSurface},
		platformhttp.Route{Pattern: participationPrefix + "/", Handler: memberSurface},
		platformhttp.Route{Pattern: exportPrefix, Handler: memberSurface},
		platformhttp.Route{Pattern: exportPrefix + "/", Handler: memberSurface},
		// Withdrawals are a sibling of the wallet rather than a path beneath
		// it, and a module of their own: reading a balance and asking for
		// money to leave are different acts, and the second one is the one
		// C-4 and C-5 exist for.
		platformhttp.Route{Pattern: withdrawalPrefix, Handler: withdrawalSurface},
		platformhttp.Route{Pattern: withdrawalPrefix + "/", Handler: withdrawalSurface},
		platformhttp.Route{Pattern: destinationsPrefix, Handler: withdrawalSurface},
		platformhttp.Route{Pattern: destinationsPrefix + "/", Handler: withdrawalSurface},
	), settlements, stop, nil
}

// brandTerms reads the brand definition BRAND_DIR names and reduces it to
// the three fields the participation record needs (ADR-0004).
//
// An unset BRAND_DIR is not an error. There is no brand this repository
// could ship that would not be a lie about a real company, so a deployment
// without one starts and the opt-in answers 503 naming the key - the same
// stance the wallet takes on a missing payout threshold. One ERROR line
// says so at start-up, because the consequence is a member-facing endpoint
// that cannot work.
//
// A BRAND_DIR that is set and unreadable, or holds a brand file that does
// not validate, IS a startup failure: a deployment that named a brand meant
// it, and starting anyway would serve the 503 while an operator believed
// the brand was configured.
func brandTerms(log *slog.Logger, dir string) (wallet.Terms, error) {
	if dir == "" {
		log.Error("BRAND_DIR is unset, so this deployment has no brand definition: members cannot opt into cashback and POST /api/v1/cashback/participation will answer 503",
			"key", "BRAND_DIR")
		return wallet.Terms{}, nil
	}
	defined, err := brand.LoadDir(dir)
	if err != nil {
		return wallet.Terms{}, fmt.Errorf("BRAND_DIR=%s: %w", dir, err)
	}
	// Validate() guarantees both documents are present, so this lookup
	// cannot miss on a brand that loaded.
	terms, ok := defined.Document(brand.DocumentTerms)
	if !ok {
		return wallet.Terms{}, fmt.Errorf("BRAND_DIR=%s: the brand defines no %q document", dir, brand.DocumentTerms)
	}
	currency, err := money.ParseCurrency(defined.Defaults.Currency)
	if err != nil {
		return wallet.Terms{}, fmt.Errorf("BRAND_DIR=%s: defaults.currency: %w", dir, err)
	}
	log.Info("brand definition loaded", "brand", defined.ID, "terms_version", terms.Version, "currency", currency)
	return wallet.Terms{Brand: defined.ID, Version: terms.Version, Currency: currency}, nil
}

// newWallets assembles the member wallet: the ledger the balances are
// projected from, the settled payouts that say what has been paid, and the
// threshold a withdrawal is checked against.
//
// The ledger is passed in rather than built here, because the withdrawal
// path needs THE SAME ONE - see the call site.
//
// The threshold may be the zero Amount here, and is passed through as one.
// Production cannot start without it; the environments that can are the
// no-Docker loop and CI, and there the endpoint answers 503 naming the two
// keys rather than being unmounted - a 404 would say the API is not here.
func newWallets(ledger wallet.Ledger, pool *pgxpool.Pool, threshold money.Amount) (*wallet.Wallets, error) {
	projector, err := wallet.NewProjector(ledger)
	if err != nil {
		return nil, err
	}
	return wallet.NewWallets(projector, walletstore.New(pool), threshold)
}

// newApprovals assembles the withdrawal approver: the rail this deployment
// pays through, and the descriptor a member reads on their statement.
//
// The MANUAL rail is the alpha's default and its only unconditional one
// (FR-052). It is not a placeholder: an operator makes the transfer and
// records the reference, and C-4 and C-5 hold identically either way -
// the payout row still names the approver and still carries the generated
// key. A deployment with a real rail selects it here; until one exists,
// "manual" is the honest answer rather than a stub pretending to move money.
//
// The descriptor comes from the brand (FR-070, FR-073), because no rail may
// contain a product name. A deployment with no BRAND_DIR has no descriptor,
// and the approver is built without one - which makes every approval answer
// rather than making the operator queue disappear, the same stance the
// wallet takes on a missing threshold.
func newApprovals(pool *pgxpool.Pool, terms wallet.Terms) (*payout.Approvals, error) {
	return payout.NewApprovals(pool, newPayoutRail(), payoutDescriptor(terms))
}

// newPayoutRail is the one rail this deployment pays through.
//
// A function rather than a value because every caller wants its own - the
// approver, the retrier and the settlement sweep - and because it is the
// single place a deployment with a real rail changes. What must NOT vary is
// which rail: asking one rail about a payment another made is asking about a
// payment that does not exist.
func newPayoutRail() payout.Rail { return manual.New() }

// payoutDescriptor is what a member reads on their statement (FR-070,
// FR-073). It comes from the brand, because no rail may contain a product
// name.
func payoutDescriptor(terms wallet.Terms) string {
	if terms.Brand == "" {
		// payout.NewApprovals refuses a blank one, and the alternative to a
		// placeholder here is an unmounted operator queue. The name is not
		// a product name: it is the absence of one, and it reaches a
		// member's statement only in a deployment that has already been
		// told at start-up that it has no brand.
		return "CASHBACK"
	}
	return terms.Brand
}

// newLedger builds the Ledger port's implementation this deployment selected
// (ADR-0002).
//
// There is no default and there must not be one: LEDGER_DRIVER is required
// whenever cashback is on, and a deployment that forgot it would otherwise
// get the in-process ledger - one that persists nothing, and looks healthy
// while doing it. config.parseCashback refuses an unknown name, so the last
// arm here is unreachable and says so rather than choosing.
func newLedger(cfg config.Config, pool *pgxpool.Pool) (wallet.Ledger, error) {
	switch cfg.Cashback.LedgerDriver {
	case config.LedgerDriverBlnk:
		opts := []blnk.Option{}
		if key := cfg.Cashback.BlnkSecretKey.Reveal(); key != "" {
			opts = append(opts, blnk.WithSecretKey(key))
		}
		return blnk.New(cfg.Cashback.BlnkURL, opts...)
	case config.LedgerDriverPostgres:
		return walletpostgres.New(pool), nil
	case config.LedgerDriverMemory:
		// Persists nothing and is scoped to this process. Configuration
		// refuses it in production; here it is the no-Docker loop's ledger
		// and nothing more.
		return walletmemory.New(), nil
	}
	return nil, fmt.Errorf("main: LEDGER_DRIVER %q selects no ledger", cfg.Cashback.LedgerDriver)
}

// newClickOuts assembles the click-out service: the catalogue read that says
// what a band is, the recorder that writes the click, and the adapters that
// can build a redirect.
//
// A nil adapter is passed through as an EMPTY registry rather than refused.
// The endpoint is then served and every request answers 502 naming the
// network nothing can build a redirect for - which is the truth, and is
// findable, where a 404 would say the API is not here at all.
func newClickOuts(pool *pgxpool.Pool, adapter networks.Network, contextHeader string) (*clickout.ClickOuts, error) {
	// The recorder that opens its own transaction, because the click and the
	// event announcing it commit together or neither does (T076).
	clicks, err := clickout.NewAnnouncedClicks(pool)
	if err != nil {
		return nil, err
	}
	// Both halves of the click rule only where the deployment named a header
	// it trusts to carry the real client address. Without one the digest is
	// the proxy, and a per-device limit would bracket every member behind
	// it - so that half stays off and the per-member half, which is keyed on
	// the caller's own account, carries the rule alone.
	rule := clickout.DefaultClickRule()
	if contextHeader != "" {
		rule = clickout.DefaultClickRuleWithContext()
	}
	limiter, err := clickout.NewLimiter(clickoutstore.New(pool), rule)
	if err != nil {
		return nil, err
	}
	var adapters []clickout.Redirector
	if adapter != nil {
		adapters = append(adapters, adapter)
	}
	deeplinks, err := clickout.NewDeeplinks(adapters...)
	if err != nil {
		return nil, err
	}
	return clickout.NewClickOuts(catalogue.NewOfferReader(cataloguestore.New(pool)), clicks, deeplinks,
		clickout.WithLimiter(limiter))
}

// memberAuth adapts the identity module to the click-out module's
// consumer-defined MemberAuthenticator. Deliberately the thinnest of the
// three adapters here: there is no role to require, because every route in
// that module acts on the caller's own click and a reader owns theirs
// exactly as an editor owns theirs. What it must NOT do is let a request
// through without an account - that is FR-023.
type memberAuth struct {
	ids *identity.Service
}

func (a memberAuth) AuthenticateMember(ctx context.Context, token string) (clickout.Member, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return clickout.Member{}, fmt.Errorf("%w: %w", clickout.ErrUnauthenticated, err)
	case err != nil:
		return clickout.Member{}, err
	}
	return clickout.Member{ID: id.Subject}, nil
}

// catalogueAuth adapts the identity module to the catalogue module's own
// consumer-defined MemberAuthenticator.
//
// It resolves a token and returns no account, which is the whole of that
// interface: browsing is gated because there is no anonymous cashback
// surface (FR-023), not because a catalogue answer depends on who is asking.
// An adapter that handed the account over would be the first step towards a
// read that quietly scoped by it.
type catalogueAuth struct {
	ids *identity.Service
}

func (a catalogueAuth) AuthenticateReader(ctx context.Context, token string) error {
	_, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return fmt.Errorf("%w: %w", catalogue.ErrUnauthenticated, err)
	case err != nil:
		return err
	}
	return nil
}

// walletAuth adapts the identity module to the wallet module's own
// consumer-defined MemberAuthenticator.
//
// A second adapter rather than memberAuth reused, because the two modules
// declare their own interface and their own Member type - which is the
// boundary rule working: neither module can be given the other's notion of
// who is asking, and a change made for the click-out surface cannot reach
// the wallet by sharing an adapter. The bodies are the same today and that
// is not a reason to merge them; every route on both surfaces acts on the
// caller's own money, so the same absence of a role check is the correct
// answer arrived at twice rather than one answer applied twice.
type walletAuth struct {
	ids *identity.Service
}

func (a walletAuth) AuthenticateMember(ctx context.Context, token string) (wallet.Member, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return wallet.Member{}, fmt.Errorf("%w: %w", wallet.ErrUnauthenticated, err)
	case err != nil:
		return wallet.Member{}, err
	}
	return wallet.Member{ID: id.Subject}, nil
}

// payoutAuth adapts the identity module to the payout module's own
// consumer-defined MemberAuthenticator, for the reason walletAuth is a
// second adapter rather than memberAuth reused: each module declares its own
// interface and its own Member type, so a change made for one surface cannot
// reach another by sharing an adapter. This surface is where money LEAVES,
// which is the surface it would matter most on.
type payoutAuth struct {
	ids *identity.Service
}

func (a payoutAuth) AuthenticateMember(ctx context.Context, token string) (payout.Member, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return payout.Member{}, fmt.Errorf("%w: %w", payout.ErrUnauthenticated, err)
	case err != nil:
		return payout.Member{}, err
	}
	return payout.Member{ID: id.Subject}, nil
}

// newOperatorAuth builds the identity-to-ops adapter. It is the single
// construction point for operatorAuth, so callers - production wiring and
// tests alike - go through the same path and a new dependency cannot be
// wired one way here and another way there.
func newOperatorAuth(ids *identity.Service, roles identity.RoleLookup) operatorAuth {
	return operatorAuth{ids: ids, roles: roles}
}

// operatorAuth adapts the identity module to the operator surface's
// consumer-defined OperatorAuthenticator, exactly as editorAuth does for
// editorial. Two adapters rather than one parameterised by a role, for the
// reason identity keeps two gates: approving an article and releasing money
// are separate authorities, and a shared adapter is where that separation
// would quietly become a parameter somebody widened.
type operatorAuth struct {
	ids   *identity.Service
	roles identity.RoleLookup
}

func (a operatorAuth) AuthenticateOperator(ctx context.Context, token string) (ops.Operator, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return ops.Operator{}, fmt.Errorf("%w: %w", ops.ErrUnauthenticated, err)
	case err != nil:
		return ops.Operator{}, err
	}
	switch err := identity.RequireOperator(ctx, id, a.roles); {
	case errors.Is(err, identity.ErrNotOperator):
		return ops.Operator{}, fmt.Errorf("%w: %w", ops.ErrNotOperator, err)
	// Authentication and the role gate are two queries, so an account
	// deleted between them surfaces as ErrUnknownAccount here as well. It
	// means the same thing in both places - the caller holds no account -
	// and so must map to the same 401, never a 500.
	case errors.Is(err, identity.ErrUnknownAccount):
		return ops.Operator{}, fmt.Errorf("%w: %w", ops.ErrUnauthenticated, err)
	case err != nil:
		return ops.Operator{}, err
	}
	return ops.Operator{ID: id.Subject, Email: id.Email, DisplayName: id.DisplayName}, nil
}

// accountAuth adapts the identity module to account's consumer-defined
// Authenticator. Deliberately thinner than editorAuth: there is no role to
// require, because every route in that module acts on the caller's own row
// and a reader owns theirs exactly as an editor owns theirs.
type accountAuth struct {
	ids *identity.Service
}

func (a accountAuth) Authenticate(ctx context.Context, token string) (account.Account, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return account.Account{}, fmt.Errorf("%w: %w", account.ErrUnauthenticated, err)
	case err != nil:
		return account.Account{}, err
	}
	return account.Account{ID: id.Subject}, nil
}

// newEditorAuth builds the identity-to-editorial adapter. It is the single
// construction point for editorAuth, so callers - production wiring and
// tests alike - go through the same path and a new dependency cannot be
// wired one way here and another way there.
func newEditorAuth(ids *identity.Service, roles identity.RoleLookup) editorAuth {
	return editorAuth{ids: ids, roles: roles}
}

// editorAuth adapts the identity module to editorial's consumer-defined
// EditorAuthenticator. The modules never import each other; this adapter
// in the composition root is where their error vocabularies meet:
// identity's "bad token" and "no account" both become editorial's 401, its
// "not an editor" the 403, and everything else stays a failure.
type editorAuth struct {
	ids   *identity.Service
	roles identity.RoleLookup
}

func (a editorAuth) AuthenticateEditor(ctx context.Context, token string) (editorial.Editor, error) {
	id, err := a.ids.Authenticate(ctx, token)
	switch {
	case errors.Is(err, identity.ErrInvalidToken), errors.Is(err, identity.ErrUnknownAccount):
		return editorial.Editor{}, fmt.Errorf("%w: %w", editorial.ErrUnauthenticated, err)
	case err != nil:
		return editorial.Editor{}, err
	}
	switch err := identity.RequireEditor(ctx, id, a.roles); {
	case errors.Is(err, identity.ErrNotEditor):
		return editorial.Editor{}, fmt.Errorf("%w: %w", editorial.ErrNotEditor, err)
	// Authentication and the role gate are two queries, so an account
	// deleted between them surfaces as ErrUnknownAccount here as well. It
	// means the same thing in both places - the caller holds no account -
	// and so must map to the same 401, never a 500.
	case errors.Is(err, identity.ErrUnknownAccount):
		return editorial.Editor{}, fmt.Errorf("%w: %w", editorial.ErrUnauthenticated, err)
	case err != nil:
		return editorial.Editor{}, err
	}
	return editorial.Editor{ID: id.Subject, Email: id.Email, DisplayName: id.DisplayName}, nil
}

// healthcheck performs one HTTP GET against the local /healthz endpoint and
// reports the outcome as an error. It reads only HTTP_ADDR - a liveness
// probe must not demand the full serving configuration.
func healthcheck(ctx context.Context, getenv func(string) string) error {
	url, err := healthURL(probeAddr(getenv))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered %s", url, resp.Status)
	}
	return nil
}

// probeAddr returns the configured listen address, applying the same :8080
// default as config.FromEnv without requiring the rest of the configuration.
func probeAddr(getenv func(string) string) string {
	if addr := getenv("HTTP_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}

// healthURL derives the probe URL from an HTTP listen address. An empty or
// wildcard host means the server listens on every interface, so the probe
// goes to localhost.
func healthURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("healthcheck: invalid HTTP_ADDR %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// readiness reports the process ready when the database answers a ping.
func readiness(pool *pgxpool.Pool) platformhttp.ReadinessCheck {
	return func(ctx context.Context) error {
		return pool.Ping(ctx)
	}
}
