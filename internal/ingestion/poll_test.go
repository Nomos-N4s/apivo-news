package ingestion

// Unit tests for the poll loop's arithmetic - the jitter bounds and the
// freshness gate - in-package because both are deliberately private: they
// are pacing details, not API. Everything that touches a feed or the
// database is exercised in poll_integration_test.go.

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestJitteredIntervalStaysWithinTenPercent(t *testing.T) {
	t.Parallel()
	const interval = 15 * time.Minute
	lo, hi := 810*time.Second, 990*time.Second // 15m minus/plus 10%
	random := rand.Float64
	for i := 0; i < 10_000; i++ {
		got := jitteredInterval(interval, random)
		if got < lo || got > hi {
			t.Fatalf("jitteredInterval(%v) = %v, want within [%v, %v]", interval, got, lo, hi)
		}
	}
}

func TestJitteredIntervalEdges(t *testing.T) {
	t.Parallel()
	const interval = 10 * time.Minute
	tests := []struct {
		name   string
		random float64
		want   time.Duration
	}{
		// rand.Float64 spans [0, 1); the extremes map to the two bounds.
		{name: "lowest draw is minus ten percent", random: 0, want: 9 * time.Minute},
		{name: "middle draw is the interval itself", random: 0.5, want: 10 * time.Minute},
		{name: "highest draw approaches plus ten percent", random: 0.9999999, want: 11 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := jitteredInterval(interval, func() float64 { return tt.random })
			// The highest draw lands a hair under +10%; a millisecond of
			// tolerance keeps the assertion about the bound, not the hair.
			if diff := got - tt.want; diff < -time.Millisecond || diff > time.Millisecond {
				t.Errorf("jitteredInterval(%v) with draw %v = %v, want %v", interval, tt.random, got, tt.want)
			}
		})
	}
}

func TestJitteredIntervalZeroInterval(t *testing.T) {
	t.Parallel()
	if got := jitteredInterval(0, rand.Float64); got != 0 {
		t.Errorf("jitteredInterval(0) = %v, want 0", got)
	}
}

func TestPollGate(t *testing.T) {
	t.Parallel()
	// The gate is the interval less the jitter's full early swing: the
	// earliest cycle Run can produce (interval minus 10%) must still find
	// its sources due, or every early cycle would be a fleet-wide skip and
	// the real cadence would stretch toward twice the interval.
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "the default interval", interval: 15 * time.Minute, want: 810 * time.Second},
		{name: "an hour", interval: time.Hour, want: 54 * time.Minute},
		{name: "zero stays zero", interval: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pollGate(tt.interval); got != tt.want {
				t.Errorf("pollGate(%v) = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}

func TestRefreshedValidators(t *testing.T) {
	t.Parallel()
	stored := Validators{ETag: `"v1"`, LastModified: "Mon, 02 Jun 2025 09:30:00 GMT"}

	tests := []struct {
		name   string
		result Result
		want   Validators
	}{
		{
			name:   "response tokens replace the stored ones",
			result: Result{ETag: `"v2"`, LastModified: "Tue, 03 Jun 2025 09:30:00 GMT"},
			want:   Validators{ETag: `"v2"`, LastModified: "Tue, 03 Jun 2025 09:30:00 GMT"},
		},
		{
			name:   "a response stating none keeps the stored ones",
			result: Result{},
			want:   stored,
		},
		{
			name:   "each token is kept or replaced independently",
			result: Result{ETag: `"v2"`},
			want:   Validators{ETag: `"v2"`, LastModified: stored.LastModified},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := refreshedValidators(stored, tt.result); got != tt.want {
				t.Errorf("refreshedValidators() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
