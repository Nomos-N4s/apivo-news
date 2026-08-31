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
	"time"

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
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")})

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
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")})
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

// TestAQueuedReportIsAnnounced. Money nobody can be credited for is an
// operator's queue, and a queue nothing announces is one somebody has to
// know to go and look at (FR-034).
func TestAQueuedReportIsAnnounced(t *testing.T) {
	t.Parallel()

	reportID := uuid.New()
	unmatched := &fakeUnmatched{}
	out := &fakeOutbox{}

	if _, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), out, earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")}); err != nil {
		t.Fatalf("Match(): %v", err)
	}

	announced := out.only(t, earnings.TypeTransactionUnattributed)
	if announced.Subject != reportID.String() {
		t.Errorf("the event is about %q, want the report %s", announced.Subject, reportID)
	}
	if announced.Payload["network_transaction_id"] != reportID.String() {
		t.Errorf("the payload names report %v, want %s", announced.Payload["network_transaction_id"], reportID)
	}
	// The instant the QUEUE ROW carries, so what a consumer is told and what
	// an operator sees name one moment.
	if announced.Payload["at"] != detectedAt.Format(time.RFC3339Nano) {
		t.Errorf("the payload says %v, want the row's detection instant %s", announced.Payload["at"], detectedAt)
	}
}

// TestAnObservationAlreadyRecordedAnnouncesNothing. A window re-read after a
// crash resolves the same references again and writes nothing; announcing
// anyway would republish one report's misfortune on every sweep forever.
func TestAnObservationAlreadyRecordedAnnouncesNothing(t *testing.T) {
	t.Parallel()

	out := &fakeOutbox{}
	unmatched := &fakeUnmatched{noRows: true}

	if _, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), out, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-nothing-answers-to")}); err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if got := out.of(t, earnings.TypeTransactionUnattributed); len(got) != 0 {
		t.Errorf("announced %d event(s) for an observation already recorded, want none", len(got))
	}
}

// TestAMatchedReportAnnouncesNothing. The queue is for money nobody can be
// credited for, and a credited purchase is not that.
func TestAMatchedReportAnnouncesNothing(t *testing.T) {
	t.Parallel()

	out := &fakeOutbox{}
	unmatched := &fakeUnmatched{}

	if _, err := matcherOver(t, &fakeClicks{}, unmatched).
		Match(t.Context(), out, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-that-names-a-click")}); err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if len(out.events) != 0 {
		t.Errorf("a matched report announced %d event(s), want none: %+v", len(out.events), out.events)
	}
}

// TestAQueuedReportThatCannotBeAnnouncedFails, for the reason every other
// announcement failure is fatal: the append shares the caller's transaction,
// so carrying on would commit nothing while reporting success.
func TestAQueuedReportThatCannotBeAnnouncedFails(t *testing.T) {
	t.Parallel()

	out := &fakeOutbox{err: errOutboxRefused}

	_, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, &fakeUnmatched{}).
		Match(t.Context(), out, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-nothing-answers-to")})

	if !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Fatalf("Match() error = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
}
