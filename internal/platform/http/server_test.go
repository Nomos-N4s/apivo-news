package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/api"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	failing := func(context.Context) error { return errors.New("db down") }
	passing := func(context.Context) error { return nil }

	tests := []struct {
		name     string
		ready    platformhttp.ReadinessCheck
		path     string
		method   string
		wantCode int
	}{
		{name: "healthz ok", ready: passing, path: "/healthz", method: http.MethodGet, wantCode: http.StatusOK},
		{name: "readyz ok", ready: passing, path: "/readyz", method: http.MethodGet, wantCode: http.StatusOK},
		{name: "readyz nil check is ok", ready: nil, path: "/readyz", method: http.MethodGet, wantCode: http.StatusOK},
		{name: "readyz failing check", ready: failing, path: "/readyz", method: http.MethodGet, wantCode: http.StatusServiceUnavailable},
		{name: "unknown path", ready: passing, path: "/nope", method: http.MethodGet, wantCode: http.StatusNotFound},
		{name: "healthz wrong method", ready: passing, path: "/healthz", method: http.MethodPost, wantCode: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := platformhttp.New(discardLogger(), ":0", tt.ready)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantCode)
			}
			if got := rec.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
				t.Fatalf("X-Robots-Tag = %q, want %q on every response", got, "noindex, nofollow")
			}
		})
	}
}

func TestMountedRoutes(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", nil, platformhttp.Route{
		Pattern: "/api/v1/editorial/",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/editorial/queue", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("mounted route answered %d, want %d", rec.Code, http.StatusTeapot)
	}
	// Module routes share the platform mux, so the noindex stamping covers
	// them too - the whole API stays uncrawlable, not just the health pair.
	if got := rec.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Fatalf("X-Robots-Tag = %q, want %q on module routes", got, "noindex, nofollow")
	}
	// The health endpoints stay reachable alongside mounted routes.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz alongside mounted routes = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestServesTheEmbeddedOpenAPIDocument(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// The description of an uncrawlable API is itself uncrawlable.
	if got := rec.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("X-Robots-Tag = %q, want %q", got, "noindex, nofollow")
	}
	if !bytes.Equal(rec.Body.Bytes(), api.OpenAPIJSON()) {
		t.Error("served document differs from the embedded api/openapi.json")
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Errorf("openapi = %q, want a 3.1.x document", doc.OpenAPI)
	}
	if len(doc.Paths) == 0 {
		t.Error("served document describes no paths")
	}
}

// TestPatternsAreRegistered proves Patterns is not a list that drifted away
// from the mux: every pattern it reports is answered by a server built the
// ordinary way.
func TestPatternsAreRegistered(t *testing.T) {
	t.Parallel()
	patterns := platformhttp.Patterns()
	if len(patterns) == 0 {
		t.Fatal("Patterns() is empty")
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("pattern %q is not %q", pattern, "METHOD /path")
			}
			srv := platformhttp.New(discardLogger(), ":0", nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want %d", pattern, rec.Code, http.StatusOK)
			}
		})
	}
}

func TestProblem(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	platformhttp.Problem(rec, http.StatusConflict, "a source with this feed URL already exists")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var got platformhttp.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling problem body: %v", err)
	}
	want := platformhttp.ProblemDetails{
		Type:   "about:blank",
		Title:  "Conflict",
		Status: http.StatusConflict,
		Detail: "a source with this feed URL already exists",
	}
	if got != want {
		t.Fatalf("problem body = %+v, want %+v", got, want)
	}
}

func TestProblemOmitsEmptyDetail(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	platformhttp.Problem(rec, http.StatusInternalServerError, "")
	if strings.Contains(rec.Body.String(), "detail") {
		t.Fatalf("empty detail must be omitted, got body %q", rec.Body.String())
	}
}

func TestMountRoutesModuleHandlers(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", nil)
	srv.Mount("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/front", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("mounted route answered %d, want %d", rec.Code, http.StatusTeapot)
	}
	// Mounted routes sit behind the same defence-in-depth header as the
	// built-in ones: the API is never a crawlable surface.
	if got := rec.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Fatalf("X-Robots-Tag = %q, want %q on mounted routes", got, "noindex, nofollow")
	}

	// Health endpoints keep working alongside the mounted module.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz alongside a mounted module = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestProblemWritesRFC9457Bodies(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	platformhttp.Problem(rec, http.StatusBadRequest, "unknown place \"atlantis\"")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding problem body %q: %v", rec.Body.String(), err)
	}
	if body.Type != "about:blank" {
		t.Errorf("type = %q, want about:blank", body.Type)
	}
	if body.Title != http.StatusText(http.StatusBadRequest) {
		t.Errorf("title = %q, want the reason phrase %q", body.Title, http.StatusText(http.StatusBadRequest))
	}
	if body.Status != http.StatusBadRequest {
		t.Errorf("status field = %d, want %d", body.Status, http.StatusBadRequest)
	}
	if body.Detail != "unknown place \"atlantis\"" {
		t.Errorf("detail = %q, want the caller's detail verbatim", body.Detail)
	}
}

// brokenWriter simulates a client that vanished mid-response; the handler
// must log and carry on, never panic.
type brokenWriter struct {
	header http.Header
	code   int
}

func (w *brokenWriter) Header() http.Header        { return w.header }
func (w *brokenWriter) WriteHeader(statusCode int) { w.code = statusCode }
func (w *brokenWriter) Write([]byte) (int, error)  { return 0, errors.New("connection lost") }

func TestHealthzSurvivesFailedResponseWrite(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", nil)
	w := &brokenWriter{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.code != http.StatusOK {
		t.Fatalf("status = %d, want %d despite write failure", w.code, http.StatusOK)
	}
}

func TestRunShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), "127.0.0.1:0", nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Give the listener a moment to start, then trigger graceful shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), "not-a-valid-listen-address", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Run(ctx); err == nil {
		t.Fatal("Run() with invalid address: want error, got nil")
	}
}
