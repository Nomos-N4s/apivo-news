package http_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
