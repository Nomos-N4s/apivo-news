// The accounting exports (T114, FR-062): two journals, by window.
//
// The ledger journal is every movement of a member's money as the earnings
// module recorded it - the entry, the states it moved between, the amount,
// and the transfer reference that pins the movement to the ledger's own
// record. The reconciliation journal is every difference detection found -
// which statement, which transaction, the figures, and the decision if one
// was made. An accountant reads the first against the ledger provider's
// statement and the second against the network's, which is why both carry
// the identifiers the other side uses.
//
// Both are read by when the row was written, in the order it was written,
// and both refuse to be one document past a bound: a window that holds more
// is answered as too large rather than truncated, because a truncated
// journal is one an accountant sums.

package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// MaxExportRows bounds one export document, the same bound the member's
// history export carries: enough for a month of alpha volume many times
// over, small enough to render in one response.
const MaxExportRows = 50_000

var (
	// ErrInvalidWindow reports a window with no start, no end, or an end
	// that is not after its start.
	ErrInvalidWindow = errors.New("ops: the export window cannot be read")
	// ErrExportTooLarge reports a window holding more rows than one
	// document carries. Nothing is wrong; ask for a narrower window.
	ErrExportTooLarge = errors.New("ops: the window holds more rows than one export carries")
	// ErrExportUnread reports an export the database could not answer.
	ErrExportUnread = errors.New("ops: the export could not be read")
)

// ExportWindow is the half-open interval [From, To) an export covers.
type ExportWindow struct {
	From time.Time
	To   time.Time
}

// Validate refuses an empty or inverted window, judged at the microseconds
// the database keeps.
func (w ExportWindow) Validate() error {
	from, to := w.From.Truncate(time.Microsecond), w.To.Truncate(time.Microsecond)
	switch {
	case from.IsZero() || to.IsZero():
		return fmt.Errorf("%w: it needs a start and an end", ErrInvalidWindow)
	case !to.After(from):
		return fmt.Errorf("%w: it ends at %s, which is not after it starts at %s",
			ErrInvalidWindow, to.UTC().Format(time.RFC3339Nano), from.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// LedgerRow is one movement of a member's money.
type LedgerRow struct {
	TransitionID uuid.UUID
	EntryID      uuid.UUID
	// Member is whose money, Brand under which brand it was earned, Report
	// the network report the entry cites.
	Member uuid.UUID
	Brand  string
	Report uuid.UUID
	// From is empty for the transition that opened the entry.
	From string
	To   string
	// Amount is the entry's, the money the transition moved.
	Amount money.Amount
	// TransferRef pins the movement to the ledger's own record (C-7).
	TransferRef string
	Reason      string
	// Actor is the named human where one caused the move, uuid.Nil where a
	// poll did.
	Actor      uuid.UUID
	OccurredAt time.Time
}

// ReconciliationRow is one difference, with the statement it came from.
type ReconciliationRow struct {
	DifferenceID uuid.UUID
	Run          uuid.UUID
	Account      uuid.UUID
	// Network and Publisher are the account in the network's terms.
	Network   string
	Publisher string
	Period    Period
	Kind      DifferenceKind
	// Report is uuid.Nil for money matching no report; TransactionID is the
	// network's own id either way.
	Report        uuid.UUID
	TransactionID string
	Expected      *money.Amount
	Actual        *money.Amount
	Delta         money.Amount
	DetectedAt    time.Time
	// Resolution is nil while the row is open.
	Resolution *DifferenceResolution
}

// ExportLedger reads the ledger journal for the window, oldest first.
func (s *PGStore) ExportLedger(ctx context.Context, w ExportWindow) ([]LedgerRow, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	rows, err := store.New(s.db).ExportTransitions(ctx, store.ExportTransitionsParams{
		FromAt:   pgtype.Timestamptz{Time: w.From, Valid: true},
		ToAt:     pgtype.Timestamptz{Time: w.To, Valid: true},
		PageSize: MaxExportRows + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: the ledger journal: %w", ErrExportUnread, err)
	}
	if len(rows) > MaxExportRows {
		return nil, fmt.Errorf("%w: more than %d transitions", ErrExportTooLarge, MaxExportRows)
	}
	out := make([]LedgerRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, LedgerRow{
			TransitionID: uuid.UUID(row.ID.Bytes),
			EntryID:      uuid.UUID(row.EntryID.Bytes),
			Member:       uuid.UUID(row.AccountID.Bytes),
			Brand:        row.BrandID,
			Report:       uuid.UUID(row.NetworkTransactionID.Bytes),
			From:         row.FromState.String,
			To:           row.ToState,
			Amount:       money.Amount{Minor: row.AmountMinor, Currency: money.Currency(row.Currency)},
			TransferRef:  row.LedgerTransferRef,
			Reason:       row.Reason.String,
			Actor:        uuid.UUID(row.ActorID.Bytes),
			OccurredAt:   row.OccurredAt.Time,
		})
	}
	return out, nil
}

// ExportReconciliation reads the reconciliation journal for the window,
// oldest first.
func (s *PGStore) ExportReconciliation(ctx context.Context, w ExportWindow) ([]ReconciliationRow, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	rows, err := store.New(s.db).ExportDifferences(ctx, store.ExportDifferencesParams{
		FromAt:   pgtype.Timestamptz{Time: w.From, Valid: true},
		ToAt:     pgtype.Timestamptz{Time: w.To, Valid: true},
		PageSize: MaxExportRows + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: the reconciliation journal: %w", ErrExportUnread, err)
	}
	if len(rows) > MaxExportRows {
		return nil, fmt.Errorf("%w: more than %d differences", ErrExportTooLarge, MaxExportRows)
	}
	out := make([]ReconciliationRow, 0, len(rows))
	for _, row := range rows {
		item, err := reconciliationRow(row)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrExportUnread, err)
		}
		out = append(out, item)
	}
	return out, nil
}

// reconciliationRow reads one difference into the journal's shape, deriving
// the delta the way detection announced it.
func reconciliationRow(row store.ExportDifferencesRow) (ReconciliationRow, error) {
	currency := money.Currency(row.Currency)
	item := ReconciliationRow{
		DifferenceID:  uuid.UUID(row.ID.Bytes),
		Run:           uuid.UUID(row.RunID.Bytes),
		Account:       uuid.UUID(row.NetworkAccountID.Bytes),
		Network:       row.NetworkID,
		Publisher:     row.ExternalPublisherID,
		Period:        Period{Start: row.StatementPeriodStart.Time, End: row.StatementPeriodEnd.Time},
		Kind:          DifferenceKind(row.Kind),
		TransactionID: row.StatementTransactionID.String,
		DetectedAt:    row.DetectedAt.Time,
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
		return ReconciliationRow{}, fmt.Errorf("difference %s: %w", item.DifferenceID, err)
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
