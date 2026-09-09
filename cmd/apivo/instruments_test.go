package main

// Building a list of instruments as a list (ADR-0007).

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"
)

// TestTheFirstBadNameIsTheOneReported, and the rest are still built.
//
// The point of collecting rather than returning early is that the
// declarations read as declarations. The cost would be a builder that
// forgot the failure, or that reported the last one - so the first is
// asserted, and a good name after a bad one is asserted not to clear it.
func TestTheFirstBadNameIsTheOneReported(t *testing.T) {
	t.Parallel()
	b := instruments(nil, "test")

	if got := b.counter("apivo.test.good", "d"); got == nil {
		t.Error("a well-named counter was not built")
	}
	b.gauge("9lives", "d")
	b.histogram("also bad", "d", "s")
	if got := b.counter("apivo.test.also_good", "d"); got == nil {
		t.Error("a well-named counter after a bad one was not built")
	}

	if b.err == nil {
		t.Fatal("two impossible names were accepted")
	}
	if !strings.Contains(b.err.Error(), "9lives") {
		t.Errorf("err = %v, want the FIRST bad name, 9lives", b.err)
	}
}

// TestABadNameStopsTheBinary. An instrument name this binary got wrong is a
// bug, not a deployment condition, so it must surface as a refusal to start
// rather than as a metric nobody can find. Nothing usable comes back with it:
// a half-built set handed to a caller that ignored the error would panic at
// the first measurement instead.
func TestABadNameStopsTheBinary(t *testing.T) {
	t.Parallel()
	b := &instrumentBuilder{meter: (*telemetry.Provider)(nil).Meter("test")}
	b.counter("", "d")
	if b.err == nil {
		t.Fatal("an unnamed counter was accepted")
	}
	// And a second failure does not displace the first.
	first := b.err
	b.gauge("", "d")
	if !errors.Is(b.err, first) {
		t.Error("a later failure displaced the first one reported")
	}
}
