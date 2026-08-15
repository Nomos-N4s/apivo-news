package editorial_test

// Contract tests for the editorial routes: status codes, problem+json
// error shapes and auth behaviour per specs/001-epiloyes-alpha/contracts/
// http-api.md. Auth verdicts are exercised through a canned
// EditorAuthenticator - the real JWT chain (JWKS, identity, role lookup)
// is wired and contract-tested in the composition root, which is the only
// place both modules may meet.

import (
	"context"
	"crypto/rand"
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

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// The module's DB seam is satisfied by the platform pool at wiring time;
// this is where that claim is checked at compile time.
var _ editorial.DB = (*pgxpool.Pool)(nil)

// Canned bearer tokens the fake authenticator understands.
const (
	editorToken = "editor-token"
	readerToken = "reader-token"
)

// testEditor is the identity behind editorToken.
var testEditor = editorial.Editor{
	ID:          uuid.MustParse("00000000-0000-4000-8000-000000000001"),
	Email:       "editor@example.test",
	DisplayName: "Contract Editor",
}

// errAuthDown simulates an authentication backend failure - not a verdict.
var errAuthDown = errors.New("jwks unreachable")

// errUnexpectedCall is what a fake answers for an operation the test under
// way must never reach.
var errUnexpectedCall = errors.New("this store operation must not be reached")

// fakeAuth resolves the canned tokens: editorToken to testEditor,
// readerToken to ErrNotEditor, "boom" to a backend failure, anything else
// to ErrUnauthenticated.
type fakeAuth struct{}

func (fakeAuth) AuthenticateEditor(_ context.Context, token string) (editorial.Editor, error) {
	switch token {
	case editorToken:
		return testEditor, nil
	case readerToken:
		return editorial.Editor{}, editorial.ErrNotEditor
	case "boom":
		return editorial.Editor{}, errAuthDown
	default:
		return editorial.Editor{}, editorial.ErrUnauthenticated
	}
}

// errStore fails every operation; it exercises the 500 paths a healthy
// database never produces on demand.
type errStore struct{ err error }

func (s errStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, s.err
}

func (s errStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, s.err
}

func (s errStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, s.err
}

func (s errStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, s.err
}

func (s errStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, s.err
}

// okStore returns a canned created source.
type okStore struct{ src editorial.Source }

func (s okStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return s.src, nil
}

func (s okStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, nil
}

func (s okStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s okStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s okStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

// recordingStore captures what the handler actually asked to persist.
type recordingStore struct{ got editorial.NewSource }

func (s *recordingStore) CreateSource(_ context.Context, src editorial.NewSource) (editorial.Source, error) {
	s.got = src
	return editorial.Source{ID: uuid.New(), URL: src.URL, UsageRule: "extract_and_link"}, nil
}

func (s *recordingStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, nil
}

func (s *recordingStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *recordingStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *recordingStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHandler(t *testing.T, store editorial.Store) http.Handler {
	t.Helper()
	return editorial.NewHandler(discardLogger(), store, fakeAuth{})
}

// doJSON performs a request with the given bearer token (empty for none)
// and raw body against the handler.
func doJSON(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// wantProblem asserts an RFC 9457 problem+json response with the given
// status and a detail containing wantDetail (empty skips the check).
func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, wantDetail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p platformhttp.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshalling problem body %q: %v", rec.Body.String(), err)
	}
	if p.Status != status {
		t.Errorf("problem.status = %d, want %d", p.Status, status)
	}
	if p.Title != http.StatusText(status) {
		t.Errorf("problem.title = %q, want %q", p.Title, http.StatusText(status))
	}
	if wantDetail != "" && !strings.Contains(p.Detail, wantDetail) {
		t.Errorf("problem.detail = %q, want it to mention %q", p.Detail, wantDetail)
	}
}

// validSourceBody returns a well-formed registration payload with a unique
// URL, as a JSON string.
func validSourceBody(t *testing.T) string {
	t.Helper()
	return `{
		"name": "Contract Feed ` + randomSuffix(t) + `",
		"url": "https://example.test/feed/` + randomSuffix(t) + `",
		"language": "el",
		"jurisdiction": "GR",
		"licence_terms": "Extract and link permitted per feed terms v1"
	}`
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func TestCreateSourceAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errors.New("store must not be reached")})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", "", validSourceBody(t))
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
		if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
			t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
		}
	})
	t.Run("401 with a non-bearer scheme", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/editorial/sources", strings.NewReader(validSourceBody(t)))
		req.Header.Set("Authorization", "Basic ZWQ6cHc=")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("401 with a blank bearer token", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/editorial/sources", strings.NewReader(validSourceBody(t)))
		req.Header.Set("Authorization", "Bearer    ")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("401 with an invalid token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", "not-a-real-token", validSourceBody(t))
		wantProblem(t, rec, http.StatusUnauthorized, "invalid")
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "invalid_token") {
			t.Errorf("WWW-Authenticate = %q, want an invalid_token challenge", got)
		}
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", readerToken, validSourceBody(t))
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
	t.Run("500 when authentication itself fails", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", "boom", validSourceBody(t))
		wantProblem(t, rec, http.StatusInternalServerError, "")
	})
	t.Run("auth gate covers unknown editorial paths", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/nope", "", "")
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
}

func TestCreateSourceValidation(t *testing.T) {
	t.Parallel()
	// The store must never be reached by an invalid payload.
	h := newHandler(t, errStore{err: errors.New("store must not be reached")})

	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		return doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, body)
	}

	t.Run("400 on a usage_rule value", func(t *testing.T) {
		t.Parallel()
		rec := post(t, `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T","usage_rule":"full_text"}`)
		wantProblem(t, rec, http.StatusBadRequest, "usage_rule is not accepted")
	})
	t.Run("400 on usage_rule null - present is present", func(t *testing.T) {
		t.Parallel()
		rec := post(t, `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T","usage_rule":null}`)
		wantProblem(t, rec, http.StatusBadRequest, "usage_rule is not accepted")
	})
	t.Run("400 on malformed JSON", func(t *testing.T) {
		t.Parallel()
		rec := post(t, `{"name": `)
		wantProblem(t, rec, http.StatusBadRequest, "not valid JSON")
	})
	t.Run("400 on an unknown field", func(t *testing.T) {
		t.Parallel()
		rec := post(t, `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T","permission_evidence":"granted"}`)
		wantProblem(t, rec, http.StatusBadRequest, "not valid JSON")
	})
	// Everything after the first document must be whitespace. A stray
	// closing delimiter is the interesting case: Decoder.More() answers
	// "is another value coming?", which is false for `]` and `}`, so a
	// More()-based check would wave these malformed bodies through.
	trailing := []struct {
		name    string
		trailer string
	}{
		{name: "a second object", trailer: `{"again":true}`},
		{name: "a stray closing bracket", trailer: `]`},
		{name: "a stray closing brace", trailer: `}`},
		{name: "a stray comma", trailer: `,`},
		{name: "a bare token", trailer: `garbage`},
	}
	for _, tc := range trailing {
		t.Run("400 on "+tc.name+" after the document", func(t *testing.T) {
			t.Parallel()
			rec := post(t, `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T"}`+tc.trailer)
			wantProblem(t, rec, http.StatusBadRequest, "single JSON document")
		})
	}
	t.Run("trailing whitespace is accepted", func(t *testing.T) {
		t.Parallel()
		// Whitespace after the document is not trailing input; a body a
		// pretty-printer produced must still be a valid request. The canned
		// store answers, so reaching 201 proves decoding succeeded.
		h := newHandler(t, okStore{src: editorial.Source{ID: uuid.New(), UsageRule: "extract_and_link"}})
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken,
			`{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T"}`+"\n\t \r\n")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
	})

	blankCases := []struct {
		name  string
		body  string
		field string
	}{
		{name: "missing name", body: `{"url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T"}`, field: "name"},
		{name: "blank name", body: `{"name":"   ","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"T"}`, field: "name"},
		{name: "missing url", body: `{"name":"N","language":"el","jurisdiction":"GR","licence_terms":"T"}`, field: "url"},
		{name: "missing language", body: `{"name":"N","url":"https://example.test/f","jurisdiction":"GR","licence_terms":"T"}`, field: "language"},
		{name: "missing jurisdiction", body: `{"name":"N","url":"https://example.test/f","language":"el","licence_terms":"T"}`, field: "jurisdiction"},
		{name: "missing licence_terms", body: `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR"}`, field: "licence_terms"},
		{name: "blank licence_terms", body: `{"name":"N","url":"https://example.test/f","language":"el","jurisdiction":"GR","licence_terms":"  "}`, field: "licence_terms"},
	}
	for _, tc := range blankCases {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			t.Parallel()
			wantProblem(t, post(t, tc.body), http.StatusBadRequest, tc.field)
		})
	}

	// source.url is the sole ingestion origin: a value the crawler cannot
	// fetch registers a source no poller will ever read, so it must be
	// refused here rather than persisted with a 201.
	badURLs := []struct {
		name string
		url  string
	}{
		{name: "no scheme", url: "not-a-url"},
		{name: "host without a scheme", url: "example.test/feed.xml"},
		{name: "scheme-relative", url: "//example.test/feed.xml"},
		{name: "relative path", url: "/feed.xml"},
		{name: "non-http scheme", url: "ftp://example.test/feed.xml"},
		{name: "file scheme", url: "file:///etc/passwd"},
		{name: "javascript scheme", url: "javascript:alert(1)"},
		{name: "scheme with no host", url: "http://"},
		{name: "port with no host", url: "http://:8080"},
		{name: "whitespace only", url: "   "},
	}
	for _, tc := range badURLs {
		t.Run("400 on url "+tc.name, func(t *testing.T) {
			t.Parallel()
			body := `{"name":"N","url":` + jsonString(t, tc.url) +
				`,"language":"el","jurisdiction":"GR","licence_terms":"T"}`
			wantProblem(t, post(t, body), http.StatusBadRequest, "url")
		})
	}

	goodURLs := []struct {
		name string
		url  string
	}{
		{name: "https", url: "https://example.test/feed.xml"},
		{name: "http", url: "http://example.test/feed.xml"},
		{name: "with port and query", url: "https://example.test:8443/feed?format=atom"},
		{name: "surrounding whitespace is trimmed", url: "  https://example.test/feed.xml  "},
	}
	for _, tc := range goodURLs {
		t.Run("201 on url "+tc.name, func(t *testing.T) {
			t.Parallel()
			// The canned store answers, so reaching 201 proves the URL
			// passed validation rather than that anything was persisted.
			h := newHandler(t, okStore{src: editorial.Source{ID: uuid.New(), UsageRule: "extract_and_link"}})
			body := `{"name":"N","url":` + jsonString(t, tc.url) +
				`,"language":"el","jurisdiction":"GR","licence_terms":"T"}`
			rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
			}
		})
	}
}

// TestCreateSourceTrimsURL pins that the stored feed URL is the trimmed
// one: the unique index that backs the 409 compares stored values, so
// leading or trailing whitespace must not smuggle a duplicate past it.
func TestCreateSourceTrimsURL(t *testing.T) {
	t.Parallel()
	rec := &recordingStore{}
	h := editorial.NewHandler(discardLogger(), rec, fakeAuth{})
	body := `{"name":"N","url":"  https://example.test/feed.xml  ","language":"el","jurisdiction":"GR","licence_terms":"T"}`
	if got := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, body); got.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", got.Code, http.StatusCreated, got.Body.String())
	}
	if want := "https://example.test/feed.xml"; rec.got.URL != want {
		t.Errorf("stored url = %q, want the trimmed %q", rec.got.URL, want)
	}
}

func TestCreateSourceStoreFailure(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errors.New("connection torn down")})
	rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, validSourceBody(t))
	wantProblem(t, rec, http.StatusInternalServerError, "")
	if strings.Contains(rec.Body.String(), "torn down") {
		t.Error("internal error detail leaked to the wire")
	}
}

func TestCreateSourceResponseShape(t *testing.T) {
	t.Parallel()
	created := editorial.Source{
		ID:           uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		Name:         "Shape Feed",
		URL:          "https://example.test/feed/shape",
		Language:     "el",
		Jurisdiction: "GR",
		LicenceTerms: "Extract and link permitted",
		UsageRule:    "extract_and_link",
		CreatedAt:    time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC),
	}
	h := newHandler(t, okStore{src: created})
	rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, validSourceBody(t))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling response: %v", err)
	}
	want := map[string]any{
		"id":            created.ID.String(),
		"name":          created.Name,
		"url":           created.URL,
		"language":      created.Language,
		"jurisdiction":  created.Jurisdiction,
		"licence_terms": created.LicenceTerms,
		"usage_rule":    created.UsageRule,
		"created_at":    "2026-08-14T10:30:00Z",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("response[%q] = %v, want %v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("response has %d fields, want %d: %v", len(got), len(want), got)
	}
}

// TestCreateSourceAgainstSchema exercises the full handler-to-database
// path: 201 with the row actually written, 409 on a duplicate feed URL,
// 400 on an unknown language - the real constraint and error mapping, not
// a simulation.
func TestCreateSourceAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the contract against Postgres")
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

	h := newHandler(t, editorial.NewPGStore(pool))

	feedURL := "https://example.test/feed/" + randomSuffix(t)
	name := "Integration Feed " + randomSuffix(t)
	body := `{"name":` + jsonString(t, name) + `,"url":` + jsonString(t, feedURL) +
		`,"language":"el","jurisdiction":"GR","licence_terms":"Extract and link permitted per feed terms v1"}`

	rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		UsageRule string `json:"usage_rule"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshalling 201 body: %v", err)
	}
	if _, err := uuid.Parse(created.ID); err != nil {
		t.Errorf("id %q is not a uuid: %v", created.ID, err)
	}
	if created.URL != feedURL {
		t.Errorf("url = %q, want %q", created.URL, feedURL)
	}
	if created.UsageRule != "extract_and_link" {
		t.Errorf("usage_rule = %q, want extract_and_link - never an input, always the default", created.UsageRule)
	}
	if _, err := time.Parse(time.RFC3339Nano, created.CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC 3339: %v", created.CreatedAt, err)
	}

	// The row exists with the database-defaulted usage rule.
	var dbRule string
	if err := pool.QueryRow(ctx, `select usage_rule from source where id = $1`, created.ID).Scan(&dbRule); err != nil {
		t.Fatalf("reading created source back: %v", err)
	}
	if dbRule != "extract_and_link" {
		t.Errorf("database usage_rule = %q, want extract_and_link", dbRule)
	}

	// Registering the same feed URL again is a conflict, and must not have
	// created a second row.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken,
		`{"name":"Second Registration","url":`+jsonString(t, feedURL)+`,"language":"de","jurisdiction":"DE","licence_terms":"Other terms"}`)
	wantProblem(t, rec, http.StatusConflict, "already registered")
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from source where url = $1`, feedURL).Scan(&count); err != nil {
		t.Fatalf("counting sources: %v", err)
	}
	if count != 1 {
		t.Errorf("source count for %q = %d, want 1", feedURL, count)
	}

	// A language outside the reference table is a validation failure.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/editorial/sources", editorToken,
		`{"name":"Unknown Language Feed","url":"https://example.test/feed/`+randomSuffix(t)+`","language":"xx","jurisdiction":"GR","licence_terms":"Terms"}`)
	wantProblem(t, rec, http.StatusBadRequest, "language")
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling %q: %v", s, err)
	}
	return string(b)
}
