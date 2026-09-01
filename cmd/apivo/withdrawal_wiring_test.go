package main

// That POST /withdrawals is actually reachable in a running binary (T091).
//
// The module's own tests prove what the endpoint answers. What is proved
// here is what only the composition root can be wrong about: that the route
// is mounted at all, that it is mounted with its subtree, that the flag takes
// it away with the rest of cashback, and that a deployment which cannot pay
// out yet still SERVES it - answering what is missing instead of a 404 that
// would say the API is not here.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v3/jwk"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

// withdrawalRoutes wires the authenticated surface with cashback on and
// returns its routes, together with the key tokens must be signed with.
func withdrawalRoutes(t *testing.T, cashback config.CashbackConfig) ([]platformhttp.Route, jwk.Key) {
	t.Helper()
	ctx, pool := opsWiringPool(t)
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	cashback.Enabled = true
	if cashback.LedgerDriver == "" {
		cashback.LedgerDriver = config.LedgerDriverMemory
	}
	routes, _, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: cashback},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)
	return routes, key
}

// handlerAt returns the handler mounted at exactly this pattern.
func handlerAt(t *testing.T, routes []platformhttp.Route, pattern string) http.Handler {
	t.Helper()
	for _, route := range routes {
		if route.Pattern == pattern {
			return route.Handler
		}
	}
	t.Fatalf("nothing is mounted at %q", pattern)
	return nil
}

// TestTheWithdrawalPathIsMountedWithItsSubtree. Without the subtree a stray
// sub-path leaves the module's error convention and is answered by whatever
// else claims the namespace.
func TestTheWithdrawalPathIsMountedWithItsSubtree(t *testing.T) {
	t.Parallel()
	routes, _ := withdrawalRoutes(t, config.CashbackConfig{})

	var mounted []string
	for _, route := range routes {
		if strings.HasPrefix(route.Pattern, payout.Prefix) {
			mounted = append(mounted, route.Pattern)
		}
	}
	for _, want := range []string{payout.Prefix, payout.Prefix + "/"} {
		if !slices.Contains(mounted, want) {
			t.Errorf("%q is not mounted; mounted: %v", want, mounted)
		}
	}
}

// TestTheWithdrawalSurfaceIsUnmountedWithoutTheFlag. CASHBACK_ENABLED is one
// switch over the whole product; a withdrawal endpoint left behind it would
// be the one route that outlived the feature.
func TestTheWithdrawalSurfaceIsUnmountedWithoutTheFlag(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jwks := newJWKSServer(t, newSigningKey(t))
	routes, _, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL}, discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	for _, route := range routes {
		if strings.HasPrefix(route.Pattern, payout.Prefix) {
			t.Fatalf("%s is mounted although cashback is off", route.Pattern)
		}
	}
}

// TestTheWiredWithdrawalDemandsAToken. The gate is the module's, and this
// proves the composition root put it in front of the mounted handler rather
// than beside it.
func TestTheWiredWithdrawalDemandsAToken(t *testing.T) {
	t.Parallel()
	routes, _ := withdrawalRoutes(t, config.CashbackConfig{})
	handler := handlerAt(t, routes, payout.Prefix)

	req := httptest.NewRequest(http.MethodPost, payout.Prefix, strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
	}
}

// TestADeploymentThatCannotPayOutSaysSoOnTheEndpoint. Neither the threshold
// nor the receivable is set here - which is every environment short of
// production - and the answer has to be 503 saying so, rather than a 404
// saying the API is not here or a refusal to start that would take the
// wallet and the operator queue down with it.
//
// A real token through the real gate, because a 401 would prove only that
// the route exists. What is under test is the answer BEHIND the gate.
func TestADeploymentThatCannotPayOutSaysSoOnTheEndpoint(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)
	member := seedAccount(ctx, t, pool, "reader")
	routes, key := withdrawalRoutes(t, config.CashbackConfig{})
	handler := handlerAt(t, routes, payout.Prefix)

	body := `{"destination_id":"` + uuid.NewString() + `","amount":{"minor":5000,"currency":"EUR"}}`
	req := httptest.NewRequest(http.MethodPost, payout.Prefix, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	var problem platformhttp.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("the body is not a problem document: %v (%s)", err, rec.Body.String())
	}
	if problem.Status != http.StatusServiceUnavailable {
		t.Errorf("the document says %d, want it to agree with the status line", problem.Status)
	}
}

// TestTheSettlementSweepIsBuiltAndRegisterable is the wiring half of T146.
//
// Nothing else in this tree observes a payment arriving, so a sweep that was
// built and never registered would leave every payout stuck at submitted and
// paid_out reporting zero - with no test failing, because every service it
// calls still works. This asserts the composition root hands one back and
// that a scheduler accepts it under the name the operator sees in the logs.
func TestTheSettlementSweepIsBuiltAndRegisterable(t *testing.T) {
	ctx, pool := opsWiringPool(t)
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	_, sweep, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)
	if sweep == nil {
		t.Fatal("cashback is on and no settlement sweep was built; no payout would ever leave submitted")
	}

	// Through the same function serve calls, so this covers the
	// registration rather than only the sweep's ability to be registered.
	jobs := scheduler.New(slog.New(slog.DiscardHandler), &networkWiringLocker{}, scheduler.Config{})
	switch added, err := registerSettlement(ctx, discardLogger(), jobs, sweep); {
	case err != nil:
		t.Fatalf("registerSettlement(): %v", err)
	case added != 1:
		// A job registered and not counted is a pool sized for fewer jobs
		// than are running.
		t.Errorf("registerSettlement counted %d jobs, want 1", added)
	}
	if _, err := jobs.RunOnce(ctx, payout.SettlementJobName); err != nil && strings.Contains(err.Error(), "no job named") {
		t.Errorf("%q was not registered", payout.SettlementJobName)
	}
}

// TestRegisteringNoSweepCountsNoJob. The capacity check is computed from the
// count, so a nil sweep that reported one would size the pool for a job that
// is not there.
func TestRegisteringNoSweepCountsNoJob(t *testing.T) {
	jobs := scheduler.New(slog.New(slog.DiscardHandler), &networkWiringLocker{}, scheduler.Config{})
	switch added, err := registerSettlement(t.Context(), discardLogger(), jobs, nil); {
	case err != nil:
		t.Fatalf("registerSettlement(nil): %v", err)
	case added != 0:
		t.Errorf("registerSettlement(nil) counted %d jobs, want 0", added)
	}
}

// TestNoSettlementSweepWhereThereIsNothingToSettle. With cashback off there
// are no payouts, and a job waking every five minutes to read an empty table
// is one more thing an operator has to explain.
func TestNoSettlementSweepWhereThereIsNothingToSettle(t *testing.T) {
	ctx, pool := opsWiringPool(t)
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	_, sweep, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL}, discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)
	if sweep != nil {
		t.Error("cashback is off and a settlement sweep was built anyway")
	}
}

// TestTheDescriptorAMemberReadsOnTheirStatement. It comes from the brand
// (FR-070, FR-073) because no rail may carry a product name, and a deployment
// with no brand gets the absence of one rather than an unmounted queue.
func TestTheDescriptorAMemberReadsOnTheirStatement(t *testing.T) {
	t.Parallel()
	if got := payoutDescriptor(wallet.Terms{Brand: "FIXTURE DE"}); got != "FIXTURE DE" {
		t.Errorf("payoutDescriptor = %q, want the brand", got)
	}
	// Never blank: payout.NewApprovals refuses one, and the alternative to
	// a placeholder is an operator queue that disappears.
	if got := payoutDescriptor(wallet.Terms{}); got == "" {
		t.Error("a deployment with no brand got a blank descriptor; the approver refuses one")
	}
}

// TestEveryLedgerDriverSelectsOne. There is no default and there must not be
// one: a deployment that forgot LEDGER_DRIVER would otherwise get the
// in-process ledger, which persists nothing and looks healthy doing it.
func TestEveryLedgerDriverSelectsOne(t *testing.T) {
	ctx, pool := opsWiringPool(t)
	_ = ctx

	for name, cashback := range map[string]config.CashbackConfig{
		"memory":   {LedgerDriver: config.LedgerDriverMemory},
		"postgres": {LedgerDriver: config.LedgerDriverPostgres},
		// Constructed, not dialled: a ledger that reached its sidecar here
		// would make start-up depend on a service being up.
		"blnk": {LedgerDriver: config.LedgerDriverBlnk, BlnkURL: "http://blnk.invalid"},
		// The secret is optional, and a deployment that set one must get a
		// client that carries it rather than one that quietly does not.
		"blnk with a secret": {
			LedgerDriver:  config.LedgerDriverBlnk,
			BlnkURL:       "http://blnk.invalid",
			BlnkSecretKey: config.NewSecret("not-a-real-key"),
		},
	} {
		ledger, err := newLedger(config.Config{Cashback: cashback}, pool)
		if err != nil {
			t.Errorf("%s: newLedger() = %v", name, err)
		}
		if ledger == nil {
			t.Errorf("%s: newLedger() selected no ledger", name)
		}
	}
	// config.parseCashback refuses an unknown name, so this arm is
	// unreachable through configuration - and says so rather than choosing.
	if _, err := newLedger(config.Config{Cashback: config.CashbackConfig{LedgerDriver: "carrier-pigeon"}}, pool); err == nil {
		t.Error("an unknown LEDGER_DRIVER selected a ledger, want a refusal naming it")
	}
}
