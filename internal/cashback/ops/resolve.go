// Resolving a difference (T112, US6, FR-061).
//
// A difference is the queue's question: the statement and the reports
// disagree about this transaction - what now? Resolving it is a named human
// answering, with a verdict and a reason, once. The row records the answer,
// the event announces it, and the confirmation gate stops counting the
// disagreement against the transaction (FR-043).
//
// NOTHING ELSE MOVES, and that is deliberate rather than unfinished. US6
// scenario 2 says any member-facing effect of a resolution is a new ledger
// posting, never an edit - and every posting this system makes rests on
// evidence. A shorted or an unpaid report becomes a smaller or a reversed
// credit when the NETWORK restates it: its superseding report is the
// evidence (C-3), and the reversal path in earnings cites that report, never
// an operator's reading of what the network meant. So there is no verdict
// here that moves a member's money, and no verdict that says "the network
// owes us and we are chasing it" either: an open difference IS the chase,
// and it keeps the gate shut until the money arrives or the network
// restates the report. The two verdicts there are both mean the question is
// closed - see [Verdict].

package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// Verdict is what an operator decided about a difference. Either lifts the
// difference from the confirmation gate.
type Verdict string

const (
	// VerdictExplained means another fact accounts for the disagreement and
	// nothing is owed either way: a later statement paid the report, the
	// network has since restated it and the reversal followed, two lines
	// were one payment, a currency-mismatched pair was one transaction.
	VerdictExplained Verdict = "explained"
	// VerdictAbsorbed means the delta is the business's to bear or to keep,
	// and the member's figure stands as reported: a shortfall too small to
	// dispute, or money nothing expected that nobody is owed.
	VerdictAbsorbed Verdict = "absorbed"
)

// Valid reports whether v is a verdict the schema knows.
func (v Verdict) Valid() bool {
	switch v {
	case VerdictExplained, VerdictAbsorbed:
		return true
	}
	return false
}

var (
	// ErrInvalidResolution reports a resolution that cannot be recorded as
	// stated: no row named, a verdict the schema does not know, a blank or
	// overlong reason, or nobody deciding. The text after it says which.
	ErrInvalidResolution = errors.New("ops: the resolution cannot be recorded")
	// ErrNoSuchDifference reports an id that names no difference. Rows are
	// never deleted, so an unknown id was never issued.
	ErrNoSuchDifference = errors.New("ops: no such difference")
	// ErrNotResolved reports a resolution the database refused or could not
	// take, for a reason that is not the resolution's. Nothing is recorded
	// when it is returned.
	ErrNotResolved = errors.New("ops: the difference was not resolved")
)

// AlreadyResolvedError reports a difference somebody decided first, and what
// they decided, so an operator whose page was minutes old is told what
// happened rather than having their verdict overwrite a colleague's.
type AlreadyResolvedError struct {
	ID      uuid.UUID
	By      uuid.UUID
	Verdict Verdict
	Reason  string
	At      time.Time
}

func (e AlreadyResolvedError) Error() string {
	return fmt.Sprintf("ops: difference %s was already resolved as %s by %s at %s: %s",
		e.ID, e.Verdict, e.By, e.At.UTC().Format(time.RFC3339), e.Reason)
}

// Resolution is one operator's decision about one difference.
type Resolution struct {
	// ID is the difference being decided.
	ID uuid.UUID
	// Verdict is what was decided; see [Verdict].
	Verdict Verdict
	// Reason is why, in the operator's words. Required and non-blank
	// (FR-061); it goes on the row and into the event.
	Reason string
	// Operator is the named human deciding, recorded as resolved_by.
	Operator Operator
}

// Validate refuses a resolution the schema or FR-061 would: it names no
// row, its verdict is unknown, its reason is blank or longer than
// maxReasonRunes, or nobody is making it. The reason is compared trimmed,
// which is also how it is recorded.
func (r Resolution) Validate() error {
	reason := strings.TrimSpace(r.Reason)
	switch {
	case r.ID == uuid.Nil:
		return fmt.Errorf("%w: it names no difference", ErrInvalidResolution)
	case !r.Verdict.Valid():
		return fmt.Errorf("%w: %q is not a verdict; use %s or %s", ErrInvalidResolution, string(r.Verdict), VerdictExplained, VerdictAbsorbed)
	case reason == "":
		return fmt.Errorf("%w: a resolution records why (FR-061); supply a non-blank reason", ErrInvalidResolution)
	case len([]rune(reason)) > maxReasonRunes:
		return fmt.Errorf("%w: the reason is longer than %s characters", ErrInvalidResolution, strconv.Itoa(maxReasonRunes))
	case r.Operator.ID == uuid.Nil:
		return fmt.Errorf("%w: nobody is deciding it; a resolution records who (FR-061)", ErrInvalidResolution)
	}
	return nil
}

// Resolved is the recorded decision.
type Resolved struct {
	ID         uuid.UUID
	Run        uuid.UUID
	Kind       DifferenceKind
	Verdict    Verdict
	ResolvedBy uuid.UUID
	Reason     string
	ResolvedAt time.Time
}

// DifferenceResolver is what the resolve endpoint needs from a store, named
// here per the boundary rules. *PGStore satisfies it.
type DifferenceResolver interface {
	ResolveDifference(ctx context.Context, r Resolution) (Resolved, error)
}

// ResolveDifference records one decision and announces it.
//
// One transaction, and the order inside it is the argument for it: the
// update comes first, and its own `resolved_at is null` is what makes two
// operators deciding the same row at the same instant end with one recorded
// verdict and one refusal, without a lock; a refusal is then explained -
// no such row, or decided already, by whom and as what - in the same
// snapshot; and the event goes last, with the row, so there is no path that
// records a decision nobody hears about or announces one the database does
// not hold.
func (s *PGStore) ResolveDifference(ctx context.Context, r Resolution) (Resolved, error) {
	if err := r.Validate(); err != nil {
		return Resolved{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotResolved, r.ID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	row, err := queries.ResolveDifference(ctx, store.ResolveDifferenceParams{
		ID:             pgtype.UUID{Bytes: r.ID, Valid: true},
		ResolvedBy:     pgtype.UUID{Bytes: r.Operator.ID, Valid: true},
		ResolvedReason: pgtype.Text{String: strings.TrimSpace(r.Reason), Valid: true},
		Resolution:     pgtype.Text{String: string(r.Verdict), Valid: true},
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Resolved{}, s.explainDifference(ctx, queries, r.ID)
	case err != nil:
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotResolved, r.ID, err)
	}

	resolved := Resolved{
		ID:         uuid.UUID(row.ID.Bytes),
		Run:        uuid.UUID(row.RunID.Bytes),
		Kind:       DifferenceKind(row.Kind),
		Verdict:    Verdict(row.Resolution.String),
		ResolvedBy: uuid.UUID(row.ResolvedBy.Bytes),
		Reason:     row.ResolvedReason.String,
		ResolvedAt: row.ResolvedAt.Time,
	}
	payload, err := resolvedEvent(resolved)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotResolved, r.ID, err)
	}
	if _, err := s.events.Append(ctx, tx, events.Message{
		Type:           TypeDifferenceResolved,
		Subject:        resolved.ID,
		IdempotencyKey: resolvedKey(resolved.ID),
		Payload:        payload,
	}); err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotResolved, r.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrNotResolved, r.ID, err)
	}
	return resolved, nil
}

// explainDifference turns "no row updated" into the reason: the id names
// nothing, or somebody decided it first. Read in the caller's transaction,
// so what it reports is what the update saw.
func (s *PGStore) explainDifference(ctx context.Context, queries *store.Queries, id uuid.UUID) error {
	row, err := queries.ClassifyDifference(ctx, pgtype.UUID{Bytes: id, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrNoSuchDifference, id)
	case err != nil:
		return fmt.Errorf("%w: %s: %w", ErrNotResolved, id, err)
	case !row.ResolvedAt.Valid:
		// The update matched nothing and the row is open: it vanished and
		// came back, which 0015 does not allow. Reported as a failure
		// rather than as a decision.
		return fmt.Errorf("%w: %s: the row was open and the update matched nothing", ErrNotResolved, id)
	}
	return AlreadyResolvedError{
		ID:      id,
		By:      uuid.UUID(row.ResolvedBy.Bytes),
		Verdict: Verdict(row.Resolution.String),
		Reason:  row.ResolvedReason.String,
		At:      row.ResolvedAt.Time,
	}
}
