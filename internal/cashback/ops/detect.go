// Recording a run's differences (T111, US6, FR-060).
//
// Derive says what the differences are; this file writes them, as rows an
// operator works through and the confirmation gate reads (FR-043), and
// announces the pass. It changes no balance and no entry: the spec's
// independent test for this story is that an omitted and a shorted
// transaction are both flagged and neither silently moves a member's money,
// and the way to be sure nothing moves is to touch nothing but the queue.

package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoSuchRun reports a run id that names no imported statement.
	ErrNoSuchRun = errors.New("ops: no such reconciliation run")
	// ErrNotDetected reports a detection that recorded nothing, for a
	// reason that is not the statement's: the database refused or could
	// not be reached, or the stored statement could not be read - which
	// would mean import wrote what it must not.
	ErrNotDetected = errors.New("ops: the differences were not detected")
)

// Detection is what one detection pass derived and what it recorded.
type Detection struct {
	Run uuid.UUID
	// Found is every difference derived this pass, whether or not it was
	// already recorded by an earlier one.
	Found []Difference
	// Recorded is how many of Found were new: rows this pass wrote. The
	// rest were already there, resolved or not, and were not raised again.
	Recorded int
	// DetectedAt is when the first new row was written; zero if none was.
	DetectedAt time.Time
}

// DifferenceDetector is what an endpoint needs from a store to run
// detection, named here per the boundary rules. *PGStore satisfies it.
type DifferenceDetector interface {
	DetectDifferences(ctx context.Context, run uuid.UUID) (Detection, error)
}

// DetectDifferences derives a run's differences and records the new ones.
//
// One transaction: the run is read, the reports it is judged against are
// read in the same snapshot, every derived difference is offered to the
// table and the table keeps the ones it does not already have (0029), and
// every row it keeps is announced, one event each, as the contract lists
// it. A repeat that finds nothing new writes nothing and announces nothing:
// the queue already says everything this pass would have said.
//
// No operator, because detection is derivation from stored facts and the
// contract's event carries none: the named human behind a run is the one
// who imported the statement, and that is on the run and in its event.
func (s *PGStore) DetectDifferences(ctx context.Context, run uuid.UUID) (Detection, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Detection{}, fmt.Errorf("%w: %w", ErrNotDetected, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	row, err := queries.GetReconciliationRun(ctx, pgtype.UUID{Bytes: run, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Detection{}, fmt.Errorf("%w: %s", ErrNoSuchRun, run)
	case err != nil:
		return Detection{}, fmt.Errorf("%w: reading run %s: %w", ErrNotDetected, run, err)
	}
	period := Period{Start: row.StatementPeriodStart.Time, End: row.StatementPeriodEnd.Time}
	// The stored statement was read in full before it was written, so a
	// failure here is not the statement's: it is this module having
	// changed what a line is since the import, which is exactly what
	// sharing the reader is meant to prevent.
	lines, err := (Statement{Raw: row.RawStatement}).Lines()
	if err != nil {
		return Detection{}, fmt.Errorf("%w: run %s holds a statement this module can no longer read: %w", ErrNotDetected, run, err)
	}
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		ids = append(ids, line.TransactionID)
	}

	rows, err := queries.CurrentReportsForStatement(ctx, store.CurrentReportsForStatementParams{
		NetworkAccountID:        row.NetworkAccountID,
		StatementTransactionIds: ids,
		PeriodStart:             row.StatementPeriodStart,
		PeriodEnd:               row.StatementPeriodEnd,
	})
	if err != nil {
		return Detection{}, fmt.Errorf("%w: reading the reports for run %s: %w", ErrNotDetected, run, err)
	}
	reports := make([]CurrentReport, 0, len(rows))
	for _, r := range rows {
		reports = append(reports, CurrentReport{
			ID:           uuid.UUID(r.ID.Bytes),
			ExternalID:   r.ExternalID,
			Status:       networks.Status(r.Status),
			Commission:   money.Amount{Minor: r.CommissionMinor, Currency: money.Currency(r.Currency)},
			TransactedAt: r.TransactedAt.Time,
		})
	}

	detection := Detection{Run: run, Found: Derive(lines, reports, period)}
	for _, d := range detection.Found {
		recorded, err := queries.RecordDifference(ctx, differenceParams(row, d))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Already there, from an earlier pass. Not raised again, and
			// not announced again: its event is already in the stream.
			continue
		case err != nil:
			return Detection{}, fmt.Errorf("%w: recording %s for %s in run %s: %w", ErrNotDetected, d.Kind, d.TransactionID, run, err)
		}
		id := uuid.UUID(recorded.ID.Bytes)
		if detection.Recorded == 0 {
			detection.DetectedAt = recorded.DetectedAt.Time
		}
		detection.Recorded++

		payload, err := foundEvent(run, id, d, recorded.DetectedAt.Time)
		if err != nil {
			return Detection{}, fmt.Errorf("%w: %w", ErrNotDetected, err)
		}
		if _, err := s.events.Append(ctx, tx, events.Message{
			Type:           TypeDifferenceFound,
			Subject:        id,
			IdempotencyKey: foundKey(id),
			Payload:        payload,
		}); err != nil {
			return Detection{}, fmt.Errorf("%w: %w", ErrNotDetected, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Detection{}, fmt.Errorf("%w: %w", ErrNotDetected, err)
	}
	return detection, nil
}

// differenceParams spells one derived difference as the row the schema
// accepts: exactly the facts that make it its kind, and none that would
// contradict it (0015, 0029).
func differenceParams(run store.GetReconciliationRunRow, d Difference) store.RecordDifferenceParams {
	params := store.RecordDifferenceParams{
		RunID:            run.ID,
		NetworkAccountID: run.NetworkAccountID,
		Kind:             string(d.Kind),
		Currency:         d.Currency().String(),
	}
	if d.Report != uuid.Nil {
		params.NetworkTransactionID = pgtype.UUID{Bytes: d.Report, Valid: true}
	} else {
		params.StatementTransactionID = pgtype.Text{String: d.TransactionID, Valid: true}
	}
	if d.Expected != nil {
		params.ExpectedMinor = pgtype.Int8{Int64: d.Expected.Minor, Valid: true}
	}
	if d.Actual != nil {
		params.ActualMinor = pgtype.Int8{Int64: d.Actual.Minor, Valid: true}
	}
	return params
}
