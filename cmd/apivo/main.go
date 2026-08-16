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

	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
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
		log.ErrorContext(ctx, "JWKS_URL is not set: every /api/v1/editorial/ route is UNMOUNTED and will answer 404; reader endpoints are unaffected. Set JWKS_URL to the auth provider JWKS endpoint to enable editorial endpoints")
	} else {
		editorialRoute, closeVerifier, err := newEditorialRoute(ctx, cfg, log, pool)
		if err != nil {
			return err
		}
		defer closeVerifier()
		routes = append(routes, editorialRoute)
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

	srv := platformhttp.New(log, cfg.HTTPAddr, version, readiness(pool), routes...)
	// The reader endpoints need no bearer token, so they mount
	// unconditionally - a missing JWKS_URL costs the editorial routes, never
	// the public site.
	srv.Mount(readerPrefix, content.NewHandler(log, pool))
	log.InfoContext(ctx, "starting", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", version)
	return srv.Run(ctx)
}

// newEditorialRoute wires the editorial module: JWT verification against
// the configured JWKS, subject-to-account mapping and the account.role
// lookup from identity, adapted behind editorial's consumer-defined auth
// seam. The returned func stops the verifier's background JWKS refreshing.
func newEditorialRoute(ctx context.Context, cfg config.Config, log *slog.Logger, pool *pgxpool.Pool) (platformhttp.Route, func(), error) {
	verifier, err := identity.NewVerifier(ctx, identity.VerifierConfig{
		JWKSURL:  cfg.JWKSURL,
		Audience: cfg.JWTAudience,
	})
	if err != nil {
		return platformhttp.Route{}, nil, err
	}
	auth := newEditorAuth(identity.New(verifier, pool), identity.NewAccountRoles(pool))
	return platformhttp.Route{
		Pattern: editorialPrefix,
		Handler: editorial.NewHandler(log, editorial.NewPGStore(pool), auth),
	}, func() { _ = verifier.Close(context.Background()) }, nil
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
