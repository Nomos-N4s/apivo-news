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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v3/jwk"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
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
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
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
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
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
