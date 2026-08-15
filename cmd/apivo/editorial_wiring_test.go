package main

// Full-chain contract tests for the editorial wiring: a real JWKS server
// with freshly generated keys, real JWT verification through the identity
// module, the account.role lookup against the migrated schema, and the
// editorial handler behind its consumer-defined auth seam. This lives in
// the composition root because it is the only place the two modules may
// meet (the arch test forbids editorial importing identity, tests
// included).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// newSigningKey generates an RSA signing key wrapped as a jwk.Key carrying
// kid and alg, ready both for signing and for publication in a test JWKS.
func newSigningKey(t *testing.T) jwk.Key {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating rsa key: %v", err)
	}
	key, err := jwk.Import(raw)
	if err != nil {
		t.Fatalf("importing key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, "wiring-rsa"); err != nil {
		t.Fatalf("setting kid: %v", err)
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("setting alg: %v", err)
	}
	return key
}

// newJWKSServer serves the public half of the key as a JWKS.
func newJWKSServer(t *testing.T, key jwk.Key) *httptest.Server {
	t.Helper()
	pub, err := jwk.PublicKeyOf(key)
	if err != nil {
		t.Fatalf("deriving public key: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatalf("adding key to set: %v", err)
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshalling JWKS: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mintBearer signs a fresh RS256 token for the given subject.
func mintBearer(t *testing.T, key jwk.Key, subject string) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewBuilder().
		Subject(subject).
		IssuedAt(now.Add(-time.Minute)).
		Expiration(now.Add(time.Hour)).
		Build()
	if err != nil {
		t.Fatalf("building token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return string(signed)
}

// seedAccount inserts an account with the given role and returns its id.
func seedAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	suffix := id.String()[:8]
	if _, err := pool.Exec(ctx,
		`insert into account (id, email, display_name, role) values ($1, $2, $3, $4)`,
		id.String(), "wiring-"+suffix+"@example.test", "Wiring "+strings.ToUpper(role)+" "+suffix, role); err != nil {
		t.Fatalf("seed %s account: %v", role, err)
	}
	return id
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEditorialWiringAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the editorial wiring")
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

	editorID := seedAccount(ctx, t, pool, "editor")
	readerID := seedAccount(ctx, t, pool, "reader")

	key := newSigningKey(t)
	jwks := newJWKSServer(t, key)

	route, closeVerifier, err := newEditorialRoute(ctx, config.Config{JWKSURL: jwks.URL}, discardLogger(), pool)
	if err != nil {
		t.Fatalf("newEditorialRoute: %v", err)
	}
	t.Cleanup(closeVerifier)
	if route.Pattern != "/api/v1/editorial/" {
		t.Fatalf("route pattern = %q, want /api/v1/editorial/", route.Pattern)
	}

	post := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"name":"Wiring Feed ` + randomHex(t) + `","url":"https://example.test/feed/` + randomHex(t) +
			`","language":"el","jurisdiction":"GR","licence_terms":"Extract and link permitted"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/editorial/sources", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		route.Handler.ServeHTTP(rec, req)
		return rec
	}

	assertProblem := func(t *testing.T, rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, want application/problem+json", ct)
		}
	}

	t.Run("no token is 401", func(t *testing.T) {
		assertProblem(t, post(t, ""), http.StatusUnauthorized)
	})
	t.Run("garbage token is 401", func(t *testing.T) {
		assertProblem(t, post(t, "definitely.not.a-jwt"), http.StatusUnauthorized)
	})
	t.Run("valid token for an unprovisioned subject is 401", func(t *testing.T) {
		assertProblem(t, post(t, mintBearer(t, key, uuid.NewString())), http.StatusUnauthorized)
	})
	t.Run("reader is 403", func(t *testing.T) {
		assertProblem(t, post(t, mintBearer(t, key, readerID.String())), http.StatusForbidden)
	})
	t.Run("editor creates the source", func(t *testing.T) {
		rec := post(t, mintBearer(t, key, editorID.String()))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		var created struct {
			ID        string `json:"id"`
			UsageRule string `json:"usage_rule"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshalling 201 body: %v", err)
		}
		if _, err := uuid.Parse(created.ID); err != nil {
			t.Errorf("id %q is not a uuid: %v", created.ID, err)
		}
		if created.UsageRule != "extract_and_link" {
			t.Errorf("usage_rule = %q, want extract_and_link", created.UsageRule)
		}
	})
}

// vanishingRoles deletes the account row before delegating to the real
// database-backed lookup, forcing the window between authentication and
// the role gate: the token authenticated against an account that no longer
// exists by the time its role is read.
type vanishingRoles struct {
	pool *pgxpool.Pool
	real identity.RoleLookup
	t    *testing.T
}

func (v vanishingRoles) Role(ctx context.Context, accountID uuid.UUID) (string, error) {
	v.t.Helper()
	if _, err := v.pool.Exec(ctx, `delete from account where id = $1`, accountID.String()); err != nil {
		v.t.Fatalf("deleting account mid-request: %v", err)
	}
	return v.real.Role(ctx, accountID)
}

// TestEditorAuthMapsVanishedAccountTo401 covers the two-query window:
// authentication and the role gate read the account separately, so an
// account deleted in between surfaces as ErrUnknownAccount from the role
// lookup. It means the same thing in both places - the caller holds no
// account - and must be the documented 401, not a 500.
func TestEditorAuthMapsVanishedAccountTo401(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the adapter")
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

	editorID := seedAccount(ctx, t, pool, "editor")
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
	auth := newEditorAuth(
		identity.New(verifier, pool),
		vanishingRoles{pool: pool, real: identity.NewAccountRoles(pool), t: t},
	)

	_, err = auth.AuthenticateEditor(ctx, mintBearer(t, key, editorID.String()))
	if !errors.Is(err, editorial.ErrUnauthenticated) {
		t.Fatalf("AuthenticateEditor error = %v, want errors.Is(err, editorial.ErrUnauthenticated)", err)
	}
	if errors.Is(err, editorial.ErrNotEditor) {
		t.Error("a vanished account must not read as a role verdict")
	}

	// End to end, the middleware answers 401 rather than 500.
	otherID := seedAccount(ctx, t, pool, "editor")
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(pool), auth)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/editorial/sources", strings.NewReader(
		`{"name":"Vanished","url":"https://example.test/feed/`+randomHex(t)+`","language":"el","jurisdiction":"GR","licence_terms":"Terms"}`))
	req.Header.Set("Authorization", "Bearer "+mintBearer(t, key, otherID.String()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

// TestRunServesWithEditorialRoutes proves serve() wires the verifier and
// mounts the editorial routes when JWKS_URL is configured.
func TestRunServesWithEditorialRoutes(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	jwks := newJWKSServer(t, newSigningKey(t))
	ctx, cancel := context.WithCancel(context.Background())
	env := map[string]string{
		"DATABASE_URL": dbURL,
		"HTTP_ADDR":    "127.0.0.1:0",
		"JWKS_URL":     jwks.URL,
	}

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, envFrom(env), out) }()

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "starting") {
		if time.Now().After(deadline) {
			t.Fatalf("run() never reached the serving phase; output: %q", out.String())
		}
		select {
		case err := <-done:
			t.Fatalf("run() exited before serving: %v; output: %q", err, out.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if strings.Contains(out.String(), "editorial endpoints are not mounted") {
		t.Error("editorial endpoints reported unmounted despite JWKS_URL being set")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run() did not return after context cancellation")
	}
}

// TestRunWithoutJWKSStillServesLoudly pins the deliberate asymmetry:
// missing JWKS_URL leaves the editorial routes unmounted but must NOT stop
// the process - the reader path needs no bearer token, so an
// editorial-only misconfiguration must not take the public site down. The
// signal is an ERROR-level startup line naming the consequence, not a
// crash and not a warning to be deduced from a later 404.
func TestRunWithoutJWKSStillServesLoudly(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	env := map[string]string{
		"DATABASE_URL": dbURL,
		"HTTP_ADDR":    "127.0.0.1:0",
		// JWKS_URL deliberately absent.
	}

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, envFrom(env), out) }()

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "starting") {
		if time.Now().After(deadline) {
			t.Fatalf("run() never reached the serving phase; output: %q", out.String())
		}
		select {
		case err := <-done:
			t.Fatalf("run() exited before serving: %v; output: %q", err, out.String())
		case <-time.After(25 * time.Millisecond):
		}
	}

	logged := out.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("missing JWKS_URL must be logged at ERROR level; output: %q", logged)
	}
	for _, want := range []string{"JWKS_URL", "UNMOUNTED", "404"} {
		if !strings.Contains(logged, want) {
			t.Errorf("startup log does not name %q as the consequence; output: %q", want, logged)
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run() did not return after context cancellation")
	}
}

// TestRunFailsFastOnUnreachableJWKS proves a configured-but-broken JWKS
// endpoint fails startup instead of serving editorial routes that could
// never authenticate anyone.
func TestRunFailsFastOnUnreachableJWKS(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	env := map[string]string{
		"DATABASE_URL": dbURL,
		"HTTP_ADDR":    "127.0.0.1:0",
		// Port 0 is never a valid destination, so the connection fails
		// deterministically - unlike a closed httptest server, whose freed
		// ephemeral port a later parallel listener could be handed back,
		// turning this into a flake. Same reasoning as the healthcheck
		// test's "nothing listening" case in main_test.go.
		"JWKS_URL": "http://127.0.0.1:0/.well-known/jwks.json",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := run(ctx, nil, envFrom(env), io.Discard); err == nil {
		t.Fatal("run() with an unreachable JWKS endpoint: want error, got nil")
	}
}
