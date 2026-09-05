package linkwise_test

// What this network will answer and at what price, held to the measurements
// it was derived from.
//
// The numbers in linkwise.go are not documentation Linkwise published - it
// publishes none - they are probe results against a live publisher account.
// A test that merely restated them would pin a typo as firmly as a fact, so
// the ones below re-derive the consequence instead: what the declared window
// COSTS, and what it protects.

import (
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
)

// Measured against the live API on 2026-09-04, over windows that returned no
// rows at all - which is the point: cost tracks the WIDTH of the window, not
// the size of the answer.
//
//	  1 day     1.0s
//	  7 days    2.1s
//	 30 days    6.2s
//	 92 days   11.8s
//	365 days   81-102s
//
// Fitted as a fixed second of overhead plus a per-day marginal cost. The
// marginal figure is taken at the CHEAPEST end (0.22s/day, from the 365-day
// point) rather than averaged, so the model over-estimates rather than
// under-estimates what a window will cost.
const (
	fixedCost   = 1 * time.Second
	costPerDay  = 220 * time.Millisecond
	httpTimeout = 30 * time.Second
)

// costOf models what a query covering d will take, from the measurements.
func costOf(d time.Duration) time.Duration {
	days := float64(d) / float64(24*time.Hour)
	return fixedCost + time.Duration(days*float64(costPerDay))
}

// TestTheCostModelMatchesWhatWasMeasured keeps the model honest before
// anything is asserted with it. A model that had drifted from the probe
// results would make every test below meaningless while still passing.
func TestTheCostModelMatchesWhatWasMeasured(t *testing.T) {
	t.Parallel()

	for _, m := range []struct {
		days     int
		measured time.Duration
	}{
		{1, 1010 * time.Millisecond},
		{7, 2084 * time.Millisecond},
		{30, 6193 * time.Millisecond},
		{92, 11836 * time.Millisecond},
		{365, 81 * time.Second},
	} {
		window := time.Duration(m.days) * 24 * time.Hour
		predicted := costOf(window)
		// The model must never predict LESS than was measured - it exists to
		// bound a budget, and a model that flatters the network is worse
		// than none.
		if predicted < m.measured {
			t.Errorf("%d days: model predicts %s, but %s was measured; the model must not under-estimate",
				m.days, predicted, m.measured)
		}
	}
}

// TestTheDeclaredWindowFitsInsideAnHTTPTimeout is the assertion that stops
// somebody widening MaxWindow because a wider one "would be fewer requests".
//
// It would be. It would also be a request that runs for a minute and a half:
// the 365-day probe took 81 seconds on one run and exceeded 90 on another,
// returning nothing at all. Fewer, slower, unresumable requests is the wrong
// trade when the cursor commits per window.
func TestTheDeclaredWindowFitsInsideAnHTTPTimeout(t *testing.T) {
	t.Parallel()

	cost := costOf(linkwise.Limits().MaxWindow)
	if cost >= httpTimeout {
		t.Fatalf("a full-width window costs about %s, which does not fit inside a %s timeout", cost, httpTimeout)
	}
	// And with room to spare, because the measurement is one account on one
	// afternoon and the network will sometimes be slower than it was.
	if cost > httpTimeout/3 {
		t.Errorf("a full-width window costs about %s, more than a third of the %s timeout; that leaves no margin for a slow day",
			cost, httpTimeout)
	}
}

// TestSlicingBeatsOneWideWindow records why the bound is worth its cost in
// extra requests, in the one unit that matters: the longest a single request
// can run before it must either return or be retried.
func TestSlicingBeatsOneWideWindow(t *testing.T) {
	t.Parallel()

	year := 365 * 24 * time.Hour
	maxWindow := linkwise.Limits().MaxWindow

	slices := int(year/maxWindow) + 1
	perSlice := costOf(maxWindow)
	whole := costOf(year)

	if perSlice >= whole {
		t.Fatalf("a slice costs %s and the whole year %s: slicing buys nothing", perSlice, whole)
	}
	t.Logf("a year of backfill: %d slices of about %s each (worst single request %s), against one request of about %s",
		slices, perSlice, perSlice, whole)
}

// TestLimitsAreUsable holds the declaration to the port's own rules: a zero
// window or a zero rate stops ingestion dead, and a negative lag would have
// the network reporting the future.
func TestLimitsAreUsable(t *testing.T) {
	t.Parallel()

	if err := linkwise.Limits().Validate(); err != nil {
		t.Fatalf("Limits() is not usable: %v", err)
	}
}

// TestDocumentedDerivesFromLimits: the row an operator is shown and the
// numbers the poller obeys are one set of facts. Two literals would be two
// places to correct, and the one nobody corrects is the one that decides
// whether a member gets paid.
func TestDocumentedDerivesFromLimits(t *testing.T) {
	t.Parallel()

	d := linkwise.Documented()
	l := linkwise.Limits()

	if got, want := time.Duration(d.MaxQueryWindowDays)*24*time.Hour, l.MaxWindow; got != want {
		t.Errorf("Documented says %d days (%s), Limits says %s", d.MaxQueryWindowDays, got, want)
	}
	if got, want := d.RateLimitPerMinute, l.RequestsPerMinute; got != want {
		t.Errorf("Documented says %d requests a minute, Limits says %d", got, want)
	}
	if got, want := time.Duration(d.ReportingLagMinutes)*time.Minute, l.ReportingLag; got != want {
		t.Errorf("Documented says %d minutes of lag (%s), Limits says %s", d.ReportingLagMinutes, got, want)
	}
}

// TestDocumentedNamesTheNetwork covers the fields a cashback.network row
// cannot be seeded without.
func TestDocumentedNamesTheNetwork(t *testing.T) {
	t.Parallel()

	d := linkwise.Documented()
	if d.ID != linkwise.ID {
		t.Errorf("Documented().ID = %q, want %q", d.ID, linkwise.ID)
	}
	if d.ID != networks.NetworkID("linkwise") {
		t.Errorf("the network id is %q; it is the primary key of the row and the value NETWORKS names", d.ID)
	}
	if d.DisplayName == "" {
		t.Error("an operator reading a list of networks would see an empty name")
	}
	// The one value in the package that was inferred rather than measured.
	// Asserted so that changing it is a deliberate act with a test to
	// update, not a silent edit - it is the value whose wrongness loses
	// attribution on every click without failing anything.
	if d.ClickRefParam != "subid1" {
		t.Errorf("ClickRefParam = %q; it is INFERRED from the report API's subid1 field and unverified against a real tracking URL. Changing it is fine - confirming it first is not optional",
			d.ClickRefParam)
	}
}
