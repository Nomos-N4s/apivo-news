package http_test

// The instrumentation seam (ADR-0007).
//
// The telemetry package proves what a span carries. This proves the server
// asks for one at all - and, more importantly, that a deployment which
// configured no telemetry is unaffected in every respect.

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// recordingInstrumentation notes every pattern it was asked to wrap.
type recordingInstrumentation struct {
	patterns []string
}

func (r *recordingInstrumentation) Middleware(pattern string, next http.Handler) http.Handler {
	r.patterns = append(r.patterns, pattern)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Instrumented", pattern)
		next.ServeHTTP(w, req)
	})
}

func (r *recordingInstrumentation) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# HELP apivo_test_total A metric.\n"))
	})
}

// TestEveryRouteIsOfferedForInstrumentation. Including the built-in ones: a
// health endpoint that is silently uninstrumented is how "the api was fine"
// and "nobody was watching the api" become the same sentence.
func TestEveryRouteIsOfferedForInstrumentation(t *testing.T) {
	t.Parallel()
	instr := &recordingInstrumentation{}
	srv := platformhttp.New(discardLogger(), ":0", "", nil, instr,
		platformhttp.Route{Pattern: "/api/v1/example/", Handler: http.NotFoundHandler()})
	srv.Mount("/api/v1/mounted/", http.NotFoundHandler())

	for _, want := range []string{"GET /healthz", "GET /readyz", "GET /metrics", "/api/v1/example/", "/api/v1/mounted/"} {
		if !slices.Contains(instr.patterns, want) {
			t.Errorf("%q was never offered for instrumentation; got %v", want, instr.patterns)
		}
	}
}

// TestTheRouteIsWrappedWithItsOwnPattern. The pattern and not the path, which
// is the whole reason Middleware takes one.
func TestTheRouteIsWrappedWithItsOwnPattern(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", "", nil, &recordingInstrumentation{},
		platformhttp.Route{Pattern: "/api/v1/example/", Handler: http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/example/anything/at/all", nil))

	if got := rec.Header().Get("X-Instrumented"); got != "/api/v1/example/" {
		t.Errorf("wrapped with %q, want the route pattern %q", got, "/api/v1/example/")
	}
}

// TestMetricsAnswer503WithoutTelemetry. Different from an empty 200 on
// purpose: a scraper reading nothing must be able to tell "this process has
// no metrics to report" from "this deployment has telemetry switched off".
func TestMetricsAnswer503WithoutTelemetry(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", "", nil, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestMetricsAreServedWithTelemetry.
func TestMetricsAreServedWithTelemetry(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", "", nil, &recordingInstrumentation{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got == "" {
		t.Error("the scrape is empty")
	}
}

// TestNoTelemetryChangesNothingElse. Every other test in this repository runs
// with instrumentation nil, so this is the case that has to keep working -
// and a pass-through that quietly swallowed a header or a status would be
// found here rather than in whichever suite broke first.
func TestNoTelemetryChangesNothingElse(t *testing.T) {
	t.Parallel()
	srv := platformhttp.New(discardLogger(), ":0", "", nil, nil,
		platformhttp.Route{Pattern: "/api/v1/example/", Handler: http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-From-Handler", "yes")
				w.WriteHeader(http.StatusTeapot)
			})})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/example/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Header().Get("X-From-Handler") != "yes" {
		t.Error("the handler's own header did not survive an uninstrumented server")
	}
	// The noindex stamping is server-wide and must not have been displaced.
	if rec.Header().Get("X-Robots-Tag") == "" {
		t.Error("X-Robots-Tag is missing; the instrumentation seam displaced the noindex wrapper")
	}
}
