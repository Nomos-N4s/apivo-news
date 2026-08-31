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
	"os"
	"path/filepath"
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

// TestTheParticipationPathIsMountedWithItsSubtree. The opt-in is a SIBLING
// of the wallet, served by the same handler, and it needs the same two
// patterns for the same reason: without the subtree a stray sub-path leaves
// the module's error convention.
func TestTheParticipationPathIsMountedWithItsSubtree(t *testing.T) {
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
		if strings.HasPrefix(route.Pattern, wallet.ParticipationPrefix) {
			mounted = append(mounted, route.Pattern)
		}
	}
	for _, want := range []string{wallet.ParticipationPrefix, wallet.ParticipationPrefix + "/"} {
		if !slices.Contains(mounted, want) {
			t.Errorf("%q is not mounted; mounted: %v", want, mounted)
		}
	}
}

// With cashback off there is nothing to opt into, so the opt-in must not be
// served either - the flag is one switch over the whole product, not over
// the wallet alone.
func TestTheParticipationSurfaceIsUnmountedWithoutTheFlag(t *testing.T) {
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
		if strings.HasPrefix(route.Pattern, wallet.ParticipationPrefix) {
			t.Fatalf("%s is mounted although cashback is off", route.Pattern)
		}
	}
}

// A deployment with no BRAND_DIR starts and serves the opt-in, which then
// says what is missing. The alternative - refusing to start, or leaving the
// route unmounted so it answers 404 - would either take a whole deployment
// down over a member-facing feature or tell a client the API is not there.
func TestNoBrandDirStillMountsTheOptIn(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jwks := newJWKSServer(t, newSigningKey(t))
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("a deployment with no BRAND_DIR was refused: %v", err)
	}
	t.Cleanup(closeVerifier)

	for _, route := range routes {
		if route.Pattern == wallet.ParticipationPrefix {
			return
		}
	}
	t.Fatalf("no route mounted at %s", wallet.ParticipationPrefix)
}

// A BRAND_DIR that is set and does not hold a readable, complete brand
// definition IS a startup failure. Starting anyway would serve the 503
// while an operator believed the brand was configured.
func TestABrandDirThatHoldsNoBrandRefusesToStart(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	jwks := newJWKSServer(t, newSigningKey(t))
	_, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, BrandDir: t.TempDir(), Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if closeVerifier != nil {
		t.Cleanup(closeVerifier)
	}
	if err == nil {
		t.Fatal("an empty BRAND_DIR started; a deployment that named a brand meant it")
	}
	if !strings.Contains(err.Error(), "BRAND_DIR") {
		t.Errorf("the refusal %q does not name the key an operator has to fix", err)
	}
}

// brandDir writes a valid brand definition into a directory of its own and
// answers the path.
//
// It COPIES the brand package's own fixture rather than carrying a second
// definition. A hand-written one here would be a copy of a shape only
// Validate() knows, and the day the brand gained a required field this test
// would be the one place still passing with a brand nobody could deploy.
func brandDir(t *testing.T) string {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "..",
		"internal", "platform", "brand", "testdata", "fixture", "brand.json"))
	if err != nil {
		t.Fatalf("reading the brand fixture: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brand.json"), definition, 0o600); err != nil {
		t.Fatalf("writing the brand definition: %v", err)
	}
	return dir
}

// TestTheWiredOptInRecordsWhatTheBrandSays walks a real token through the
// real gate to a real database, and opts in.
//
// The values the member ends up with come from the brand FILE - the terms
// version they must send, and the currency their wallet is denominated in -
// which is the whole of what BRAND_DIR is for, and the thing no test that
// builds Terms by hand can show.
func TestTheWiredOptInRecordsWhatTheBrandSays(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, BrandDir: brandDir(t), Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	var handler http.Handler
	for _, route := range routes {
		if route.Pattern == wallet.ParticipationPrefix {
			handler = route.Handler
		}
	}
	if handler == nil {
		t.Fatalf("no route mounted at %s", wallet.ParticipationPrefix)
	}
	token := mintBearer(t, key, member.String())

	// The version in the fixture, sent by a client that read it from the
	// brand rather than from this test's imagination.
	req := httptest.NewRequest(http.MethodPost, wallet.ParticipationPrefix,
		strings.NewReader(`{"terms_version":"3.1.0"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body is not a participation: %v (%s)", err, rec.Body)
	}
	if got["terms_version"] != "3.1.0" {
		t.Errorf("terms_version = %v, want the brand's 3.1.0", got["terms_version"])
	}
	if got["default_currency"] != "SEK" {
		t.Errorf("default_currency = %v, want the brand's SEK", got["default_currency"])
	}

	// And the brand id the row carries is the file's, which is the tenant
	// boundary ADR-0004 draws and the one field the response does not show.
	var brandID string
	if err := pool.QueryRow(ctx,
		`select brand_id from cashback.participation where account_id = $1`, member).Scan(&brandID); err != nil {
		t.Fatalf("reading the participation back: %v", err)
	}
	if brandID != "zephyra" {
		t.Errorf("brand_id = %q, want the brand file's own id", brandID)
	}
	t.Cleanup(func() {
		// The guard refuses a delete, so the row is closed rather than
		// removed: this pool is shared and a later run must find the
		// account free of an ACTIVE participation.
		if _, err := pool.Exec(ctx,
			`update cashback.participation set status = 'left', left_at = now() where account_id = $1`,
			member); err != nil {
			t.Errorf("closing the participation: %v", err)
		}
	})
}

// A deployment with no brand serves the opt-in and says what is missing,
// naming the key rather than answering 404.
func TestTheWiredOptInSaysWhenThereIsNoBrand(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	handler := walletRoutes(ctx, t, pool, jwks.URL, money.Amount{})

	req := httptest.NewRequest(http.MethodPost, wallet.ParticipationPrefix,
		strings.NewReader(`{"terms_version":"3.1.0"}`))
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body)
	}
}

// TestTheExportPathIsMountedWithItsSubtree. The third tree the member
// surface serves, and it needs the same two patterns for the same reason.
func TestTheExportPathIsMountedWithItsSubtree(t *testing.T) {
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
		if strings.HasPrefix(route.Pattern, wallet.ExportPrefix) {
			mounted = append(mounted, route.Pattern)
		}
	}
	for _, want := range []string{wallet.ExportPrefix, wallet.ExportPrefix + "/"} {
		if !slices.Contains(mounted, want) {
			t.Errorf("%q is not mounted; mounted: %v", want, mounted)
		}
	}
}

// TestTheWiredExportAnswersItsOwnMember walks a real token through the real
// gate to a real query. The document is empty because nothing has been
// earned - which is the answer a new member gets, and the one an export
// whose joins could not resolve would fail to give at all.
func TestTheWiredExportAnswersItsOwnMember(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	member := seedAccount(ctx, t, pool, "reader")
	routes, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{
			Enabled: true, LedgerDriver: config.LedgerDriverMemory,
		}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	var handler http.Handler
	for _, route := range routes {
		if route.Pattern == wallet.ExportPrefix {
			handler = route.Handler
		}
	}
	if handler == nil {
		t.Fatalf("no route mounted at %s", wallet.ExportPrefix)
	}

	for _, tc := range []struct{ query, wantType string }{
		{"", "application/json"},
		{"?format=csv&lang=de", "text/csv"},
	} {
		req := httptest.NewRequest(http.MethodGet, wallet.ExportPrefix+tc.query, nil)
		req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, member.String()))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%q answered %d, want 200 (%s)", tc.query, rec.Code, rec.Body)
			continue
		}
		if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, tc.wantType) {
			t.Errorf("%q sent Content-Type %q, want %s", tc.query, kind, tc.wantType)
		}
	}
}
