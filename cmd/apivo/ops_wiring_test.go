package main

// Full-chain contract tests for the cashback operator wiring: a real JWKS
// server with freshly generated keys, real JWT verification through the
// identity module, the account.role lookup against the migrated schema, and
// the operator handler behind its consumer-defined auth seam.
//
// It lives in the composition root because that is the only place the two
// modules may meet - the arch test forbids the operator module importing
// identity, tests included - and because the thing under test is precisely
// the adapter between their error vocabularies.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// opsWiringPool connects to the migrated schema, skipping when no database
// is configured.
func opsWiringPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the operator wiring")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// TestOperatorWiringAgainstSchema walks the whole chain for each of the
// three answers the gate has: no token, a token whose account holds the
// wrong role, and a token whose account holds the operator role.
//
// The editor case is the one worth having. Both roles are real authorities
// in this system and both pass JWT verification identically; only the role
// lookup separates them, and it is the separation between approving an
// article and deciding who gets paid.
func TestOperatorWiringAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	operatorID := seedAccount(ctx, t, pool, "operator")
	editorID := seedAccount(ctx, t, pool, "editor")
	readerID := seedAccount(ctx, t, pool, "reader")

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	routes, _, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{Enabled: true, LedgerDriver: config.LedgerDriverMemory}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	// Picked out by pattern rather than by position: the list's order is
	// not part of the contract.
	var route platformhttp.Route
	for _, candidate := range routes {
		if candidate.Pattern == ops.Prefix {
			route = candidate
		}
	}
	if route.Handler == nil {
		t.Fatalf("no %s route among %d authenticated routes", ops.Prefix, len(routes))
	}

	get := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, ops.Prefix+"unattributed", strings.NewReader(""))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		route.Handler.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name    string
		subject string
		want    int
	}{
		{name: "no token at all", want: http.StatusUnauthorized},
		{name: "a token for nobody provisioned here", subject: uuid.NewString(), want: http.StatusUnauthorized},
		{name: "a reader", subject: readerID.String(), want: http.StatusForbidden},
		{name: "an editor", subject: editorID.String(), want: http.StatusForbidden},
		{name: "an operator", subject: operatorID.String(), want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var token string
			if tc.subject != "" {
				token = mintBearer(t, key, tc.subject)
			}
			rec := get(t, token)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestOperatorAuthMapsVanishedAccountTo401 covers the two-query window, as
// its editorial counterpart does: authentication and the role gate read the
// account separately, so an account deleted in between surfaces as
// ErrUnknownAccount from the role lookup. It means the same thing in both
// places - the caller holds no account - and must be the documented 401,
// never a 403 that would tell somebody their authority was revoked.
func TestOperatorAuthMapsVanishedAccountTo401(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	operatorID := seedAccount(ctx, t, pool, "operator")
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	verifier, err := identity.NewVerifier(ctx, identity.VerifierConfig{JWKSURL: jwks.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	t.Cleanup(func() { _ = verifier.Close(context.Background()) })

	// Built through the same constructor the composition root uses, so a
	// change to how the adapter is assembled is exercised here too; only
	// the role source is substituted, since forcing the delete-between-
	// queries window is the whole point of this test.
	auth := newOperatorAuth(
		identity.New(verifier, pool),
		vanishingRoles{pool: pool, real: identity.NewAccountRoles(pool), t: t},
	)

	_, err = auth.AuthenticateOperator(ctx, mintBearer(t, key, operatorID.String()))
	if !errors.Is(err, ops.ErrUnauthenticated) {
		t.Fatalf("AuthenticateOperator error = %v, want errors.Is(err, ops.ErrUnauthenticated)", err)
	}
	if errors.Is(err, ops.ErrNotOperator) {
		t.Error("a vanished account must not read as a role verdict")
	}
}

// TestTheOperatorSurfaceIsUnmountedWithoutTheFlag pins the other half of
// the mounting rule. With cashback off nothing fills the queue, so serving
// a surface for deciding it would show an operator an empty page that means
// "no work" when it means "no product".
func TestTheOperatorSurfaceIsUnmountedWithoutTheFlag(t *testing.T) {
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
		if route.Pattern == ops.Prefix {
			t.Fatalf("%s is mounted although cashback is off", ops.Prefix)
		}
	}
	// The authenticated routes that do not depend on the flag are still
	// there: turning cashback off must not take editorial down with it.
	if len(routes) != 2 {
		t.Errorf("%d authenticated route(s), want the two that need no feature flag", len(routes))
	}
}

// TestTheOperatorSurfaceAnswersProblemJSON keeps the module's single error
// convention true through the mounted route rather than only in its own
// tests: the whole prefix is problem+json, including the paths nobody
// serves.
func TestTheOperatorSurfaceAnswersProblemJSON(t *testing.T) {
	t.Parallel()
	ctx, pool := opsWiringPool(t)

	operatorID := seedAccount(ctx, t, pool, "operator")
	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)
	routes, _, closeVerifier, err := newAuthenticatedRoutes(ctx,
		config.Config{JWKSURL: jwks.URL, Cashback: config.CashbackConfig{Enabled: true, LedgerDriver: config.LedgerDriverMemory}},
		discardLogger(), pool, nil)
	if err != nil {
		t.Fatalf("newAuthenticatedRoutes: %v", err)
	}
	t.Cleanup(closeVerifier)

	var handler http.Handler
	for _, route := range routes {
		if route.Pattern == ops.Prefix {
			handler = route.Handler
		}
	}
	if handler == nil {
		t.Fatalf("no %s route among %d authenticated routes", ops.Prefix, len(routes))
	}

	req := httptest.NewRequest(http.MethodGet, ops.Prefix+"held", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, operatorID.String()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("answer is not problem+json: %v (body %q)", err, rec.Body.String())
	}
	if !strings.Contains(problem.Detail, "no such endpoint") {
		t.Errorf("detail = %q, want the catch-all's answer", problem.Detail)
	}
}
