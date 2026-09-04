// The window arithmetic, tested from inside the package.
//
// An internal test file, unlike the rest of this package's tests, and
// deliberately: what is being judged here is two unexported functions, and
// exporting them to test them would put the poller's private arithmetic in
// the API for the sake of the test suite. The fixture package next door
// makes the same choice for the same reason.
//
// They are worth this attention because they are pure and they decide
// everything the durable cursors mean. Every other property of the poller -
// what it writes, when the cursor moves - is asserted against a real
// database and is slow; these can be asserted exhaustively, in microseconds,
// over combinations no hand-written scenario would think to try.

package networks

import (
	"testing"
	"time"
)

var (
	pollTestNow    = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	pollTestStart  = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	pollTestMax    = 31 * 24 * time.Hour
	pollTestLag    = 100 * 24 * time.Hour
	pollTestNoTime = time.Time{}
)

func TestNextForwardWindow(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cursor    time.Time
		cursorSet bool
		now       time.Time
		// lag is the network's reporting lag. Zero in every case that
		// predates it, which is what keeps those cases meaning what they
		// meant: a network that answers up to the moment.
		lag      time.Duration
		wantOK   bool
		wantFrom time.Time
		wantTo   time.Time
	}{
		"a first poll starts where the operator said to": {
			now:      pollTestStart.Add(pollTestMax * 2),
			wantOK:   true,
			wantFrom: pollTestStart,
			wantTo:   pollTestStart.Add(pollTestMax),
		},
		"a later poll starts where the cursor stands": {
			cursor: pollTestStart.Add(pollTestMax), cursorSet: true,
			now:      pollTestNow,
			wantOK:   true,
			wantFrom: pollTestStart.Add(pollTestMax),
			wantTo:   pollTestStart.Add(2 * pollTestMax),
		},
		"a window never exceeds what the network allows": {
			cursor: pollTestStart, cursorSet: true,
			now:      pollTestStart.Add(365 * 24 * time.Hour),
			wantOK:   true,
			wantFrom: pollTestStart,
			wantTo:   pollTestStart.Add(pollTestMax),
		},
		"a window never ends in the future": {
			cursor: pollTestNow.Add(-time.Hour), cursorSet: true,
			now:      pollTestNow,
			wantOK:   true,
			wantFrom: pollTestNow.Add(-time.Hour),
			wantTo:   pollTestNow,
		},
		"a cursor that has caught up reads nothing": {
			cursor: pollTestNow, cursorSet: true,
			now:    pollTestNow,
			wantOK: false,
		},
		// Clocks move. A restored backup or a mis-set clock would otherwise
		// produce a window ending before it began, which the port refuses -
		// and the poller would report that as the adapter's fault.
		"a cursor ahead of now reads nothing rather than a backwards window": {
			cursor: pollTestNow.Add(24 * time.Hour), cursorSet: true,
			now:    pollTestNow,
			wantOK: false,
		},
		"a first poll with nothing yet to read": {
			now:    pollTestStart,
			wantOK: false,
		},

		// A network that reports late. Ending the window at now would ask
		// it about ground it has not covered; it would answer cleanly and
		// emptily, and the cursor would move past transactions nobody has
		// reported. Only the trailing sweep would return, ~100 days later.
		"a lagging network is not asked about ground it has not reported": {
			cursor: pollTestNow.Add(-6 * time.Hour), cursorSet: true,
			now:      pollTestNow,
			lag:      2 * time.Hour,
			wantOK:   true,
			wantFrom: pollTestNow.Add(-6 * time.Hour),
			wantTo:   pollTestNow.Add(-2 * time.Hour),
		},
		"the width limit still binds ahead of the lag": {
			cursor: pollTestStart, cursorSet: true,
			now:      pollTestStart.Add(365 * 24 * time.Hour),
			lag:      2 * time.Hour,
			wantOK:   true,
			wantFrom: pollTestStart,
			wantTo:   pollTestStart.Add(pollTestMax),
		},
		"a cursor inside the lag reads nothing rather than a backwards window": {
			cursor: pollTestNow.Add(-time.Hour), cursorSet: true,
			now:    pollTestNow,
			lag:    6 * time.Hour,
			wantOK: false,
		},
		"a cursor exactly at the horizon reads nothing": {
			cursor: pollTestNow.Add(-6 * time.Hour), cursorSet: true,
			now:    pollTestNow,
			lag:    6 * time.Hour,
			wantOK: false,
		},
		// The whole point of the default: an adapter that declares no lag
		// gets the behaviour it had before the field existed.
		"a zero lag is exactly the old behaviour": {
			cursor: pollTestNow.Add(-time.Hour), cursorSet: true,
			now:      pollTestNow,
			lag:      0,
			wantOK:   true,
			wantFrom: pollTestNow.Add(-time.Hour),
			wantTo:   pollTestNow,
		},
		// backfill_from inside the lag is the state ErrBackfillStartInFuture
		// exists to catch, arriving through a different door: nothing is
		// readable yet, and the operator is owed a reason rather than
		// silence. Reported by the ops surface, not by this arithmetic.
		"a first poll whose start is inside the lag reads nothing": {
			now:    pollTestStart.Add(time.Hour),
			lag:    6 * time.Hour,
			wantOK: false,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := nextForwardWindow(c.cursor, c.cursorSet, pollTestStart, c.now, pollTestMax, c.lag)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (window %s)", ok, c.wantOK, got)
			}
			if !ok {
				return
			}
			if !got.From.Equal(c.wantFrom) || !got.To.Equal(c.wantTo) {
				t.Errorf("window = %s..%s, want %s..%s", got.From, got.To, c.wantFrom, c.wantTo)
			}
		})
	}
}

func TestNextTrailingWindow(t *testing.T) {
	t.Parallel()

	// The main cursor far enough ahead that a full trailing window has aged.
	aged := pollTestStart.Add(pollTestLag + 2*pollTestMax)

	cases := map[string]struct {
		trailing    time.Time
		trailingSet bool
		cursor      time.Time
		cursorSet   bool
		wantOK      bool
		wantFrom    time.Time
		wantTo      time.Time
	}{
		"an account never polled forward re-reads nothing": {
			cursor: pollTestNoTime, cursorSet: false,
			wantOK: false,
		},
		"the first sweep starts where the operator said to": {
			cursor: aged, cursorSet: true,
			wantOK:   true,
			wantFrom: pollTestStart,
			wantTo:   pollTestStart.Add(pollTestMax),
		},
		"a later sweep starts where the trailing cursor stands": {
			trailing: pollTestStart.Add(pollTestMax), trailingSet: true,
			cursor: aged, cursorSet: true,
			wantOK:   true,
			wantFrom: pollTestStart.Add(pollTestMax),
			wantTo:   pollTestStart.Add(2 * pollTestMax),
		},
		"nothing has aged past the lag yet": {
			cursor: pollTestStart.Add(pollTestLag), cursorSet: true,
			wantOK: false,
		},
		"the sweep stops at the horizon rather than at a whole window": {
			cursor: pollTestStart.Add(pollTestLag + time.Hour), cursorSet: true,
			wantOK:   true,
			wantFrom: pollTestStart,
			wantTo:   pollTestStart.Add(time.Hour),
		},
		"a sweep that has caught up with the horizon reads nothing": {
			trailing: pollTestStart.Add(pollTestMax), trailingSet: true,
			cursor: pollTestStart.Add(pollTestMax + pollTestLag), cursorSet: true,
			wantOK: false,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := nextTrailingWindow(c.trailing, c.trailingSet, pollTestStart, c.cursor, c.cursorSet, pollTestLag, pollTestMax)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (window %s)", ok, c.wantOK, got)
			}
			if !ok {
				return
			}
			if !got.From.Equal(c.wantFrom) || !got.To.Equal(c.wantTo) {
				t.Errorf("window = %s..%s, want %s..%s", got.From, got.To, c.wantFrom, c.wantTo)
			}
		})
	}
}

// TestWindowArithmeticHoldsItsInvariants sweeps a grid rather than naming
// scenarios, because the properties below must hold for EVERY position the
// two cursors can be in - including the ones nobody would think to write
// down. Each is a rule something downstream depends on:
//
//   - a window the port would refuse is one the poller never gets to send;
//   - a forward window ending in the future would carry the cursor over
//     transactions that arrive afterwards;
//   - a trailing window reaching the main cursor would break the schema's
//     own check and, before that, would re-read ground the forward sweep has
//     not finished with.
func TestWindowArithmeticHoldsItsInvariants(t *testing.T) {
	t.Parallel()

	limits := Limits{MaxWindow: pollTestMax, RequestsPerMinute: 360}
	if err := limits.Validate(); err != nil {
		t.Fatalf("the limits this grid is built on are not usable: %v", err)
	}

	offsets := []time.Duration{
		-400 * 24 * time.Hour, -pollTestLag, -pollTestMax, -time.Hour, 0,
		time.Hour, pollTestMax, pollTestLag, pollTestLag + pollTestMax, 400 * 24 * time.Hour,
	}

	forward, trailing := 0, 0
	for _, cursorAt := range offsets {
		for _, set := range []bool{false, true} {
			cursor := pollTestStart.Add(cursorAt)

			if w, ok := nextForwardWindow(cursor, set, pollTestStart, pollTestNow, pollTestMax, 0); ok {
				forward++
				if err := limits.ValidateWindow(w); err != nil {
					t.Errorf("forward window %s (cursor %s, set %v) is one the port refuses: %v", w, cursor, set, err)
				}
				if w.To.After(pollTestNow) {
					t.Errorf("forward window %s ends after now (%s); the cursor would advance over transactions that have not arrived", w, pollTestNow)
				}
			}

			for _, trailingAt := range offsets {
				for _, tset := range []bool{false, true} {
					tcur := pollTestStart.Add(trailingAt)
					w, ok := nextTrailingWindow(tcur, tset, pollTestStart, cursor, set, pollTestLag, pollTestMax)
					if !ok {
						continue
					}
					trailing++
					if err := limits.ValidateWindow(w); err != nil {
						t.Errorf("trailing window %s is one the port refuses: %v", w, err)
					}
					if !set {
						t.Errorf("trailing window %s was offered for an account never polled forward", w)
					}
					if !w.To.Before(cursor) {
						t.Errorf("trailing window %s reaches the main cursor %s; the schema refuses a trailing cursor that is not behind it", w, cursor)
					}
					if w.To.After(cursor.Add(-pollTestLag)) {
						t.Errorf("trailing window %s passes the %s horizon; it would re-read ground the network has not had time to change its mind about", w, pollTestLag)
					}
				}
			}
		}
	}

	// A grid that offered no windows at all would assert nothing.
	if forward == 0 || trailing == 0 {
		t.Fatalf("the grid produced %d forward and %d trailing window(s); with none of either it proves nothing", forward, trailing)
	}
	t.Logf("invariants held over %d forward and %d trailing windows", forward, trailing)
}
