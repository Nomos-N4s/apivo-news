package main

// Full-chain contract tests for the click-out wiring: a real JWKS server
// with freshly generated keys, real JWT verification through the identity
// module, and the click-out handler behind its consumer-defined auth seam.
//
// It lives in the composition root because that is the only place the two
// modules may meet - the arch test forbids the click-out module importing
// identity, tests included - and because what is under test is precisely the
// adapter between their error vocabularies, plus which routes exist at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// clickOutRoutes builds the authenticated routes for a cashback deployment
// and picks out the click-out handler.
func clickOutRoutes(ctx context.Context, t *testing.T, pool *pgxpool.Pool, jwksURL string) http.Handler {
	t.Helper()
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwksURL, Cashback: config.CashbackConfig{Enabled: true, LedgerDriver: config.LedgerDriverMemory}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	for _, route := range routes {
		if route.Pattern == clickout.Prefix {
			return route.Handler
		}
	}
	t.Fatalf("no %s route among %d authenticated routes", clickout.Prefix, len(routes))
	return nil
}

// TestClickOutWiringAgainstSchema walks the chain for the two answers the
// gate has. There is no third: this surface requires an account and nothing
// beyond one, so a reader reaches the handler exactly as anybody else would.
func TestClickOutWiringAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	memberID := seedAccount(ctx, t, pool, "reader")
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	h := clickOutRoutes(ctx, t, pool, jwks.URL)

	post := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, clickout.Prefix,
			strings.NewReader(`{"offer_id":"`+uuid.NewString()+`"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("no token creates no click", func(t *testing.T) {
		rec := post(t, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	})

	t.Run("a token for nobody provisioned here", func(t *testing.T) {
		rec := post(t, mintBearer(t, key, uuid.NewString()))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
		}
	})

	// A signed-in member reaches the handler, which then answers about the
	// offer rather than about the caller. 409 because the id names no live
	// band - which is the point: the gate is behind us.
	t.Run("a signed-in member reaches the handler", func(t *testing.T) {
		rec := post(t, mintBearer(t, key, memberID.String()))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusConflict, rec.Body.String())
		}
		var problem struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("answer is not problem+json: %v (body %q)", err, rec.Body.String())
		}
		if !strings.Contains(problem.Detail, "not available") {
			t.Errorf("detail = %q, want it to be about the offer", problem.Detail)
		}
	})
}

// TestTheClickOutSurfaceIsUnmountedWithoutTheFlag pins the mounting rule.
// With cashback off there is no catalogue to click through and no ledger to
// credit, so serving the endpoint would promise something the process
// cannot do at all.
func TestTheClickOutSurfaceIsUnmountedWithoutTheFlag(t *testing.T) {
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
		if strings.HasPrefix(route.Pattern, clickout.Prefix) {
			t.Fatalf("%s is mounted although cashback is off", route.Pattern)
		}
	}
}

// TestTheClickOutPathIsMountedWithItsSubtree keeps the module's single error
// convention true for a stray sub-path. ServeMux would otherwise redirect
// or hand it to whatever else claims the namespace, and the answer would
// stop being problem+json.
func TestTheClickOutPathIsMountedWithItsSubtree(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jwks := newJWKSServer(t, newSigningKey(t))
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{Enabled: true, LedgerDriver: config.LedgerDriverMemory}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	var mounted []string
	for _, route := range routes {
		if strings.HasPrefix(route.Pattern, clickout.Prefix) {
			mounted = append(mounted, route.Pattern)
		}
	}
	for _, want := range []string{clickout.Prefix, clickout.Prefix + "/"} {
		if !slices.Contains(mounted, want) {
			t.Errorf("%q is not mounted; mounted: %v", want, mounted)
		}
	}
}
