// The tests for window.go: half-open membership, both bounds required, and a
// window judged against the limits before its width is measured. It holds the
// anchor instant and Awin's documented limits, which the later files build
// their own fixtures from.

package networks_test

import (
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// portTestAnchor is a fixed instant. Windows and transaction times are built
// from it so that a failure prints the same dates every run and a test can
// never straddle midnight.
var portTestAnchor = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// portTestLimits are Awin's documented numbers (ADR-0003): a 31-day maximum
// query window and 6 requests per second. They are the reference case the
// contract's window rule is written against.
var portTestLimits = networks.Limits{
	MaxWindow:         31 * 24 * time.Hour,
	RequestsPerMinute: 360,
}

// portTestWindow is a window well inside Awin's 31-day maximum.
func portTestWindow() networks.QueryWindow {
	return networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(7 * 24 * time.Hour)}
}

func TestQueryWindowValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		window  networks.QueryWindow
		wantErr error
	}{
		{name: "a week", window: portTestWindow()},
		{
			name:   "an empty but ordered window",
			window: networks.QueryWindow{From: portTestAnchor, To: portTestAnchor},
		},
		{
			name:    "no lower bound",
			window:  networks.QueryWindow{To: portTestAnchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
		{
			name:    "no upper bound",
			window:  networks.QueryWindow{From: portTestAnchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
		{
			name:    "the zero window, which is what a forgotten argument carries",
			window:  networks.QueryWindow{},
			wantErr: networks.ErrInvalidQueryWindow,
		},
		{
			name:    "swapped bounds",
			window:  networks.QueryWindow{From: portTestAnchor.Add(time.Hour), To: portTestAnchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			portTestAssert(t, "QueryWindow.Validate()", tc.window.Validate(), tc.wantErr, nil)
		})
	}
}

// TestQueryWindowContainsIsHalfOpen pins the membership rule adjacent windows
// partition a backfill by: the lower bound is in and the upper bound is out,
// so no transaction is counted twice and none falls in a seam (FR-031).
func TestQueryWindowContainsIsHalfOpen(t *testing.T) {
	t.Parallel()

	first := networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(24 * time.Hour)}
	second := networks.QueryWindow{From: first.To, To: first.To.Add(24 * time.Hour)}

	for _, at := range []time.Time{
		first.From,
		first.From.Add(time.Nanosecond),
		first.To.Add(-time.Nanosecond),
		first.To,
		second.To.Add(-time.Nanosecond),
		first.From.Add(-time.Nanosecond),
	} {
		in, out := first.Contains(at), second.Contains(at)
		if in && out {
			t.Errorf("%s falls in both adjacent windows", at.Format(time.RFC3339Nano))
		}
	}
	if !first.Contains(first.From) {
		t.Errorf("QueryWindow.Contains(From) = false, want true: the lower bound is inclusive")
	}
	if first.Contains(first.To) {
		t.Errorf("QueryWindow.Contains(To) = true, want false: the upper bound is exclusive")
	}
	if !second.Contains(first.To) {
		t.Errorf("the instant one window ends at must belong to the next, or a backfill has a seam")
	}
}

func TestLimitsValidateWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		limits  networks.Limits
		window  networks.QueryWindow
		wantErr error
		wantIn  []string
	}{
		{
			name:   "a week against Awin's 31 days",
			limits: portTestLimits,
			window: portTestWindow(),
		},
		{
			name:   "exactly the documented maximum",
			limits: portTestLimits,
			window: networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(portTestLimits.MaxWindow)},
		},
		{
			name:    "one nanosecond over the documented maximum",
			limits:  portTestLimits,
			window:  networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(portTestLimits.MaxWindow + time.Nanosecond)},
			wantErr: networks.ErrWindowTooWide,
			wantIn:  []string{"744h0m0s"},
		},
		{
			name:    "a 90-day backfill asked for in one go",
			limits:  portTestLimits,
			window:  networks.QueryWindow{From: portTestAnchor, To: portTestAnchor.Add(90 * 24 * time.Hour)},
			wantErr: networks.ErrWindowTooWide,
		},
		{
			name:    "unset limits, which a forgotten declaration carries",
			limits:  networks.Limits{},
			window:  portTestWindow(),
			wantErr: networks.ErrInvalidLimits,
		},
		{
			name:    "a network declaring no maximum window, which would refuse every window as too wide",
			limits:  networks.Limits{RequestsPerMinute: 360},
			window:  portTestWindow(),
			wantErr: networks.ErrInvalidLimits,
			wantIn:  []string{"maximum query window"},
		},
		{
			name:    "a network declaring no request rate",
			limits:  networks.Limits{MaxWindow: 31 * 24 * time.Hour},
			window:  portTestWindow(),
			wantErr: networks.ErrInvalidLimits,
			wantIn:  []string{"request rate"},
		},
		{
			name:    "a negative maximum window",
			limits:  networks.Limits{MaxWindow: -time.Hour, RequestsPerMinute: 360},
			window:  portTestWindow(),
			wantErr: networks.ErrInvalidLimits,
		},
		{
			name:    "a negative request rate, which no limiter can be built from",
			limits:  networks.Limits{MaxWindow: 31 * 24 * time.Hour, RequestsPerMinute: -360},
			window:  portTestWindow(),
			wantErr: networks.ErrInvalidLimits,
			wantIn:  []string{"request rate"},
		},
		{
			name:    "the limits are judged before the window",
			limits:  networks.Limits{},
			window:  networks.QueryWindow{},
			wantErr: networks.ErrInvalidLimits,
		},
		{
			name:    "an unbounded window cannot be clamped",
			limits:  portTestLimits,
			window:  networks.QueryWindow{From: portTestAnchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
		{
			name:    "swapped bounds are refused before their width is measured",
			limits:  portTestLimits,
			window:  networks.QueryWindow{From: portTestAnchor.Add(time.Hour), To: portTestAnchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			portTestAssert(t, "Limits.ValidateWindow()", tc.limits.ValidateWindow(tc.window), tc.wantErr, tc.wantIn)
		})
	}
}
