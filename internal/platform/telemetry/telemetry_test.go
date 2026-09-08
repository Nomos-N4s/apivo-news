package telemetry

// The provider, and Invariant 2 (ADR-0007).
//
// Invariant 2 says telemetry is never on the request path. That is a claim
// about what happens when the collector is NOT there, so every case that
// matters here points the exporter at something that accepts a connection and
// then says nothing - which is the worst case, and the one a health check
// would not catch.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// discard is a logger that keeps the test output readable.
func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// blackHole answers an endpoint that accepts TCP connections and never
// replies. A closed port would be the easy case - it fails fast. This is the
// one that hangs.
func blackHole(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Held open, never written to, never closed until teardown.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return "http://" + listener.Addr().String()
}

// TestAnUnconfiguredDeploymentStartsAndServes. Off is a supported state, not
// a misconfiguration - the same stance the vault takes. Every existing test
// in this repository runs with telemetry unconfigured, so this is also the
// case that keeps them green.
func TestAnUnconfiguredDeploymentStartsAndServes(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{})
	if err != nil {
		t.Fatalf("an unconfigured deployment must start: %v", err)
	}
	if p.Enabled() {
		t.Error("a provider with no endpoint reports itself enabled")
	}
	if p.TracerProvider() == nil {
		t.Error("TracerProvider is nil, so every caller would have to guard a call")
	}
	// The middleware must be a pass-through rather than a wrapper that does
	// nothing, so an unconfigured deployment pays no cost at all.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	rec := httptest.NewRecorder()
	p.Middleware("/x", handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 204 {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("shutting down an unconfigured provider: %v", err)
	}
}

// TestAnEndpointThatIsNotAnAddressRefusesToStart. Set and unusable is an
// error, on the vault's reasoning: a deployment that named a collector meant
// it, and starting anyway would emit nothing while somebody believed it did.
func TestAnEndpointThatIsNotAnAddressRefusesToStart(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"alloy:4318", "not a url", "ftp://alloy:4318", "://"} {
		if _, err := New(context.Background(), discard(), Config{Endpoint: endpoint}); err == nil {
			t.Errorf("Endpoint %q was accepted; it is not an absolute http or https URL", endpoint)
		}
	}
}

// TestTheNilProviderIsSafe. Nothing should have to check, including code
// paths that never built one.
func TestTheNilProviderIsSafe(t *testing.T) {
	t.Parallel()
	var p *Provider
	if p.Enabled() {
		t.Error("a nil provider reports itself enabled")
	}
	if p.TracerProvider() == nil {
		t.Error("a nil provider answers a nil tracer provider")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("shutting down a nil provider: %v", err)
	}
}

// TestACollectorThatNeverAnswersDoesNotBlockTheCaller. Invariant 2, stated as
// a test rather than as a comment.
//
// The exporter is pointed at a socket that accepts and then says nothing, and
// far more spans are ended than the queue can hold, so the drop path is the
// one under test rather than the happy path. If the batch processor were
// built with the blocking option this would sit here until the deadline.
func TestACollectorThatNeverAnswersDoesNotBlockTheCaller(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{
		Endpoint: blackHole(t), ServiceName: "test", Environment: "test",
	})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	tracer := p.TracerProvider().Tracer("test")
	// Three times the queue, so the queue is certainly full and certainly
	// dropping well before the last span.
	const spans = queueSize * 3
	start := time.Now()
	for range spans {
		_, span := tracer.Start(context.Background(), "op")
		span.SetAttributes(attribute.String("http.route", "/x"))
		span.End()
	}
	elapsed := time.Since(start)

	// Generous by two orders of magnitude against the failure it is looking
	// for: a blocking processor would wait exportTimeout per batch, so this
	// would be tens of seconds rather than milliseconds.
	if elapsed > 5*time.Second {
		t.Fatalf("ending %d spans against a dead collector took %v; telemetry is on the request path", spans, elapsed)
	}
}

// TestShutdownIsBoundedAgainstADeadCollector. A process must be able to stop.
// Without the bound, the flush at exit waits on the same socket that was
// never going to answer, and a deploy hangs on the container it is replacing.
func TestShutdownIsBoundedAgainstADeadCollector(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{Endpoint: blackHole(t)})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	tracer := p.TracerProvider().Tracer("test")
	_, span := tracer.Start(context.Background(), "op")
	span.End()

	start := time.Now()
	_ = p.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > shutdownTimeout+3*time.Second {
		t.Fatalf("shutdown took %v against a dead collector; it is bounded by %v", elapsed, shutdownTimeout)
	}
}

// TestMetricsAreServedForScraping. Metrics are pulled, not pushed, so this
// endpoint IS the metrics pipeline - and it must answer without the collector
// being reachable at all, which is why it is tested against the black hole.
func TestMetricsAreServedForScraping(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{Endpoint: blackHole(t)})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "# HELP") && rec.Body.Len() != 0 {
		t.Errorf("the body is not Prometheus exposition: %q", rec.Body.String())
	}
}

// TestMetricsSayNoRatherThanNothingWhenUnconfigured. An empty 200 reads as a
// process with nothing to report, which is the one answer a scraper must not
// be given by a deployment that simply has telemetry switched off.
func TestMetricsSayNoRatherThanNothingWhenUnconfigured(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestTheRouteNamesTheSpanAndThePathDoesNot. A span name that varies per
// request makes every trace a unique operation and every metric label
// unbounded - the most expensive mistake available in this package, and the
// easiest to make by handing the middleware r.URL.Path.
//
// Built from an in-memory exporter rather than through New, because this
// asserts what was RECORDED and the black hole records nothing readable.
func TestTheRouteNamesTheSpanAndThePathDoesNot(t *testing.T) {
	t.Parallel()
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(scrubbingExporter{inner: sink}))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	p := &Provider{enabled: true, tracer: tp}

	const route = "/api/v1/cashback/merchants/{slug}"
	handler := p.Middleware(route, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// A path carrying a value that must never become a span name.
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/cashback/merchants/fixture-outdoor-co", nil))

	spans := sink.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	// `{method} {route}` is OpenTelemetry's convention for a server span.
	if want := http.MethodGet + " " + route; spans[0].Name != want {
		t.Errorf("span name = %q, want %q", spans[0].Name, want)
	}
	if strings.Contains(spans[0].Name, "fixture-outdoor-co") {
		t.Error("the span is named after the request path; every request would be its own operation")
	}
}
