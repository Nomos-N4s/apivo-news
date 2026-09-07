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
	click   clickout.Click
	err     error
	asked   networks.ClickRef
	askedOn networks.NetworkID
	reads   int
}

func (f *fakeClicks) ByRef(_ context.Context, network networks.NetworkID, reported networks.ClickRef) (clickout.Click, error) {
	f.asked, f.askedOn = reported, network
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
	// foreignRow makes the foreign-network statement answer a row; without
	// it that statement answers no rows, as it does for a reference nobody
	// minted. Its writes are counted apart from the unmatched statement's,
	// because which statement wrote the row is the whole of what a case
	// about it asserts.
	foreignRow    bool
	foreignWrites int
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

// RecordCreditedClickReference answers as the unmatched write does: the
// matcher's own cases never reach it, and the statement it stands for is
// proved against the schema in the store's tests.
func (f *fakeUnmatched) RecordCreditedClickReference(ctx context.Context, id pgtype.UUID) (store.RecordCreditedClickReferenceRow, error) {
	row, err := f.RecordUnmatchedReference(ctx, id)
	return store.RecordCreditedClickReferenceRow(row), err
}

func (f *fakeUnmatched) RecordForeignCurrencyReference(ctx context.Context, id pgtype.UUID) (store.RecordForeignCurrencyReferenceRow, error) {
	row, err := f.RecordUnmatchedReference(ctx, id)
	return store.RecordForeignCurrencyReferenceRow(row), err
}

// RecordForeignNetworkReference answers a row only when a case says the
// reference is another network's click's; otherwise no rows, as the
// statement answers for a reference nobody minted.
func (f *fakeUnmatched) RecordForeignNetworkReference(_ context.Context, id pgtype.UUID) (store.RecordForeignNetworkReferenceRow, error) {
	f.foreignWrites++
	switch {
	case f.err != nil:
		return store.RecordForeignNetworkReferenceRow{}, f.err
	case !f.foreignRow:
		return store.RecordForeignNetworkReferenceRow{}, pgx.ErrNoRows
	}
	row := f.row
	row.NetworkTransactionID = id
	if !row.ID.Valid {
		row.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	if !row.DetectedAt.Valid {
		row.DetectedAt = pgtype.Timestamptz{Time: detectedAt, Valid: true}
	}
	return store.RecordForeignNetworkReferenceRow(row), nil
}

// reported is a reference a network echoed back.
func reported(ref string) networks.ClickRef { return networks.NewClickRef(ref) }

// onNetwork is the network every report in these files came from.
const onNetwork networks.NetworkID = "awin"

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
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: ref, Network: onNetwork})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if !attributed.Matched {
		t.Fatal("a reference naming a click was not reported as matched")
	}
	if attributed.Click.ID != clickID || attributed.Click.AccountID != member {
		t.Errorf("Click = %+v, want the click %v belonging to %v", attributed.Click, clickID, member)
	}
	// Looked up under the network that reported it (FR-096), and no other.
	if clicks.askedOn != onNetwork {
		t.Errorf("the click was looked up under %q, want the reporting network %q", clicks.askedOn, onNetwork)
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
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: reportID, Ref: reported("a-reference-nothing-answers-to"), Network: onNetwork})
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

// TestAReferenceAnotherNetworkIssuedIsQueuedThroughItsOwnStatement is
// FR-098's half of FR-096. Under the reporting network the reference names
// nothing; the unmatched statement, whose predicate is that NO click carries
// the reference, writes nothing; the foreign-network statement then does.
// The report is queued and announced exactly once, and the two statements
// each ran once - which statement wrote the row is, until the queue carries
// a reason, the only record of why.
func TestAReferenceAnotherNetworkIssuedIsQueuedThroughItsOwnStatement(t *testing.T) {
	t.Parallel()

	reportID, rowID := uuid.New(), uuid.New()
	unmatched := &fakeUnmatched{noRows: true, foreignRow: true,
		row: store.RecordUnmatchedReferenceRow{ID: pgtype.UUID{Bytes: rowID, Valid: true}}}
	out := &fakeOutbox{}

	attributed, err := matcherOver(t, &fakeClicks{err: clickoutMiss()}, unmatched).
		Match(t.Context(), out, earnings.Report{ID: reportID, Ref: reported("a-reference-another-network-issued"), Network: onNetwork})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}

	if attributed.Matched {
		t.Error("a reference another network's click carries was reported as matched")
	}
	if attributed.Queued != rowID {
		t.Errorf("Queued = %v, want the queue row %v", attributed.Queued, rowID)
	}
	if unmatched.writes != 1 || unmatched.foreignWrites != 1 {
		t.Errorf("the unmatched statement ran %d time(s) and the foreign-network one %d, want once each",
			unmatched.writes, unmatched.foreignWrites)
	}
	if announced := out.only(t, earnings.TypeTransactionUnattributed); announced.Subject != reportID.String() {
		t.Errorf("the event is about %q, want the report %s", announced.Subject, reportID)
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

// TestAReportNamingNoNetworkIsRefused: a reference is looked up among one
// network's clicks (FR-096), so a report that names none has no set to be
// looked up in. Refused rather than queued: a miss is a queue row nobody
// re-examines, and a caller's mistake would fill the queue with them.
func TestAReportNamingNoNetworkIsRefused(t *testing.T) {
	t.Parallel()

	unmatched := &fakeUnmatched{}
	clicks := &fakeClicks{}

	_, err := matcherOver(t, clicks, unmatched).
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-that-names-a-click")})

	if !errors.Is(err, earnings.ErrNoNetwork) {
		t.Fatalf("Match() error = %v, want one wrapping %v", err, earnings.ErrNoNetwork)
	}
	if clicks.reads != 0 || unmatched.writes != 0 {
		t.Errorf("a report naming no network read %d click(s) and wrote %d row(s), want none",
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
		Match(t.Context(), &fakeOutbox{}, earnings.Report{ID: uuid.New(), Ref: reported("a-reference-that-names-a-click"), Network: onNetwork})

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
