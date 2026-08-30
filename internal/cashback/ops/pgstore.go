package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	networksstore "github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// DBTX is the database access this module needs, named here per the
// boundary rules. A *pgxpool.Pool satisfies it, and so does a pgx.Tx.
type DBTX = networksstore.DBTX

// Beginner is a DBTX that can also start a transaction. Every operator
// action needs one: the decision, the evidence it was taken against and the
// event announcing it are one commit or none.
type Beginner interface {
	DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PGStore is the operator surface over a real database.
//
// It reads the unattributed queue through the networks module's own queue
// rather than through a query of its own. That is deliberate and not merely
// tidy: "still open" is three conditions about evidence - nobody resolved
// it, no later report replaced the one it names, nothing has been credited
// against it - and a second spelling of them here would be a second answer
// to whether an operator is looking at money that is still theirs to
// decide. What this module owns is the decision, and only the decision.
type PGStore struct {
	db     Beginner
	events *events.Writer
}

// NewPGStore builds the store the composition root wires with the platform
// pool. It builds its own event writer: which domain these events come from
// is a fact about cashback, not a composition choice, and a writer passed
// in is a writer that could be built for the wrong producer.
func NewPGStore(db Beginner) (*PGStore, error) {
	if db == nil {
		return nil, errors.New("ops: the operator surface needs a database")
	}
	writer, err := events.NewWriter(EventProducer)
	if err != nil {
		return nil, fmt.Errorf("ops: %w", err)
	}
	return &PGStore{db: db, events: writer}, nil
}

// Open returns one page of open unattributed work.
func (s *PGStore) Open(ctx context.Context, after networks.After, limit int) ([]networks.OpenReport, error) {
	queue, err := networks.NewUnattributedQueue(networksstore.New(s.db))
	if err != nil {
		return nil, fmt.Errorf("ops: reading the unattributed queue: %w", err)
	}
	return queue.Open(ctx, after, limit)
}

// Dismiss closes one queue row and publishes that it was closed.
//
// The order inside the transaction is the argument for it. The openness
// read comes first, because an operator's page is minutes old and the
// question it answered may have changed; the update comes second, and its
// own `resolved_at is null` is what makes two operators dismissing the same
// row at the same instant end with one recorded reason and one 409, without
// a lock; the event comes last, in the same transaction, so there is no
// path that records a decision nobody hears about or announces one the
// database does not hold.
func (s *PGStore) Dismiss(ctx context.Context, d Dismissal) (Dismissed, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}
	// Rollback after a successful commit is a no-op; this is what makes
	// every early return below leave nothing behind.
	defer func() { _ = tx.Rollback(ctx) }()

	queue, err := networks.NewUnattributedQueue(networksstore.New(tx))
	if err != nil {
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}
	queries := store.New(tx)

	if _, err := queue.OpenByID(ctx, d.ID); err != nil {
		if errors.Is(err, networks.ErrNoLongerOpen) {
			return Dismissed{}, s.explain(ctx, queries, d.ID)
		}
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}

	row, err := queries.ResolveUnattributedReport(ctx, store.ResolveUnattributedReportParams{
		ID:             pgtype.UUID{Bytes: d.ID, Valid: true},
		ResolvedBy:     pgtype.UUID{Bytes: d.Operator.ID, Valid: true},
		ResolvedReason: pgtype.Text{String: d.Reason, Valid: true},
	})
	if err != nil {
		// No rows means another transaction resolved it between the read
		// above and this update: the statement waited on that row lock,
		// re-read after it committed and matched nothing. That is the race
		// the guard exists for, and the caller is told the row was taken
		// rather than having this operator's reason overwrite the other's.
		if errors.Is(err, pgx.ErrNoRows) {
			return Dismissed{}, s.explain(ctx, queries, d.ID)
		}
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}

	dismissed := Dismissed{
		ID:         uuid.UUID(row.ID.Bytes),
		ReportID:   uuid.UUID(row.NetworkTransactionID.Bytes),
		DetectedAt: row.DetectedAt.Time,
		ResolvedBy: uuid.UUID(row.ResolvedBy.Bytes),
		Reason:     row.ResolvedReason.String,
		ResolvedAt: row.ResolvedAt.Time,
	}

	payload, err := dismissedEvent(dismissed)
	if err != nil {
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}
	// A collision here is not swallowed. The key is the row, the row is
	// resolvable once, and the unique violation that would report a
	// collision has already aborted this transaction - so reporting success
	// would report a decision that cannot commit.
	if _, err := s.events.Append(ctx, tx, events.Message{
		Type:           TypeUnattributedDismissed,
		Subject:        dismissed.ID,
		IdempotencyKey: dismissedKey(dismissed.ID),
		Payload:        payload,
	}); err != nil {
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Dismissed{}, fmt.Errorf("%w: %s: %w", ErrNotDismissed, d.ID, err)
	}
	return dismissed, nil
}

// explain turns "not open" into the reason it is not, so the 409 says
// something an operator can act on - or reports that the id names no row at
// all, which is a different mistake.
//
// It runs inside the caller's transaction, so what it reports is what that
// transaction saw, not what a later snapshot might say.
func (s *PGStore) explain(ctx context.Context, queries *store.Queries, id uuid.UUID) error {
	row, err := queries.ClassifyUnattributedReport(ctx, pgtype.UUID{Bytes: id, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrNoSuchQueueRow, id)
	case err != nil:
		return fmt.Errorf("%w: %s: %w", ErrNotDismissed, id, err)
	}
	return ClosedError{
		ID: id,
		Why: ClosedReason{
			Resolved:   row.ResolvedAt.Valid,
			Reason:     row.ResolvedReason.String,
			Credited:   row.Credited,
			Superseded: row.Superseded,
		},
	}
}
