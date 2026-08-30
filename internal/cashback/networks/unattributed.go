// The unattributed queue: recording that a report arrived with no click
// reference, and answering which of those records is still an operator's
// work (T058, FR-034).
//
// Two different kinds of thing, and keeping them apart is the whole design.
// What was SEEN is written once and never edited: the row is the record that
// this report went unattributed at this instant, the schema freezes both
// (0013) and refuses to let the row be deleted or its resolution erased
// (0024). Whether it is still WORK is a property of the transaction NOW - a
// later report carrying a reference does not make the earlier observation
// false, any more than a reversal makes a confirmation false (C-3) - so that
// question is asked at read time and answered by the chain.

package networks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoUnattributedStore reports a queue built with nothing to write to.
	// Refused at construction for the reason [NewEvidenceWriter] refuses one:
	// a poller that discovered it mid-window has already read a window it
	// cannot record.
	ErrNoUnattributedStore = errors.New("networks: an unattributed queue needs a store to write to")
	// ErrUnstoredReport reports an attempt to record an observation about a
	// report the database did not write - the zero [Recorded] an unchanged
	// re-report yields.
	//
	// A named refusal rather than a quiet no-op, because the two are
	// indistinguishable from outside and only one of them is a defect: a
	// report stored and never examined is money nobody will look for again.
	ErrUnstoredReport = errors.New("networks: only a report the database stored can be recorded as unattributed")
	// ErrNotRecorded reports an observation that could not be written. It
	// wraps the database's refusal unchanged, as [ErrEvidenceNotWritten]
	// does: the observation and the evidence it names commit together, so a
	// caller that swallowed this would advance a cursor past a window whose
	// observations are missing.
	ErrNotRecorded = errors.New("networks: the report could not be recorded as unattributed")
	// ErrNoLongerOpen reports a queue row that is no longer work: somebody
	// resolved it, something has been credited against the report it names,
	// or a later report has superseded that one.
	//
	// It is the answer to an operator page rendered before the last poll, and
	// it is what stops an action against a superseded report becoming a
	// second credit. An entry may legally cite a superseded report - the
	// evidence guard reads the click reference from whichever report the
	// entry names - so an operator attributing a stale root while the
	// automatic path credits the attributed tip would write two entries for
	// one purchase, past entry_one_per_report, because those are two
	// different reports.
	ErrNoLongerOpen = errors.New("networks: the report is no longer open unattributed work")
)

// UnattributedStore is the write and the two reads this file needs, named
// here rather than taken as *store.Queries so the caller keeps the
// transaction boundary. For the write that is not a convenience: the
// observation, the evidence it names and the cursor that follows are one
// commit or none.
type UnattributedStore interface {
	RecordUnattributedReport(ctx context.Context, networkTransactionID pgtype.UUID) (store.RecordUnattributedReportRow, error)
	ListOpenUnattributedReports(ctx context.Context, arg store.ListOpenUnattributedReportsParams) ([]store.ListOpenUnattributedReportsRow, error)
	GetOpenUnattributedReport(ctx context.Context, id pgtype.UUID) (store.GetOpenUnattributedReportRow, error)
}

// Queued is what the DATABASE decided about one observation: the row that
// records it, the report it names, and the instant it stamped.
//
// The instant is read back rather than assumed, for the reason [Recorded]
// reads its digest back: the column has a default, so a caller that echoed
// its own value would report an instant the row does not carry - and the
// event published for this observation (T062) must name the same one the
// operator reads.
type Queued struct {
	// ID is the queue row.
	ID uuid.UUID
	// ReportID is the evidence row it was recorded about.
	ReportID uuid.UUID
	// DetectedAt is when the row says it was noticed.
	DetectedAt time.Time
}

// OpenReport is one line of unattributed work: the queue row's identity and
// the facts of the report it cites.
//
// Those facts are current by construction. A row is open only while the
// report it names is still what the network last said about the transaction,
// so the amounts here are never money the network has since withdrawn - the
// open row IS the current report rather than a join to it.
type OpenReport struct {
	// ID is the queue row, and what an operator action names.
	ID uuid.UUID
	// DetectedAt is when this observation was recorded.
	DetectedAt time.Time
	// Report is the evidence row, and the one an entry must cite so that
	// entry_one_per_report refuses a second credit for this purchase.
	Report uuid.UUID
	// Account is the publisher account whose poll found it.
	Account uuid.UUID
	// Network and ExternalID are how an operator finds the transaction in
	// the network's own dashboard.
	Network    NetworkID
	ExternalID string
	// Status is what the network last said about it.
	Status Status
	// Sale is what the member spent and Commission is what the network says
	// it will pay; the member's share is computed from the second (FR-040).
	Sale       money.Amount
	Commission money.Amount
	// TransactedAt is when the purchase happened, RetrievedAt when this
	// report was read.
	TransactedAt time.Time
	RetrievedAt  time.Time
	// Attributable reports whether an entry may be created for this report
	// at all. It is false where the network named a reference that matched
	// no click: the evidence guard refuses a null click there and there is
	// no click to cite, so dismissing is the only lawful outcome. Derived
	// from immutable evidence rather than stored, because a column would be
	// a second copy to fall out of step.
	Attributable bool
}

// After is a position in the open queue: everything ordered after this row.
// The zero value starts at the beginning.
//
// It carries both columns because every observation one poll records shares
// its detection instant exactly - the poller reads its clock once per poll -
// so the instant alone is not a total order, and a page keyed on it would
// skip rows or repeat them.
type After struct {
	DetectedAt time.Time
	ID         uuid.UUID
}

// After is the position just past this row, for asking for the next page.
func (o OpenReport) After() After { return After{DetectedAt: o.DetectedAt, ID: o.ID} }

// UnattributedQueue records that a report went unattributed, and answers
// which of those records is still work.
//
// It never credits, never resolves, never edits and never deletes. Every
// question it answers about a report is answered from what the network said
// and where that report sits in its chain.
type UnattributedQueue struct {
	store UnattributedStore
}

// NewUnattributedQueue builds the queue over the given store, refusing a nil
// one.
func NewUnattributedQueue(s UnattributedStore) (*UnattributedQueue, error) {
	if s == nil {
		return nil, ErrNoUnattributedStore
	}
	return &UnattributedQueue{store: s}, nil
}

// Record writes the observation if the report the database stored carries no
// click reference, reporting whether it wrote one.
//
// Nothing here reads a click reference. The statement's own WHERE clause
// decides, for the reason the content digest is computed by a trigger rather
// than supplied: the stored column is the authority on what the network
// said, and it is the same column the evidence guard reads when it later
// decides whether a credit may omit its click. A second implementation in Go
// is the place the two would eventually disagree - as a member never paid,
// or as money in no queue at all.
//
// False and no error covers two outcomes that mean the same thing to a
// caller: the report carried a reference, and the observation was already
// recorded. The second is the ordinary path after a crash, when the poller
// re-reads a window whose observations it had already written.
//
// It takes a [Recorded] rather than an id so it cannot be asked about a
// report the database did not just write. That is load-bearing: the
// statement finds nothing for an id that names no row, so a bare uuid would
// let a caller's mistake pass as "nothing to do".
func (q *UnattributedQueue) Record(ctx context.Context, recorded Recorded) (Queued, bool, error) {
	if recorded.ID == uuid.Nil {
		return Queued{}, false, ErrUnstoredReport
	}

	row, err := q.store.RecordUnattributedReport(ctx, pgtype.UUID{Bytes: recorded.ID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Queued{}, false, nil
	case err != nil:
		return Queued{}, false, fmt.Errorf("%w: report %s: %w", ErrNotRecorded, recorded.ID, err)
	}
	return Queued{
		ID:         uuid.UUID(row.ID.Bytes),
		ReportID:   uuid.UUID(row.NetworkTransactionID.Bytes),
		DetectedAt: row.DetectedAt.Time,
	}, true, nil
}

// Open is one page of unattributed work, oldest first: observations nobody
// has resolved, whose report is still what the network last said about the
// transaction and carries no credit.
func (q *UnattributedQueue) Open(ctx context.Context, after After, limit int) ([]OpenReport, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("networks: a page of unattributed work needs a positive size, got %d", limit)
	}
	rows, err := q.store.ListOpenUnattributedReports(ctx, store.ListOpenUnattributedReportsParams{
		AfterDetectedAt: pgtype.Timestamptz{Time: after.DetectedAt, Valid: true},
		AfterID:         pgtype.UUID{Bytes: after.ID, Valid: true},
		PageSize:        int32(limit), //nolint:gosec // G115: bounded above by the caller's page size, which is small by construction.
	})
	if err != nil {
		return nil, fmt.Errorf("networks: reading the unattributed queue: %w", err)
	}

	open := make([]OpenReport, 0, len(rows))
	for _, row := range rows {
		report, err := openReport(store.GetOpenUnattributedReportRow(row))
		if err != nil {
			return nil, err
		}
		open = append(open, report)
	}
	return open, nil
}

// OpenByID asks the same question about one row, and is what an operator
// action calls in its own transaction before it acts.
//
// Deliberately not a lookup by id. A page is minutes or hours old, and
// between the render and the click a poll may have superseded the report
// with an attributed one; answering [ErrNoLongerOpen] there is what stops a
// stale page becoming a second credit.
func (q *UnattributedQueue) OpenByID(ctx context.Context, id uuid.UUID) (OpenReport, error) {
	row, err := q.store.GetOpenUnattributedReport(ctx, pgtype.UUID{Bytes: id, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return OpenReport{}, fmt.Errorf("%w: %s", ErrNoLongerOpen, id)
	case err != nil:
		return OpenReport{}, fmt.Errorf("networks: reading unattributed work %s: %w", id, err)
	}
	return openReport(row)
}

// openReport turns one stored row into the value a caller reads.
//
// The two amounts are built through [money.New] rather than assembled,
// because a row is only ever as good as the currency beside it and this is
// the last place that can be checked before an operator decides money on it.
// The row's single currency column covers both, exactly as the evidence row
// stores one for both (C-6).
func openReport(row store.GetOpenUnattributedReportRow) (OpenReport, error) {
	currency := money.Currency(row.Currency)
	sale, err := money.New(row.SaleAmountMinor, currency)
	if err != nil {
		return OpenReport{}, fmt.Errorf("networks: unattributed work %v: sale amount: %w", row.ID, err)
	}
	commission, err := money.New(row.CommissionMinor, currency)
	if err != nil {
		return OpenReport{}, fmt.Errorf("networks: unattributed work %v: commission: %w", row.ID, err)
	}
	return OpenReport{
		ID:           uuid.UUID(row.ID.Bytes),
		DetectedAt:   row.DetectedAt.Time,
		Report:       uuid.UUID(row.NetworkTransactionID.Bytes),
		Account:      uuid.UUID(row.NetworkAccountID.Bytes),
		Network:      NetworkID(row.NetworkID),
		ExternalID:   row.ExternalID,
		Status:       Status(row.Status),
		Sale:         sale,
		Commission:   commission,
		TransactedAt: row.TransactedAt.Time,
		RetrievedAt:  row.RetrievedAt.Time,
		Attributable: row.Attributable,
	}, nil
}
