package main

// What the scheduler's runs look like in a scrape (ADR-0007).
//
// Asserted against the real Prometheus exposition rather than against a stub
// counter, because the thing that has to be true is that an operator reading
// /metrics can answer "did the settlement sweep run", and every layer between
// the scheduler and that page is part of the answer.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
	"github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"
)

// observedProvider builds a provider with telemetry on, pointed at a collector
// that does not exist. Nothing here pushes; the scrape is a pull, and it must
// work whether or not the push half can reach anything.
func observedProvider(t *testing.T) *telemetry.Provider {
	t.Helper()
	p, err := telemetry.New(context.Background(), slog.New(slog.DiscardHandler), telemetry.Config{
		Endpoint: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

// scrape reads the provider's metrics endpoint.
func scrape(t *testing.T, p *telemetry.Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scraping: status %d", rec.Code)
	}
	return rec.Body.String()
}

// TestARunIsCountedAndTimed.
func TestARunIsCountedAndTimed(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	observer, err := newJobObserver(p)
	if err != nil {
		t.Fatalf("building the observer: %v", err)
	}

	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "ledger-zero-sum", Outcome: scheduler.Ran, Took: 250 * time.Millisecond,
	})

	body := scrape(t, p)
	for _, want := range []string{
		"apivo_scheduler_job_attempts",
		"apivo_scheduler_job_duration",
		`job="ledger-zero-sum"`,
		`outcome="ran"`,
		`result="ok"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not carry %q:\n%s", want, body)
		}
	}
}

// TestASkipIsCountedAndNotTimed. A skipped attempt took the time it takes to
// lose a race for a lock. Recorded on the duration histogram, that wait would
// be indistinguishable from work done - and with more than one instance,
// skips are the majority of attempts, so it would be most of the histogram.
func TestASkipIsCountedAndNotTimed(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	observer, err := newJobObserver(p)
	if err != nil {
		t.Fatalf("building the observer: %v", err)
	}

	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "network-poll:linkwise:1", Outcome: scheduler.Skipped,
	})

	body := scrape(t, p)
	if !strings.Contains(body, `outcome="skipped"`) {
		t.Errorf("the skip was not counted:\n%s", body)
	}
	if strings.Contains(body, "apivo_scheduler_job_duration") {
		t.Errorf("a skipped attempt was timed:\n%s", body)
	}
}

// TestAFailureAndAPanicAreDifferentResults. Both arrive as an error and they
// are not the same news: a failure is a condition the job reported, a panic is
// a bug in it.
func TestAFailureAndAPanicAreDifferentResults(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	observer, err := newJobObserver(p)
	if err != nil {
		t.Fatalf("building the observer: %v", err)
	}

	boom := errors.New("it went wrong")
	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "cashback-earnings-lifecycle", Outcome: scheduler.Ran, Took: time.Second, Err: boom,
	})
	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "cashback-earnings-lifecycle", Outcome: scheduler.Ran, Took: time.Second,
		Err: boom, Panicked: true,
	})

	body := scrape(t, p)
	if !strings.Contains(body, `result="failed"`) {
		t.Errorf("a failed run is not labelled failed:\n%s", body)
	}
	if !strings.Contains(body, `result="panicked"`) {
		t.Errorf("a panicking run is not labelled panicked:\n%s", body)
	}
}

// TestTheObserverWorksWithTelemetryOff. This is how every environment runs
// today and how every other test in this package runs, so it is the case that
// must not need a guard at the call site.
func TestTheObserverWorksWithTelemetryOff(t *testing.T) {
	t.Parallel()
	p, err := telemetry.New(context.Background(), slog.New(slog.DiscardHandler), telemetry.Config{})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	observer, err := newJobObserver(p)
	if err != nil {
		t.Fatalf("building the observer with telemetry off: %v", err)
	}
	// The assertion is that none of this panics or blocks.
	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "sweep", Outcome: scheduler.Ran, Took: time.Second,
	})
	observer.JobAttempted(context.Background(), scheduler.Attempt{
		Job: "sweep", Outcome: scheduler.LockFailed, Err: errors.New("no lock"),
	})

	// And the seam it plugs into accepts it, which is the whole point of
	// building one even when there is nothing to report to.
	var _ scheduler.Observer = observer
}
