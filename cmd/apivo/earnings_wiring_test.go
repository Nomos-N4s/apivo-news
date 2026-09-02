package main

// Whether a deployment gets the crediting job, what it is told when it does
// not, and that the HOLD_* configuration reaches the rules inside it (#435,
// T118).
//
// Every wrong answer here is a quiet one: a deployment with no job would
// store every report the networks send and credit none of them, with
// nothing failing, because every service the job calls still works.

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	walletmemory "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// creditingConfigured is a deployment with everything the job needs and
// two rules set.
func creditingConfigured() config.Config {
	return config.Config{Cashback: config.CashbackConfig{
		Enabled:      true,
		LedgerDriver: config.LedgerDriverMemory,
		HouseAccounts: config.HouseAccountsConfig{
			NetworkReceivable: "network-receivable",
		},
		HoldRules: config.HoldRulesConfig{
			SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour,
			SaleCap: money.Amount{Minor: 50_000, Currency: "EUR"},
		},
	}}
}

func TestAConfiguredDeploymentGetsTheCreditingJobWithItsRules(t *testing.T) {
	t.Parallel()
	_, pool := opsWiringPool(t)

	job, missing, err := newEarningsLifecycle(discardLogger(), creditingConfigured(), pool, walletmemory.New())
	if err != nil {
		t.Fatalf("newEarningsLifecycle(): %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("a fully configured deployment reports %v missing", missing)
	}
	if job == nil {
		t.Fatal("a fully configured deployment got no crediting job")
	}
	// The rules the job runs are the ones the environment set, and only
	// those - a rule that reached the job unset would hold nothing while
	// looking like protection, and one that reached it set when it was not
	// would hold credits nobody asked to hold.
	want := []string{earnings.RuleSharedContext, earnings.RuleSaleCap}
	if got := job.Rules().Active(); !slices.Equal(got, want) {
		t.Errorf("the job runs %v, want %v", got, want)
	}
	if got := job.Rules().SaleCap; got != (money.Amount{Minor: 50_000, Currency: "EUR"}) {
		t.Errorf("the sale cap reached the job as %v", got)
	}
}

// TestADeploymentWithNoHouseAccountIsToldWhichKeyItLacks. Not a startup
// failure here - production refuses in config, and the environments that
// reach this are the no-Docker loop and CI - but never a silent one.
func TestADeploymentWithNoHouseAccountIsToldWhichKeyItLacks(t *testing.T) {
	t.Parallel()
	_, pool := opsWiringPool(t)

	unnamed := creditingConfigured()
	unnamed.Cashback.HouseAccounts.NetworkReceivable = ""
	job, missing, err := newEarningsLifecycle(discardLogger(), unnamed, pool, walletmemory.New())
	if err != nil {
		t.Fatalf("newEarningsLifecycle(): %v", err)
	}
	if job != nil {
		t.Error("a deployment with no house account got a crediting job that would pay out of nowhere")
	}
	if !slices.Equal(missing, []string{"HOUSE_ACCOUNT_NETWORK_RECEIVABLE"}) {
		t.Errorf("missing = %v, want the one key", missing)
	}
}

// TestRulesThatCannotRunAreAStartupFailure. Config refuses a half-configured
// set first; this is the rules refusing it again, so that a Config built by
// hand cannot reach a job that would then fail on the first report.
func TestRulesThatCannotRunAreAStartupFailure(t *testing.T) {
	t.Parallel()
	_, pool := opsWiringPool(t)

	half := creditingConfigured()
	half.Cashback.HoldRules = config.HoldRulesConfig{MemberVelocity: 5}
	if _, _, err := newEarningsLifecycle(discardLogger(), half, pool, walletmemory.New()); err == nil {
		t.Fatal("a velocity count with no window built a job")
	}
}

// TestTheCreditingJobIsBuiltWithTheRoutesAndRegisterable. Through the same
// function serve calls, so this covers the registration rather than only
// the job's ability to be registered - and proves it is built beside the
// ledger the routes share rather than over one of its own.
func TestTheCreditingJobIsBuiltWithTheRoutesAndRegisterable(t *testing.T) {
	ctx, pool := opsWiringPool(t)
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	cfg := creditingConfigured()
	cfg.JWKSURL = jwks.URL
	_, built, closeVerifier, err := newAuthenticatedRoutes(ctx, cfg, discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)
	if built == nil || built.lifecycle == nil {
		t.Fatal("cashback is on and no crediting job was built; nothing the networks report would ever be owed")
	}

	jobs := scheduler.New(slog.New(slog.DiscardHandler), &networkWiringLocker{}, scheduler.Config{})
	switch added, err := registerLifecycle(ctx, discardLogger(), jobs, built.lifecycle); {
	case err != nil:
		t.Fatalf("registerLifecycle(): %v", err)
	case added != 1:
		// A job registered and not counted is a pool sized for fewer jobs
		// than are running.
		t.Errorf("registerLifecycle counted %d jobs, want 1", added)
	}
	if _, err := jobs.RunOnce(ctx, earnings.LifecycleJobName); err != nil && strings.Contains(err.Error(), "no job named") {
		t.Errorf("%q was not registered", earnings.LifecycleJobName)
	}
}

// TestRegisteringNoLifecycleCountsNoJob. The capacity check is computed from
// the count, so a nil job that reported one would size the pool for a job
// that is not there.
func TestRegisteringNoLifecycleCountsNoJob(t *testing.T) {
	t.Parallel()
	jobs := scheduler.New(slog.New(slog.DiscardHandler), &networkWiringLocker{}, scheduler.Config{})
	switch added, err := registerLifecycle(t.Context(), discardLogger(), jobs, nil); {
	case err != nil:
		t.Fatalf("registerLifecycle(nil): %v", err)
	case added != 0:
		t.Errorf("registerLifecycle(nil) counted %d jobs, want 0", added)
	}
}

// TestNoCreditingJobWhereTheHouseAccountIsUnnamed. The surface still mounts
// - every other route works - and the jobs bundle carries a nil job that
// the registration counts as none.
func TestNoCreditingJobWhereTheHouseAccountIsUnnamed(t *testing.T) {
	ctx, pool := opsWiringPool(t)
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	_, built, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)
	if built == nil {
		t.Fatal("cashback is on and no jobs bundle came back")
	}
	if built.lifecycle != nil {
		t.Error("no house account is named and a crediting job was built anyway")
	}
}
