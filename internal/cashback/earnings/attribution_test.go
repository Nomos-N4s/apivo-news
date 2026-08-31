package earnings_test

// What the matcher decides, and what it refuses to decide (T067).
//
// The fakes and helpers every case in this package uses live here, beside
// the type they stand in for.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// fakeClicks stands in for the one read the matcher makes, recording what it
// was asked so a case can assert the reference actually looked up.
type fakeClicks struct {
	click clickout.Click
	err   error
	asked networks.ClickRef
	reads int
}

func (f *fakeClicks) ByRef(_ context.Context, reported networks.ClickRef) (clickout.Click, error) {
	f.asked = reported
	f.reads++
	if f.err != nil {
		return clickout.Click{}, f.err
	}
	return f.click, nil
}

// fakeUnmatched stands in for the one statement this package writes,
// recording what it was asked about so a case can assert the report named.
// detectedAt is the instant the fake's queue rows carry, fixed so a case can
// assert what was announced.
var detectedAt = time.Date(2026, time.March, 1, 7, 15, 0, 0, time.UTC)

type fakeUnmatched struct {
	err    error
	noRows bool
	asked  pgtype.UUID
	writes int
	row    store.RecordUnmatchedReferenceRow
}

func (f *fakeUnmatched) RecordUnmatchedReference(_ context.Context, id pgtype.UUID) (store.RecordUnmatchedReferenceRow, error) {
	f.asked = id
	f.writes++
	switch {
	case f.noRows:
		return store.RecordUnmatchedReferenceRow{}, pgx.ErrNoRows
	case f.err != nil:
		return store.RecordUnmatchedReferenceRow{}, f.err
	}
	row := f.row
	row.NetworkTransactionID = id
	if !row.ID.Valid {
		row.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	if !row.DetectedAt.Valid {
		// The statement returns detected_at, and what is announced about the
		// observation is read from it. A fake that left it unset would let
		// this package announce a detection at the zero time.
		row.DetectedAt = pgtype.Timestamptz{Time: detectedAt, Valid: true}
	}
	return row, nil
}

// reported is a reference a network echoed back.
func reported(ref string) networks.ClickRef { return networks.NewClickRef(ref) }

// clickoutMiss is what the click reader answers when a reference names
// nothing, which is the ordinary outcome this whole file is about.
func clickoutMiss() error { return clickout.ErrNoSuchClick }

// matcherOver builds the matcher over the given parts.
func matcherOver(t *testing.T, clicks earnings.Clicks, unmatched earnings.UnmatchedStore) *earnings.Matcher {
	t.Helper()
	matcher, err := earnings.NewMatcher(clicks, unmatched)
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}
	return matcher
}

// TestAReferenceNamingAClickIsAttributedToIt is the path a member is paid
// on. The click comes back whole, because the rate it snapshotted at click
// time is what governs the credit (FR-013) and T068 reads it from here.
func TestAReferenceNamingAClickIsAttributedToIt(t *testing.T) {
	t.Parallel()

	reportID, clickID, member := uuid.New(), uuid.New(), uuid.New()
	ref := reported("a-reference-that-names-a-click")
	clicks := &fakeClicks{click: clickout.Click{
		ID:        clickID,
		AccountID: member,
		ClickedAt: time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC),
	}}
	unmatched := &fakeUnmatched{}

	attributed, err := matcherOver(t, clicks, unmatched).
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: ref})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if !attributed.Matched {
		t.Fatal("a reference naming a click was not reported as matched")
	}
	if attributed.Click.ID != clickID || attributed.Click.AccountID != member {
		t.Errorf("Click = %+v, want the click %v belonging to %v", attributed.Click, clickID, member)
	}
	if attributed.Report != reportID {
		t.Errorf("Report = %v, want %v", attributed.Report, reportID)
	}
	// The one that would cost a member their cashback: an attributed report
	// put in the queue is a paid purchase shown to an operator as unclaimed.
	if unmatched.writes != 0 {
		t.Errorf("an attributed report was queued %d time(s)", unmatched.writes)
	}
	if got, _ := clicks.asked.Ref(); got != "a-reference-that-names-a-click" {
		t.Errorf("looked up %q, want the reference the network reported", got)
	}
}

// TestAReferenceNamingNothingIsQueuedRatherThanRefused pins the choice to
// report the miss as a value. Networks echo references minted by other
// publishers and by links that predate a deployment, so a caller working
// through a window must be able to carry on past one.
func TestAReferenceNamingNothingIsQueuedRatherThanRefused(t *testing.T) {
	t.Parallel()

	reportID, rowID := uuid.New(), uuid.New()
	unmatched := &fakeUnmatched{row: store.RecordUnmatchedReferenceRow{ID: pgtype.UUID{Bytes: rowID, Valid: true}}}

	attributed, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to")})
	if err != nil {
		t.Fatalf("Match() refused a miss: %v", err)
	}

	if attributed.Matched {
		t.Error("a reference that named nothing was reported as matched")
	}
	if attributed.Queued != rowID {
		t.Errorf("Queued = %v, want the queue row %v", attributed.Queued, rowID)
	}
	if unmatched.writes != 1 {
		t.Fatalf("the report was queued %d time(s), want once", unmatched.writes)
	}
	if uuid.UUID(unmatched.asked.Bytes) != reportID {
		t.Errorf("queued report %v, want %v", uuid.UUID(unmatched.asked.Bytes), reportID)
	}
}

// TestAReportCarryingNoReferenceIsRefused keeps the two halves of FR-034
// apart. That report is the networks module's to queue and it has already
// queued it; answering "unattributed" here would be true and would still put
// one purchase in the queue twice.
func TestAReportCarryingNoReferenceIsRefused(t *testing.T) {
	t.Parallel()

	unmatched := &fakeUnmatched{}
	clicks := &fakeClicks{}

	_, err := matcherOver(t, clicks, unmatched).
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: uuid.New()})

	if !errors.Is(err, earnings.ErrNoReference) {
		t.Fatalf("Match() error = %v, want one wrapping %v", err, earnings.ErrNoReference)
	}
	if clicks.reads != 0 || unmatched.writes != 0 {
		t.Errorf("a report with no reference read %d click(s) and wrote %d row(s), want none",
			clicks.reads, unmatched.writes)
	}
}

// TestAFailedReadIsNotAMiss is the distinction that stops a dropped
// connection becoming a permanent record that a purchase went unattributed -
// a record 0013 freezes and nothing later re-examines.
func TestAFailedReadIsNotAMiss(t *testing.T) {
	t.Parallel()

	unmatched := &fakeUnmatched{}
	clicks := &fakeClicks{err: errors.New("connection reset")}

	_, err := matcherOver(t, clicks, unmatched).
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-that-names-a-click")})

	if err == nil {
		t.Fatal("Match() reported success although the click could not be read")
	}
	if errors.Is(err, earnings.ErrNoReference) {
		t.Error("a failed read reads as a report carrying no reference")
	}
	if unmatched.writes != 0 {
		t.Errorf("a failed read queued the report %d time(s), want none", unmatched.writes)
	}
}
