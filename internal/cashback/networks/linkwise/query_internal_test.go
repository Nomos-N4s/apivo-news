package linkwise

// Rendering a [networks.QueryWindow] into the four parameters Linkwise's
// custom length takes (T243).
//
// An internal test, because what is being pinned is a private rendering and
// the two ways it can be wrong are both silent: a month-first date is a
// different window the API answers perfectly happily, and an off-by-one at
// the upper bound double-counts or drops a transaction at every seam of a
// backfill. Neither produces an error anywhere.

import (
	"net/url"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// athens is the zone Linkwise's own timestamps arrive in, and the one a
// window built from a Greek merchant's day is most likely to carry.
var athens = time.FixedZone("EEST", 3*60*60)

// TestTheWindowIsRenderedDayFirst is the transposition case.
//
// The usage text says "Its value is of the format 31/12/2010" - day first -
// and 02/01/2006 differs from 01/02/2006 by one transposition that no test
// catches unless it uses a date where the two disagree. The third of
// September is exactly that date: day-first it is 03/09, month-first it is
// 09/03, and both are dates Linkwise will answer.
func TestTheWindowIsRenderedDayFirst(t *testing.T) {
	t.Parallel()

	query, err := windowQuery(networks.QueryWindow{
		From: time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.September, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("windowQuery(): %v", err)
	}
	for _, want := range []struct{ key, value string }{
		{"length", "custom"},
		{"from", "03/09/2026"},
		{"from_time", "00:00:00"},
		{"to", "09/09/2026"},
		{"to_time", "23:59:59"},
	} {
		if got := query.Get(want.key); got != want.value {
			t.Errorf("%s = %q, want %q", want.key, got, want.value)
		}
	}
}

// TestTheUpperBoundIsMovedBackByOneSecond. A QueryWindow is half-open -
// From <= t < To - and Linkwise's to/to_time is inclusive. Adjacent windows
// must therefore meet exactly: one ending at midnight and the next beginning
// at midnight must not both ask about the same second.
func TestTheUpperBoundIsMovedBackByOneSecond(t *testing.T) {
	t.Parallel()

	midnight := time.Date(2026, time.September, 10, 0, 0, 0, 0, time.UTC)
	first, err := windowQuery(networks.QueryWindow{From: midnight.AddDate(0, 0, -7), To: midnight})
	if err != nil {
		t.Fatalf("windowQuery(): %v", err)
	}
	second, err := windowQuery(networks.QueryWindow{From: midnight, To: midnight.AddDate(0, 0, 7)})
	if err != nil {
		t.Fatalf("windowQuery(): %v", err)
	}

	if got, want := first.Get("to")+" "+first.Get("to_time"), "09/09/2026 23:59:59"; got != want {
		t.Errorf("the first window ends at %q, want %q: an inclusive bound at midnight would ask about the next window's first second", got, want)
	}
	if got, want := second.Get("from")+" "+second.Get("from_time"), "10/09/2026 00:00:00"; got != want {
		t.Errorf("the second window starts at %q, want %q", got, want)
	}
}

// TestTheWindowIsRenderedInUTC. The window is a pair of instants; the zone
// they were built in is not part of the question. A window built from an
// Athens midnight covers the UTC evening before, and Linkwise is asked about
// that - which is also why the request carries timezone=UTC.
func TestTheWindowIsRenderedInUTC(t *testing.T) {
	t.Parallel()

	query, err := windowQuery(networks.QueryWindow{
		From: time.Date(2026, time.September, 4, 1, 30, 0, 0, athens),
		To:   time.Date(2026, time.September, 5, 1, 30, 0, 0, athens),
	})
	if err != nil {
		t.Fatalf("windowQuery(): %v", err)
	}
	// 01:30 in +03:00 is 22:30 the previous day in UTC.
	if got, want := query.Get("from")+" "+query.Get("from_time"), "03/09/2026 22:30:00"; got != want {
		t.Errorf("the window starts at %q, want %q", got, want)
	}
	if got, want := query.Get("to")+" "+query.Get("to_time"), "04/09/2026 22:29:59"; got != want {
		t.Errorf("the window ends at %q, want %q", got, want)
	}
}

// TestAWindowTooNarrowToExpressIsRefused, not widened.
//
// Linkwise's finest bound is a second. A window narrower than that cannot be
// asked for without covering more ground than the caller asked for, and
// silently asking for more is how a transaction is ingested twice.
func TestAWindowTooNarrowToExpressIsRefused(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		window networks.QueryWindow
	}{
		{name: "empty", window: networks.QueryWindow{From: at, To: at}},
		{name: "half a second", window: networks.QueryWindow{From: at, To: at.Add(500 * time.Millisecond)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := windowQuery(tt.window); err == nil {
				t.Fatalf("windowQuery(%s) was accepted; it cannot be expressed without widening it", tt.window)
			}
		})
	}
}

// TestExactlyOneSecondIsExpressible is the boundary on the other side of
// that refusal: the narrowest window this API can be asked for is one
// second, and it must not be refused.
func TestExactlyOneSecondIsExpressible(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	query, err := windowQuery(networks.QueryWindow{From: at, To: at.Add(time.Second)})
	if err != nil {
		t.Fatalf("windowQuery(): %v", err)
	}
	if got, want := query.Get("from_time"), query.Get("to_time"); got != want {
		t.Errorf("a one-second window runs from %q to %q; both bounds name the same second", got, want)
	}
}

// TestAnUnusableWindowIsRefusedBeforeAnythingIsRendered.
func TestAnUnusableWindowIsRefusedBeforeAnythingIsRendered(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		window networks.QueryWindow
	}{
		{name: "no bounds at all", window: networks.QueryWindow{}},
		{name: "no lower bound", window: networks.QueryWindow{To: at}},
		{name: "no upper bound", window: networks.QueryWindow{From: at}},
		{name: "ends before it starts", window: networks.QueryWindow{From: at, To: at.Add(-time.Hour)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := windowQuery(tt.window); err == nil {
				t.Fatalf("windowQuery(%s) was accepted", tt.window)
			}
		})
	}
}

// TestTheGlobalOptionsAreAddedRatherThanSubstituted covers the copy: Get
// takes the caller's values and returns its own, so a caller reusing one
// url.Values across two windows never inherits the first one's additions.
func TestTheGlobalOptionsAreAddedRatherThanSubstituted(t *testing.T) {
	t.Parallel()

	mine := url.Values{"fields": {"transaction_id,amount"}}
	full := withMachineReadableAnswers(mine)

	if got := full.Get("fields"); got != "transaction_id,amount" {
		t.Errorf("fields = %q, want the caller's own value", got)
	}
	if got := full.Get("format"); got != "json" {
		t.Errorf("format = %q, want json", got)
	}
	if _, ok := mine["format"]; ok {
		t.Error("the caller's own values were written to")
	}
}
