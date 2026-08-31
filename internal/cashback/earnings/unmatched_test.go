package earnings_test

// What the queue write treats as nothing to do, and what it refuses to
// swallow (T067).
//
// The distinction is the whole file. Two different outcomes come back from
// the database as "no rows" and both mean the same thing to a caller, so
// they are answered alike; a failure that is NOT one of them must not join
// them, because a caller that mistook a dropped connection for "nothing to
// record" would advance past money that is in no queue at all.

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
)

// TestAMatcherIsRefusedWithoutItsParts covers the construction refusals,
// which is where they have to happen. A matcher that discovered this
// mid-window has already read reports it cannot resolve or cannot queue, and
// those are transactions nobody will look for again.
func TestAMatcherIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	if _, err := earnings.NewMatcher(&fakeClicks{}, nil); !errors.Is(err, earnings.ErrNoUnmatchedStore) {
		t.Errorf("NewMatcher(clicks, nil) error = %v, want one wrapping %v", err, earnings.ErrNoUnmatchedStore)
	}
	if _, err := earnings.NewMatcher(nil, &fakeUnmatched{}); !errors.Is(err, earnings.ErrNoClicks) {
		t.Errorf("NewMatcher(nil, store) error = %v, want one wrapping %v", err, earnings.ErrNoClicks)
	}
}

// TestAWriteThatFailedIsNotAnObservationRecorded is the case the error wrap
// exists for. A caller reading this as "nothing to do" would move a cursor
// past a report whose observation was never written.
func TestAWriteThatFailedIsNotAnObservationRecorded(t *testing.T) {
	t.Parallel()

	reportID := uuid.New()
	unmatched := &fakeUnmatched{err: errors.New("connection reset")}

	_, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")})

	if !errors.Is(err, earnings.ErrNotQueued) {
		t.Fatalf("Match() error = %v, want one wrapping %v", err, earnings.ErrNotQueued)
	}
	// The report has to be named: somebody reading a log needs to know WHICH
	// transaction is missing from the queue, not that one is.
	if !strings.Contains(err.Error(), reportID.String()) {
		t.Errorf("the error %q does not name the report %s", err, reportID)
	}
}

// TestAnObservationAlreadyRecordedIsNotAnError is the ordinary path after a
// crash: the window is re-read and every observation in it is offered again.
// Treating that as a failure would stop the poller on its own recovery.
func TestAnObservationAlreadyRecordedIsNotAnError(t *testing.T) {
	t.Parallel()

	reportID := uuid.New()
	unmatched := &fakeUnmatched{noRows: true}

	attributed, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if attributed.Matched {
		t.Error("a reference that matched nothing was reported as matched")
	}
	if attributed.Queued != uuid.Nil {
		t.Errorf("Queued = %v, want the zero uuid - no row was written this time", attributed.Queued)
	}
	if attributed.Report != reportID {
		t.Errorf("Report = %v, want %v", attributed.Report, reportID)
	}
}
