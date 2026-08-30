// The tests for unattributed.go: what the Go layer decides, which is
// deliberately very little.
//
// The predicates all live in SQL and are asserted against the real schema in
// the store package. What is left here is the seam - refusing to ask about a
// report the database did not write, telling "nothing to do" apart from "it
// could not be written", and reading back what the row says rather than what
// the caller supplied - and a fake store is the only way to drive those,
// because a real one cannot be made to answer wrongly.

package networks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// unattributedTestStore answers each of the three statements with whatever a
// case needs, and remembers what it was asked.
type unattributedTestStore struct {
	recorded []pgtype.UUID
	listed   []store.ListOpenUnattributedReportsParams
	asked    []pgtype.UUID

	row  store.RecordUnattributedReportRow
	page []store.ListOpenUnattributedReportsRow
	one  store.GetOpenUnattributedReportRow
	fail error
}

func (s *unattributedTestStore) RecordUnattributedReport(_ context.Context, id pgtype.UUID) (store.RecordUnattributedReportRow, error) {
	s.recorded = append(s.recorded, id)
	if s.fail != nil {
		return store.RecordUnattributedReportRow{}, s.fail
	}
	return s.row, nil
}

func (s *unattributedTestStore) ListOpenUnattributedReports(_ context.Context, arg store.ListOpenUnattributedReportsParams) ([]store.ListOpenUnattributedReportsRow, error) {
	s.listed = append(s.listed, arg)
	if s.fail != nil {
		return nil, s.fail
	}
	return s.page, nil
}

func (s *unattributedTestStore) GetOpenUnattributedReport(_ context.Context, id pgtype.UUID) (store.GetOpenUnattributedReportRow, error) {
	s.asked = append(s.asked, id)
	if s.fail != nil {
		return store.GetOpenUnattributedReportRow{}, s.fail
	}
	return s.one, nil
}

// unattributedTestRow is one open row of the shape the query returns.
func unattributedTestRow(t *testing.T, detectedAt time.Time, attributable bool) store.GetOpenUnattributedReportRow {
	t.Helper()
	return store.GetOpenUnattributedReportRow{
		ID:                   pgtype.UUID{Bytes: uuid.New(), Valid: true},
		DetectedAt:           pgtype.Timestamptz{Time: detectedAt, Valid: true},
		NetworkTransactionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		NetworkID:            "fixture",
		NetworkAccountID:     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ExternalID:           "FIX-1002",
		Status:               string(networks.StatusDeclined),
		SaleAmountMinor:      15000,
		CommissionMinor:      1125,
		Currency:             "GBP",
		TransactedAt:         pgtype.Timestamptz{Time: detectedAt.Add(-72 * time.Hour), Valid: true},
		RetrievedAt:          pgtype.Timestamptz{Time: detectedAt, Valid: true},
		Attributable:         attributable,
	}
}

func TestNewUnattributedQueueNeedsAStore(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewUnattributedQueue(nil); !errors.Is(err, networks.ErrNoUnattributedStore) {
		t.Errorf("NewUnattributedQueue(nil) = %v, want one wrapping ErrNoUnattributedStore", err)
	}
	if _, err := networks.NewUnattributedQueue(&unattributedTestStore{}); err != nil {
		t.Errorf("NewUnattributedQueue() refused a usable store: %v", err)
	}
}

// TestRecordRefusesAReportTheDatabaseDidNotWrite is why Record takes a
// Recorded rather than a uuid. The statement finds nothing for an id naming
// no row, so without this a caller's mistake would pass as "nothing to do" -
// and the caller's mistake here is the zero Recorded an unchanged re-report
// yields, which is the commonest value in the poller's loop.
func TestRecordRefusesAReportTheDatabaseDidNotWrite(t *testing.T) {
	t.Parallel()

	fake := &unattributedTestStore{}
	queue, err := networks.NewUnattributedQueue(fake)
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}

	queued, wrote, err := queue.Record(t.Context(), networks.Recorded{})
	if !errors.Is(err, networks.ErrUnstoredReport) {
		t.Fatalf("Record(zero) = %v, want one wrapping ErrUnstoredReport", err)
	}
	if wrote || queued != (networks.Queued{}) {
		t.Errorf("a refused observation came back as %+v / %t", queued, wrote)
	}
	if len(fake.recorded) != 0 {
		t.Errorf("a refused observation reached the store as %v", fake.recorded)
	}
}

// TestRecordReadsBackWhatTheRowSays holds the same rule Recorded holds about
// its digest: detected_at has a column default, so the caller has no value to
// echo and must be handed the row's own. The event published for this
// observation and the row an operator reads have to name one instant.
func TestRecordReadsBackWhatTheRowSays(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	rowID, reportID := uuid.New(), uuid.New()
	fake := &unattributedTestStore{row: store.RecordUnattributedReportRow{
		ID:                   pgtype.UUID{Bytes: rowID, Valid: true},
		NetworkTransactionID: pgtype.UUID{Bytes: reportID, Valid: true},
		DetectedAt:           pgtype.Timestamptz{Time: at, Valid: true},
	}}
	queue, err := networks.NewUnattributedQueue(fake)
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}

	queued, wrote, err := queue.Record(t.Context(), networks.Recorded{ID: reportID})
	if err != nil || !wrote {
		t.Fatalf("Record() = %t, %v", wrote, err)
	}
	if queued.ID != rowID || queued.ReportID != reportID {
		t.Errorf("the observation is %+v, want row %s about report %s", queued, rowID, reportID)
	}
	if !queued.DetectedAt.Equal(at) {
		t.Errorf("the observation was detected at %s, want the row's own %s", queued.DetectedAt, at)
	}
	if len(fake.recorded) != 1 || uuid.UUID(fake.recorded[0].Bytes) != reportID {
		t.Errorf("the store was asked about %v, want the report %s", fake.recorded, reportID)
	}
}

// TestRecordTellsNothingToDoApartFromCouldNotBeWritten is the distinction
// the poller's window depends on. No rows means the report carried a
// reference or the observation was already stored - both ordinary, and the
// second is the path every re-read after a crash takes. Anything else means
// the window is not fully recorded, and its cursor must not move.
func TestRecordTellsNothingToDoApartFromCouldNotBeWritten(t *testing.T) {
	t.Parallel()

	quiet, err := networks.NewUnattributedQueue(&unattributedTestStore{fail: pgx.ErrNoRows})
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	queued, wrote, err := quiet.Record(t.Context(), networks.Recorded{ID: uuid.New()})
	if err != nil {
		t.Fatalf("an attributed or already-recorded report was reported as a failure: %v", err)
	}
	if wrote || queued != (networks.Queued{}) {
		t.Errorf("nothing was written, but Record answered %+v / %t", queued, wrote)
	}

	broken := errors.New("connection reset")
	failing, err := networks.NewUnattributedQueue(&unattributedTestStore{fail: broken})
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	_, wrote, err = failing.Record(t.Context(), networks.Recorded{ID: uuid.New()})
	if !errors.Is(err, networks.ErrNotRecorded) {
		t.Fatalf("Record() = %v, want one wrapping ErrNotRecorded", err)
	}
	if !errors.Is(err, broken) {
		t.Errorf("Record() = %v, want it to carry the cause", err)
	}
	if wrote {
		t.Error("a failed write reported that it wrote")
	}
}

// TestOpenReadsTheRowAsMoneyAndAPosition covers the mapping, which is the
// only other thing this layer does: the row's single currency governs both
// amounts (C-6), and the position a caller pages from carries both ordering
// columns because one poll's observations all share an instant.
func TestOpenReadsTheRowAsMoneyAndAPosition(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	row := unattributedTestRow(t, at, true)
	fake := &unattributedTestStore{page: []store.ListOpenUnattributedReportsRow{store.ListOpenUnattributedReportsRow(row)}}
	queue, err := networks.NewUnattributedQueue(fake)
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}

	open, err := queue.Open(t.Context(), networks.After{}, 20)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("Open() returned %d row(s), want 1", len(open))
	}

	want, err := money.New(1125, money.Currency("GBP"))
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	if open[0].Commission != want {
		t.Errorf("the commission is %s, want %s", open[0].Commission, want)
	}
	if open[0].Sale.Currency != open[0].Commission.Currency {
		t.Errorf("the two amounts carry %s and %s; the row stores one currency for both",
			open[0].Sale.Currency, open[0].Commission.Currency)
	}
	if open[0].Status != networks.StatusDeclined || open[0].ExternalID != "FIX-1002" {
		t.Errorf("the row reads %s/%s, want declined/FIX-1002", open[0].Status, open[0].ExternalID)
	}
	if !open[0].Attributable {
		t.Error("an attributable row came back as one an operator may only dismiss")
	}

	after := open[0].After()
	if !after.DetectedAt.Equal(at) || after.ID != open[0].ID {
		t.Errorf("the next page starts at %+v, want both ordering columns of the last row", after)
	}
	if got := fake.listed[0]; !got.AfterDetectedAt.Time.IsZero() || got.PageSize != 20 {
		t.Errorf("the store was asked for %+v, want a page of 20 from the beginning", got)
	}
}

// TestOpenRefusesAPageOfNothing keeps a caller's mistake from reaching the
// database as a query that returns nothing and looks like an empty queue.
func TestOpenRefusesAPageOfNothing(t *testing.T) {
	t.Parallel()

	fake := &unattributedTestStore{}
	queue, err := networks.NewUnattributedQueue(fake)
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	if _, err := queue.Open(t.Context(), networks.After{}, 0); err == nil {
		t.Fatal("a page of nothing was accepted")
	}
	if len(fake.listed) != 0 {
		t.Errorf("a refused page reached the store: %+v", fake.listed)
	}
}

// TestOpenByIDSaysTheAnswerChangedRatherThanNotFound is the distinction that
// stops a stale page becoming a second credit. The query asks the whole
// question again, so no rows means the report has been superseded, resolved
// or credited since the page was rendered - never that the row is missing.
func TestOpenByIDSaysTheAnswerChangedRatherThanNotFound(t *testing.T) {
	t.Parallel()

	gone, err := networks.NewUnattributedQueue(&unattributedTestStore{fail: pgx.ErrNoRows})
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	id := uuid.New()
	if _, err := gone.OpenByID(t.Context(), id); !errors.Is(err, networks.ErrNoLongerOpen) {
		t.Fatalf("OpenByID() = %v, want one wrapping ErrNoLongerOpen", err)
	}

	at := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	still := &unattributedTestStore{one: unattributedTestRow(t, at, false)}
	queue, err := networks.NewUnattributedQueue(still)
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	open, err := queue.OpenByID(t.Context(), id)
	if err != nil {
		t.Fatalf("OpenByID(): %v", err)
	}
	if open.Attributable {
		t.Error("a report naming a click nobody minted was offered as attributable")
	}
	if len(still.asked) != 1 || uuid.UUID(still.asked[0].Bytes) != id {
		t.Errorf("the store was asked about %v, want %s", still.asked, id)
	}
}

// TestOpenRefusesAnUnreadableCurrency is the last gate before an operator
// decides money on a row. A currency the money type refuses cannot be
// rendered, compared or split, and a zero Amount in its place would be a
// screen showing nothing owed.
func TestOpenRefusesAnUnreadableCurrency(t *testing.T) {
	t.Parallel()

	row := unattributedTestRow(t, time.Now(), true)
	row.Currency = "gbp"
	queue, err := networks.NewUnattributedQueue(&unattributedTestStore{one: row})
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	if _, err := queue.OpenByID(t.Context(), uuid.New()); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Fatalf("OpenByID() = %v, want one wrapping money.ErrInvalidCurrency", err)
	}
}
