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
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
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
		authenticated, closeVerifier, err := newAuthenticatedRoutes(ctx, cfg, log, pool, adapter)
		if err != nil {
			return err
		}
		defer closeVerifier()
		routes = append(routes, authenticated...)
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
		// arithmetic, so registering the sweeps takes the requirement from
		// 4 to 8. pgx defaults MaxConns to max(4, NumCPU), which is why
		// turning ingestion on may mean raising pool_max_conns in
		// DATABASE_URL - the error below says so with the numbers in it.
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
func newAuthenticatedRoutes(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, adapter networks.Network) ([]platformhttp.Route, func(), error) {
	verifier, err := identity.NewVerifier(ctx, identity.VerifierConfig{
		JWKSURL:  cfg.JWKSURL,
		Audience: cfg.JWTAudience,
	})
	if err != nil {
		return nil, nil, err
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
		return routes, stop, nil
	}
	opsStore, err := ops.NewPGStore(pool)
	if err != nil {
		stop()
		return nil, nil, err
	}
	clickouts, err := newClickOuts(pool, adapter)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return append(routes,
		platformhttp.Route{
			Pattern: opsPrefix,
			Handler: ops.NewHandler(log, opsStore, newOperatorAuth(ids, roles)),
		},
		// Mounted at the path AND at its subtree, so a stray sub-path is
		// answered in problem+json by the module rather than redirected by
		// ServeMux or handed to whatever else claims the namespace.
		platformhttp.Route{
			Pattern: clickoutPrefix,
			Handler: clickout.NewHandler(log, clickouts, memberAuth{ids: ids}),
		},
		platformhttp.Route{
			Pattern: clickoutPrefix + "/",
			Handler: clickout.NewHandler(log, clickouts, memberAuth{ids: ids}),
		},
	), stop, nil
}

// newClickOuts assembles the click-out service: the catalogue read that says
// what a band is, the recorder that writes the click, and the adapters that
// can build a redirect.
//
// A nil adapter is passed through as an EMPTY registry rather than refused.
// The endpoint is then served and every request answers 502 naming the
// network nothing can build a redirect for - which is the truth, and is
// findable, where a 404 would say the API is not here at all.
func newClickOuts(pool *pgxpool.Pool, adapter networks.Network) (*clickout.ClickOuts, error) {
	clicks, err := clickout.NewClicks(clickoutstore.New(pool))
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
	return clickout.NewClickOuts(catalogue.NewOfferReader(cataloguestore.New(pool)), clicks, deeplinks)
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
