package main

// The wallet surface as the binary actually assembles it (T078).
//
// The module's own tests build the handler by hand. These build it the way
// production does - from a config, a pool and the identity service - because
// the mistakes that only show up here are the wiring ones: a route mounted
// without its subtree, a surface served when the product is off, and a
// ledger chosen by a driver name.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// walletRoutes builds the authenticated routes for a cashback deployment and
// picks out the wallet handler.
func walletRoutes(ctx context.Context, t *testing.T, pool *pgxpool.Pool, jwksURL string, threshold money.Amount) http.Handler {
	t.Helper()
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwksURL, Cashback: config.CashbackConfig{
			Enabled:         true,
			LedgerDriver:    config.LedgerDriverMemory,
			PayoutThreshold: threshold,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	for _, route := range routes {
		if route.Pattern == wallet.Prefix {
			return route.Handler
		}
	}
	t.Fatalf("no route mounted at %s", wallet.Prefix)
	return nil
}

// TestTheWalletSurfaceIsUnmountedWithoutTheFlag. With cashback off there is
// no ledger to project from, so serving the endpoint would promise a figure
// the process cannot compute.
func TestTheWalletSurfaceIsUnmountedWithoutTheFlag(t *testing.T) {
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
		if strings.HasPrefix(route.Pattern, wallet.Prefix) {
			t.Fatalf("%s is mounted although cashback is off", route.Pattern)
		}
	}
}

// TestTheWalletPathIsMountedWithItsSubtree keeps the module's single error
// convention true for a stray sub-path.
func TestTheWalletPathIsMountedWithItsSubtree(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jwks := newJWKSServer(t, newSigningKey(t))
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	var mounted []string
	for _, route := range routes {
		if strings.HasPrefix(route.Pattern, wallet.Prefix) {
			mounted = append(mounted, route.Pattern)
		}
	}
	for _, want := range []string{wallet.Prefix, wallet.Prefix + "/"} {
		if !slices.Contains(mounted, want) {
			t.Errorf("%q is not mounted; mounted: %v", want, mounted)
		}
	}
}

// TestTheWiredWalletAnswersItsOwnMember walks a real token through the real
// gate to a real ledger. Everything is zero because nothing has been
// credited - which is the answer a new member gets, and the one a wallet
// that could not resolve its accounts would fail to give at all.
func TestTheWiredWalletAnswersItsOwnMember(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	threshold := money.Amount{Minor: 2000, Currency: "EUR"}
	handler := walletRoutes(ctx, t, pool, jwks.URL, threshold)

	req := httptest.NewRequest(http.MethodGet, wallet.Prefix, nil)
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body is not a wallet: %v (%s)", err, rec.Body)
	}
	for _, field := range []string{"pending", "confirmed", "reserved", "paid_out"} {
		if got[field]["minor"] != float64(0) {
			t.Errorf("%s = %v, want 0: nothing has been credited", field, got[field]["minor"])
		}
		if got[field]["currency"] != "EUR" {
			t.Errorf("%s.currency = %v, want the threshold's EUR", field, got[field]["currency"])
		}
	}
	// The threshold reaches the member as configured, so a client cannot
	// show one figure while the server enforces another.
	if got["payout_threshold"]["minor"] != float64(2000) {
		t.Errorf("payout_threshold = %v, want 2000", got["payout_threshold"]["minor"])
	}
}

// TestTheWiredWalletSaysWhenTheDeploymentIsIncomplete. The no-Docker loop
// and CI enable cashback with no money configuration at all, and there the
// endpoint is served and says what is missing - a 404 would say the API is
// not here.
func TestTheWiredWalletSaysWhenTheDeploymentIsIncomplete(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	handler := walletRoutes(ctx, t, pool, jwks.URL, money.Amount{})

	req := httptest.NewRequest(http.MethodGet, wallet.Prefix, nil)
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body)
	}
}

// TestTheWiredHistoryAnswersItsOwnMember. The list is empty because nothing
// has been earned - which is the answer a new member gets, and the one a
// query whose joins could not resolve would fail to give at all.
func TestTheWiredHistoryAnswersItsOwnMember(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	handler := walletRoutes(ctx, t, pool, jwks.URL, money.Amount{Minor: 2000, Currency: "EUR"})

	req := httptest.NewRequest(http.MethodGet, wallet.Prefix+"/entries?lang=de", nil)
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body is not a page: %v (%s)", err, rec.Body)
	}
	if len(got.Items) != 0 {
		t.Errorf("returned %d entries, want none: nothing has been earned", len(got.Items))
	}
	if got.NextCursor != nil {
		t.Errorf("next_cursor = %v on an empty page, want null", *got.NextCursor)
	}
}
