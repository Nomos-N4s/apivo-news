// Reading a run's differences (T113, US6 scenario 1, FR-060).
//
// The queue an operator works through: every disagreement a run produced,
// oldest first, each with the figures behind it, the money as a delta, and
// two facts the row alone does not say - which transaction, in the
// network's own terms, and whether the network has since restated the
// report the difference was filed against.

package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// ErrDifferencesUnread reports a listing that could not be read, for a
// reason that is not the caller's.
var ErrDifferencesUnread = errors.New("ops: the differences could not be read")

// DifferenceAfter is a keyset position in a run's differences: the page
// continues after the row found at DetectedAt with this ID. The zero value
// is the start.
type DifferenceAfter struct {
	DetectedAt time.Time
	ID         uuid.UUID
}

// DifferenceResolution is what an operator decided about a listed row.
type DifferenceResolution struct {
	Verdict    Verdict
	ResolvedBy uuid.UUID
	Reason     string
	ResolvedAt time.Time
}

// ListedDifference is one row of the queue as an operator reads it.
type ListedDifference struct {
	ID   uuid.UUID
	Kind DifferenceKind
	// Report is the report the difference names; uuid.Nil for money
	// matching no report.
	Report uuid.UUID
	// TransactionID is the network's own id for the transaction: the
	// report's external id where one is named, the statement line's
	// otherwise. It is how an operator finds it in the network's dashboard.
	TransactionID string
	// Expected and Actual are the figures on the row; either may be absent,
	// by kind. Delta is paid less owed, as the event carried it.
	Expected   *money.Amount
	Actual     *money.Amount
	Delta      money.Amount
	DetectedAt time.Time
	// Superseded reports that the network has restated the report this
	// difference names since it was filed: the disagreement is with what
	// the network used to say.
	Superseded bool
	// Resolution is nil while the row is open.
	Resolution *DifferenceResolution
}

// After is this row's position, for the cursor that continues past it.
func (l ListedDifference) After() DifferenceAfter {
	return DifferenceAfter{DetectedAt: l.DetectedAt, ID: l.ID}
}

// ListDifferences returns one page of a run's differences, oldest first,
// starting after the given position. ErrNoSuchRun for a run nobody imported,
// so that an empty page and an unknown run are not the same answer.
func (s *PGStore) ListDifferences(ctx context.Context, run uuid.UUID, after DifferenceAfter, limit int) ([]ListedDifference, error) {
	queries := store.New(s.db)
	if _, err := queries.GetReconciliationRun(ctx, pgtype.UUID{Bytes: run, Valid: true}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNoSuchRun, run)
		}
		return nil, fmt.Errorf("%w: run %s: %w", ErrDifferencesUnread, run, err)
	}
	rows, err := queries.ListDifferencesForRun(ctx, store.ListDifferencesForRunParams{
		RunID:           pgtype.UUID{Bytes: run, Valid: true},
		AfterDetectedAt: pgtype.Timestamptz{Time: after.DetectedAt, Valid: true},
		AfterID:         pgtype.UUID{Bytes: after.ID, Valid: true},
		PageSize:        int32(limit), //nolint:gosec // the handler bounds limit at maxQueueLimit
	})
	if err != nil {
		return nil, fmt.Errorf("%w: run %s: %w", ErrDifferencesUnread, run, err)
	}
	listed := make([]ListedDifference, 0, len(rows))
	for _, row := range rows {
		item, err := listedDifference(row)
		if err != nil {
			return nil, fmt.Errorf("%w: run %s: %w", ErrDifferencesUnread, run, err)
		}
		listed = append(listed, item)
	}
	return listed, nil
}

// listedDifference reads one row into the queue's shape, deriving the delta
// the same way detection announced it.
func listedDifference(row store.ListDifferencesForRunRow) (ListedDifference, error) {
	currency := money.Currency(row.Currency)
	item := ListedDifference{
		ID:            uuid.UUID(row.ID.Bytes),
		Kind:          DifferenceKind(row.Kind),
		TransactionID: row.StatementTransactionID.String,
		DetectedAt:    row.DetectedAt.Time,
		Superseded:    row.Superseded,
	}
	if row.NetworkTransactionID.Valid {
		item.Report = uuid.UUID(row.NetworkTransactionID.Bytes)
		item.TransactionID = row.ExternalID.String
	}
	if row.ExpectedMinor.Valid {
		item.Expected = &money.Amount{Minor: row.ExpectedMinor.Int64, Currency: currency}
	}
	if row.ActualMinor.Valid {
		item.Actual = &money.Amount{Minor: row.ActualMinor.Int64, Currency: currency}
	}
	delta, err := (Difference{Kind: item.Kind, Expected: item.Expected, Actual: item.Actual}).Delta()
	if err != nil {
		return ListedDifference{}, fmt.Errorf("difference %s: %w", item.ID, err)
	}
	item.Delta = delta
	if row.ResolvedAt.Valid {
		item.Resolution = &DifferenceResolution{
			Verdict:    Verdict(row.Resolution.String),
			ResolvedBy: uuid.UUID(row.ResolvedBy.Bytes),
			Reason:     row.ResolvedReason.String,
			ResolvedAt: row.ResolvedAt.Time,
		}
	}
	return item, nil
}
