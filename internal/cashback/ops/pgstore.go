package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// UnverifiedDestinations returns one page of the destinations waiting for
// somebody to prove they belong to their member (FR-051).
func (s *PGStore) UnverifiedDestinations(ctx context.Context, after DestinationAfter, limit int) ([]UnverifiedDestination, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ops: a page of unverified destinations needs a positive size, got %d", limit)
	}
	rows, err := store.New(s.db).ListUnverifiedDestinations(ctx, store.ListUnverifiedDestinationsParams{
		AfterCreatedAt: pgtype.Timestamptz{Time: after.CreatedAt, Valid: true},
		AfterID:        pgtype.UUID{Bytes: after.ID, Valid: true},
		PageSize:       int32(limit), //nolint:gosec // G115: bounded above by the caller's page size, which is small by construction.
	})
	if err != nil {
		return nil, fmt.Errorf("ops: reading the unverified destinations: %w", err)
	}
	queue := make([]UnverifiedDestination, 0, len(rows))
	for _, row := range rows {
		queue = append(queue, UnverifiedDestination{
			ID:           uuid.UUID(row.ID.Bytes),
			AccountID:    uuid.UUID(row.AccountID.Bytes),
			AccountEmail: row.AccountEmail,
			Kind:         row.Kind,
			DetailsRef:   row.DetailsRef,
			CreatedAt:    row.CreatedAt.Time,
		})
	}
	return queue, nil
}

// WithdrawalsAwaitingApproval returns one page of the requests nobody has
// decided yet, with the destination each would pay (FR-060).
//
// A read and nothing else. The three decisions this queue feeds live in the
// payout module and are reached through withdrawals.go; what happens here is
// finding a request to make one about.
func (s *PGStore) WithdrawalsAwaitingApproval(ctx context.Context, after WithdrawalAfter, limit int) ([]AwaitingWithdrawal, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ops: a page of withdrawals awaiting approval needs a positive size, got %d", limit)
	}
	rows, err := store.New(s.db).ListWithdrawalsAwaitingApproval(ctx, store.ListWithdrawalsAwaitingApprovalParams{
		AfterRequestedAt: pgtype.Timestamptz{Time: after.RequestedAt, Valid: true},
		AfterID:          pgtype.UUID{Bytes: after.ID, Valid: true},
		PageSize:         int32(limit), //nolint:gosec // G115: bounded above by the caller's page size, which is small by construction.
	})
	if err != nil {
		return nil, fmt.Errorf("ops: reading the withdrawals awaiting approval: %w", err)
	}
	queue := make([]AwaitingWithdrawal, 0, len(rows))
	for _, row := range rows {
		queue = append(queue, AwaitingWithdrawal{
			ID:                        uuid.UUID(row.ID.Bytes),
			AccountID:                 uuid.UUID(row.AccountID.Bytes),
			AccountEmail:              row.AccountEmail,
			AmountMinor:               row.AmountMinor,
			Currency:                  row.Currency,
			RequestedAt:               row.RequestedAt.Time,
			DestinationID:             uuid.UUID(row.DestinationID.Bytes),
			DestinationKind:           row.DestinationKind,
			DestinationDetailsRef:     row.DestinationDetailsRef,
			DestinationVerifiedAt:     row.DestinationVerifiedAt.Time,
			DestinationVerifiedMethod: row.DestinationVerifiedMethod.String,
			DestinationCreatedAt:      row.DestinationCreatedAt.Time,
		})
	}
	return queue, nil
}

// ConnectedNetworks returns every seeded network with its publisher
// accounts, grouped (T228).
//
// It fills neither DriverShipped nor CredentialPresent: both are facts about
// the build and the environment rather than about a row, and this store has
// no business knowing either. The composition root wraps this and answers
// them - see cmd/apivo, where the registry and the configuration live.
//
// The query LEFT JOINs, so a network with no publisher account arrives as one
// row with a null account. That is a real state and it is preserved here as
// a network with an empty Accounts slice rather than dropped.
func (s *PGStore) ConnectedNetworks(ctx context.Context) ([]ConnectedNetwork, error) {
	rows, err := store.New(s.db).ListConnectedNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: reading the connected networks: %w", err)
	}
	// Ordered by network then publisher, so one pass groups them and the
	// output keeps the order the query chose.
	networks := make([]ConnectedNetwork, 0, len(rows))
	for _, row := range rows {
		if len(networks) == 0 || networks[len(networks)-1].ID != row.NetworkID {
			networks = append(networks, ConnectedNetwork{
				ID:                  row.NetworkID,
				DisplayName:         row.DisplayName,
				ClickRefParam:       row.ClickRefParam,
				MaxQueryWindowDays:  int(row.MaxQueryWindowDays),
				RateLimitPerMinute:  int(row.RateLimitPerMinute),
				ReportingLagMinutes: int(row.ReportingLagMinutes),
				Active:              row.NetworkActive,
			})
		}
		if !row.AccountID.Valid {
			// The LEFT JOIN's null side: seeded, with nobody connected.
			continue
		}
		current := &networks[len(networks)-1]
		current.Accounts = append(current.Accounts, ConnectedAccount{
			ID:                  uuid.UUID(row.AccountID.Bytes),
			ExternalPublisherID: row.ExternalPublisherID.String,
			CredentialRef:       row.CredentialRef.String,
			CursorAt:            row.CursorAt.Time,
			TrailingCursorAt:    row.TrailingCursorAt.Time,
			BackfillFrom:        row.BackfillFrom.Time,
			ReportsCurrency:     row.ReportsCurrency.String,
			Active:              row.AccountActive.Bool,
		})
	}
	return networks, nil
}

// Verify records that a named operator proved a destination belongs to its
// member, and announces it, in one transaction (FR-051, FR-061).
//
// Idempotent by reading first. Verification is one-way - the table's guard
// refuses to re-date or re-attribute one that stands, because it is the
// evidence a withdrawal was allowed to name the destination - so a second
// call answers the verification that already exists rather than replacing
// it or failing. An operator repeating themselves has done nothing wrong,
// and the answer names who actually performed it, so they can see it was
// somebody else.
func (s *PGStore) Verify(ctx context.Context, v Verification) (Verified, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}
	// Rollback after a successful commit is a no-op; this is what makes
	// every early return below leave nothing behind.
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	// Read first, and not only to be friendly about a repeat: the update
	// cannot tell "no such destination" from "already verified", and those
	// are a not-found and a success that happened earlier.
	existing, err := queries.GetDestinationForVerification(ctx, pgtype.UUID{Bytes: v.ID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Verified{}, fmt.Errorf("%w: %s", ErrNoSuchDestination, v.ID)
	case err != nil:
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}
	if existing.VerifiedAt.Valid {
		// Nothing to write and nothing to announce: the fact is already in
		// the stream, put there by whoever performed it.
		return verifiedFrom(existing), nil
	}

	row, err := queries.VerifyDestinationAsOperator(ctx, store.VerifyDestinationAsOperatorParams{
		ID:             pgtype.UUID{Bytes: v.ID, Valid: true},
		VerifiedMethod: pgtype.Text{String: v.Method, Valid: true},
		VerifiedBy:     pgtype.UUID{Bytes: v.Operator.ID, Valid: true},
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Somebody verified it between the read and the write. The
		// statement waited on that row lock, re-read after it committed and
		// matched nothing. The destination is verified either way, so it is
		// read back rather than reported as a failure.
		return s.verifiedAlready(ctx, queries, v.ID)
	case err != nil:
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}

	verified := Verified{
		ID:         uuid.UUID(row.ID.Bytes),
		AccountID:  uuid.UUID(row.AccountID.Bytes),
		Kind:       row.Kind,
		Method:     row.VerifiedMethod.String,
		VerifiedBy: uuid.UUID(row.VerifiedBy.Bytes),
		VerifiedAt: row.VerifiedAt.Time,
	}
	payload, err := verifiedEvent(verified)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}
	// A collision is not swallowed, for the reason the dismissal gives: the
	// key is the row, the row is verifiable once, and the unique violation
	// that would report a collision has already aborted this transaction -
	// so reporting success would report a decision that cannot commit.
	if _, err := s.events.Append(ctx, tx, events.Message{
		Type:           TypeDestinationVerified,
		Subject:        verified.ID,
		IdempotencyKey: TypeDestinationVerified + ":" + verified.ID.String(),
		Payload:        payload,
	}); err != nil {
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, v.ID, err)
	}
	return verified, nil
}

// verifiedAlready reads back a verification another transaction recorded
// between this one's read and its write.
func (s *PGStore) verifiedAlready(ctx context.Context, queries *store.Queries, id uuid.UUID) (Verified, error) {
	row, err := queries.GetDestinationForVerification(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return Verified{}, fmt.Errorf("%w: %s: %w", ErrNotVerified, id, err)
	}
	return verifiedFrom(row), nil
}

// verifiedFrom maps one stored destination to the verification it carries.
// One spelling, because the read-first branch and the lost-race branch
// answer the same question and a second copy is where they would diverge.
func verifiedFrom(row store.GetDestinationForVerificationRow) Verified {
	return Verified{
		ID:         uuid.UUID(row.ID.Bytes),
		AccountID:  uuid.UUID(row.AccountID.Bytes),
		Kind:       row.Kind,
		Method:     row.VerifiedMethod.String,
		VerifiedBy: uuid.UUID(row.VerifiedBy.Bytes),
		VerifiedAt: row.VerifiedAt.Time,
	}
}

// verifiedEvent renders the contract's payload for one verification.
//
// It names the destination, its member, the method and the operator, and
// nothing about where the money goes: the details are in the vault and an
// event carrying them would put a bank account into the stream forever.
func verifiedEvent(v Verified) ([]byte, error) {
	return json.Marshal(struct {
		DestinationID  string    `json:"destination_id"`
		AccountID      string    `json:"account_id"`
		Kind           string    `json:"kind"`
		VerifiedMethod string    `json:"verified_method"`
		VerifiedBy     string    `json:"verified_by"`
		At             time.Time `json:"at"`
	}{
		DestinationID:  v.ID.String(),
		AccountID:      v.AccountID.String(),
		Kind:           v.Kind,
		VerifiedMethod: v.Method,
		VerifiedBy:     v.VerifiedBy.String(),
		At:             v.VerifiedAt,
	})
}
