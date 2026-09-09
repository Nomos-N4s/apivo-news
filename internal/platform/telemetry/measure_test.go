package telemetry

// The measurement vocabulary (ADR-0007).
//
// Two things have to hold and neither is obvious from reading the code. A
// deployment with telemetry off must be able to build and record instruments
// exactly as one with telemetry on does - otherwise every call site grows a
// guard, and the guarded path is the one that never runs in a test. And a
// number recorded here must actually reach the scrape, because an instrument
// that records into nothing is worse than no instrument: it reads as evidence.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInstrumentsWorkWithTelemetryOff. Every other test in this repository
// runs in exactly this configuration, so this is the case that has to keep
// working - and a nil provider is included because code paths exist that never
// built one.
func TestInstrumentsWorkWithTelemetryOff(t *testing.T) {
	t.Parallel()
	off, err := New(context.Background(), discard(), Config{})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	var never *Provider

	for name, p := range map[string]*Provider{"unconfigured": off, "nil": never} {
		t.Run(name, func(t *testing.T) {
			m := p.Meter("test")
			if m == nil {
				t.Fatal("Meter answered nil")
			}
			count, err := m.Counter("apivo.test.things", "things", "")
			if err != nil {
				t.Fatalf("building a counter: %v", err)
			}
			level, err := m.Gauge("apivo.test.level", "a level", "")
			if err != nil {
				t.Fatalf("building a gauge: %v", err)
			}
			took, err := m.Histogram("apivo.test.duration", "how long", "s")
			if err != nil {
				t.Fatalf("building a histogram: %v", err)
			}
			// The point of the exercise: recording must be safe, not merely
			// buildable.
			count.Add(context.Background(), 1, Label("kind", "x"))
			level.Record(context.Background(), 7)
			took.Record(context.Background(), 0.25, Label("job", "y"))
		})
	}
}

// TestARecordedCountReachesTheScrape, with its label. This is the end to end
// one: the instrument, the meter provider, the Prometheus reader and the
// handler, against a collector that is not there - because the scrape is a
// pull and must not depend on the push half working.
func TestARecordedCountReachesTheScrape(t *testing.T) {
	t.Parallel()
	p, err := New(context.Background(), discard(), Config{Endpoint: blackHole(t)})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	widgets, err := p.Meter("test").Counter("apivo.test.widgets", "widgets seen", "")
	if err != nil {
		t.Fatalf("building a counter: %v", err)
	}
	widgets.Add(context.Background(), 3, Label("colour", "green"))

	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The Prometheus exporter renames dots to underscores and suffixes a
	// monotonic counter with _total, so the assertion is on the stem rather
	// than on a spelling this package does not choose.
	if !strings.Contains(body, "apivo_test_widgets") {
		t.Fatalf("the counter is not in the scrape:\n%s", body)
	}
	if !strings.Contains(body, `colour="green"`) {
		t.Errorf("the label is not in the scrape:\n%s", body)
	}
	if !strings.Contains(body, "} 3") {
		t.Errorf("the value 3 is not in the scrape:\n%s", body)
	}
}

// TestABadInstrumentNameIsRefusedWhetherOrNotTelemetryIsOn.
//
// The second half is the point. OpenTelemetry's NO-OP meter accepts any name -
// it has nothing to register one against - so before validName existed, an
// instrument named by a copy-paste built cleanly in every test in this
// repository, all of which run with telemetry off, and would have failed for
// the first time on the first deployment that had it on. Asserting only the
// enabled case would leave exactly that gap open.
func TestABadInstrumentNameIsRefusedWhetherOrNotTelemetryIsOn(t *testing.T) {
	t.Parallel()
	on, err := New(context.Background(), discard(), Config{Endpoint: blackHole(t)})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	t.Cleanup(func() { _ = on.Shutdown(context.Background()) })
	off, err := New(context.Background(), discard(), Config{})
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	for state, p := range map[string]*Provider{"on": on, "off": off} {
		t.Run(state, func(t *testing.T) {
			m := p.Meter("test")
			for _, name := range []string{"", "9lives", "spaces are not allowed", "has:a:colon"} {
				if _, err := m.Counter(name, "d", ""); err == nil {
					t.Errorf("a counter named %q was accepted", name)
				}
				if _, err := m.Gauge(name, "d", ""); err == nil {
					t.Errorf("a gauge named %q was accepted", name)
				}
				if _, err := m.Histogram(name, "d", "s"); err == nil {
					t.Errorf("a histogram named %q was accepted", name)
				}
			}
			// And the shape this repository actually uses is not caught by it.
			if _, err := m.Counter("apivo.scheduler.job.runs", "d", ""); err != nil {
				t.Errorf("a well-formed name was refused: %v", err)
			}
		})
	}
}

// TestNoLabelsIsNotOneEmptyLabel. keyValues short-circuits on the empty case,
// and a wrong short-circuit that returned a one-element slice would produce a
// series labelled ="" that nothing would notice until a dashboard grouped by
// it.
func TestNoLabelsIsNotOneEmptyLabel(t *testing.T) {
	t.Parallel()
	if got := keyValues(nil); got != nil {
		t.Errorf("keyValues(nil) = %v, want nil", got)
	}
	got := keyValues([]Attr{Label("a", "1"), Label("b", "2")})
	if len(got) != 2 {
		t.Fatalf("keyValues gave %d attributes, want 2", len(got))
	}
	if string(got[0].Key) != "a" || got[0].Value.AsString() != "1" {
		t.Errorf("first attribute = %v, want a=1", got[0])
	}
}

// TestAnInstrumentNameHasALengthLimit. 255 is OpenTelemetry's, and a name
// built by concatenating something unbounded - a job name, a network id - is
// how a limit like this gets found in production rather than here.
func TestAnInstrumentNameHasALengthLimit(t *testing.T) {
	t.Parallel()
	m := (*Provider)(nil).Meter("test")
	longest := "a" + strings.Repeat("b", maxNameLen-1)
	if _, err := m.Counter(longest, "d", ""); err != nil {
		t.Errorf("a name of exactly %d characters was refused: %v", maxNameLen, err)
	}
	if _, err := m.Counter(longest+"c", "d", ""); err == nil {
		t.Errorf("a name of %d characters was accepted", maxNameLen+1)
	}
}
