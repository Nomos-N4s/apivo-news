// Reviewing a held credit: the two decisions only a human may take (T119,
// US7 scenario 3, FR-061).
//
// A hold is the rules saying "not yet" (holdrules.go); this is the person
// saying yes or no. Releasing moves the credit to pending and the ordinary
// lifecycle resumes - it confirms nothing, because an operator can only
// assert that the credit is ordinary, not that the network paid (state.go).
// Rejecting undoes the credit the way a network reversal does: a reversing
// entry beside it, born reversed, carrying the money back to the receivable
// (SC-010), with the operator as its cause and their reason on its opening
// transition. The credit's own row is never edited by either.
//
// Both record a named human and a non-blank reason in the same transaction
// as the effect, and announce what they did beside it (FR-061): an audit
// record written afterwards is a record of what somebody remembered.

package earnings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// MaxReviewReasonRunes bounds a recorded reason. A paragraph rather than a
// form field: the reason is the audit record, and an operator cut off
// mid-sentence writes a worse one. The schema's own rule is only that it is
// not blank.
const MaxReviewReasonRunes = 2000

var (
	// ErrInvalidReview reports a review that cannot be recorded as asked:
	// naming no entry, taken by nobody, or giving no reason. The text after
	// it names the field.
	ErrInvalidReview = errors.New("earnings: the review cannot be recorded")
	// ErrNoSuchEntry reports an id that names no entry. Entries are never
	// deleted, so this id was never issued.
	ErrNoSuchEntry = errors.New("earnings: no such entry")
	// ErrNotHeld reports a review of a credit that is not held. Wrapped by
	// [NotHeldError], which says what it is instead.
	ErrNotHeld = errors.New("earnings: the credit is not held")
	// ErrAlreadyRejected reports a review of a credit an operator has
	// already rejected: a reversing entry undoes it, and the money has gone
	// back. The credit's own row still reads held, because a rejection
	// edits nothing (SC-010), which is why this is asked separately.
	ErrAlreadyRejected = errors.New("earnings: the credit has already been rejected")
	// ErrNotReviewed reports a review the database refused for any other
	// reason.
	ErrNotReviewed = errors.New("earnings: the review could not be recorded")
)

// NotHeldError reports a credit that exists and is not held, naming the
// state it is in: the difference between an operator retrying and an
// operator reloading.
type NotHeldError struct {
	ID    uuid.UUID
	State State
}

func (e NotHeldError) Error() string {
	return fmt.Sprintf("earnings: credit %s is %s, not held", e.ID, e.State)
}

// Unwrap makes the verdict matchable without knowing this type.
func (e NotHeldError) Unwrap() error { return ErrNotHeld }

// Review is one operator's decision on a held credit: which, by whom, why.
type Review struct {
	// Entry is the held credit.
	Entry uuid.UUID
	// Operator is the authenticated caller, recorded as the transition's
	// actor. Never from a request body (C-4).
	Operator uuid.UUID
	// Reason is why. Trimmed, never blank (FR-061).
	Reason string
}

// Validate refuses a review that cannot be recorded, before anything is
// read.
func (r Review) Validate() error {
	reason := strings.TrimSpace(r.Reason)
	switch {
	case r.Entry == uuid.Nil:
		return fmt.Errorf("%w: it names no entry", ErrInvalidReview)
	case r.Operator == uuid.Nil:
		return fmt.Errorf("%w: nobody is deciding it", ErrInvalidReview)
	case reason == "":
		return fmt.Errorf("%w: a review records why (FR-061): supply a non-blank reason", ErrInvalidReview)
	case len([]rune(reason)) > MaxReviewReasonRunes:
		return fmt.Errorf("%w: the reason is longer than %d characters", ErrInvalidReview, MaxReviewReasonRunes)
	}
	return nil
}

// HeldAfter is a keyset position in the held queue: the page continues
// after the credit held since HeldSince with this ID. The zero value is the
// start.
type HeldAfter struct {
	HeldSince time.Time
	ID        uuid.UUID
}

// HeldCredit is one row of the queue as an operator reads it: the credit,
// the rule that held it and what the rule said, and what the network
// reported, so the decision is made from one screen.
type HeldCredit struct {
	ID     uuid.UUID
	Member uuid.UUID
	Brand  string
	Report uuid.UUID
	// Click is the zero uuid where an operator attributed the purchase by
	// hand.
	Click uuid.UUID
	// Rule is what held it (entry.hold_rule) and Reason what the rule
	// wrote on the opening transition; Reason is empty where a hold was
	// recorded with none.
	Rule   string
	Reason string
	// Amount is the member's share, sitting in the held stage.
	Amount    money.Amount
	HeldSince time.Time
	// The report, in the network's own terms.
	Network      networks.NetworkID
	ExternalID   string
	ReportStatus networks.Status
	Sale         money.Amount
	Commission   money.Amount
	TransactedAt time.Time
}

// After is this row's position, for the cursor that continues past it.
func (h HeldCredit) After() HeldAfter { return HeldAfter{HeldSince: h.HeldSince, ID: h.ID} }

// Released is what a release did, read back from the transition that
// recorded it.
type Released struct {
	// Entry is the credit as it now stands: pending, under no rule.
	Entry Entry
	// Rule is the rule the credit was held under, which the row no longer
	// carries and the announcement does.
	Rule string
	// ReleasedBy and Reason are the transition's actor and reason; Transfer
	// the posting that moved the money to the pending stage; At the row's
	// own instant.
	ReleasedBy uuid.UUID
	Reason     string
	Transfer   wallet.TransferRef
	At         time.Time
}

// Rejected is what a rejection did: the credit, left exactly as it was, and
// the reversing entry that undoes it.
type Rejected struct {
	Credit   Entry
	Reversal Entry
	// Rule is the rule the credit was held under.
	Rule string
	// RejectedBy and Reason are what the reversal's opening transition
	// recorded; At is the reversing row's own instant.
	RejectedBy uuid.UUID
	Reason     string
	At         time.Time
}

// Reviews decides held credits.
type Reviews struct {
	db         Beginner
	ledger     wallet.Ledger
	receivable string
	announcer  *Announcer
}

// NewReviews builds the service. The receivable may be blank, and is then
// refused at the first decision rather than here, with [ErrNoReceivable]:
// the operator queue stays mounted in a deployment that cannot move money
// yet, and answers on that route what it lacks.
func NewReviews(db Beginner, ledger wallet.Ledger, receivable string) (*Reviews, error) {
	switch {
	case db == nil:
		return nil, ErrNoEntryStore
	case ledger == nil:
		return nil, ErrNoLedger
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &Reviews{db: db, ledger: ledger, receivable: receivable, announcer: announcer}, nil
}

// Held returns one page of the queue, oldest first, starting after the
// given position: every held credit nothing has yet decided.
func (r *Reviews) Held(ctx context.Context, after HeldAfter, limit int) ([]HeldCredit, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: a page of %d rows", ErrNotReviewed, limit)
	}
	rows, err := store.New(r.db).ListHeldEntries(ctx, store.ListHeldEntriesParams{
		AfterCreatedAt: pgtype.Timestamptz{Time: after.HeldSince, Valid: true},
		AfterID:        pgUUID(after.ID),
		PageSize:       int32(min(limit, 1000)), //nolint:gosec // bounded just above
	})
	if err != nil {
		return nil, fmt.Errorf("%w: reading the held queue: %w", ErrNotReviewed, err)
	}
	queue := make([]HeldCredit, 0, len(rows))
	for _, row := range rows {
		credit, err := heldCredit(row)
		if err != nil {
			return nil, err
		}
		queue = append(queue, credit)
	}
	return queue, nil
}

// heldCredit reads one row into the queue's shape, building every amount
// through [money.New] for the reason entryFrom does (C-6).
func heldCredit(row store.ListHeldEntriesRow) (HeldCredit, error) {
	id := uuid.UUID(row.ID.Bytes)
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		return HeldCredit{}, fmt.Errorf("%w: entry %s: %w", ErrNotReviewed, id, err)
	}
	sale, err := money.New(row.SaleAmountMinor, money.Currency(row.Currency))
	if err != nil {
		return HeldCredit{}, fmt.Errorf("%w: entry %s: the reported sale: %w", ErrNotReviewed, id, err)
	}
	commission, err := money.New(row.CommissionMinor, money.Currency(row.Currency))
	if err != nil {
		return HeldCredit{}, fmt.Errorf("%w: entry %s: the reported commission: %w", ErrNotReviewed, id, err)
	}
	return HeldCredit{
		ID:           id,
		Member:       uuid.UUID(row.AccountID.Bytes),
		Brand:        row.BrandID,
		Report:       uuid.UUID(row.NetworkTransactionID.Bytes),
		Click:        uuid.UUID(row.ClickID.Bytes),
		Rule:         row.HoldRule.String,
		Reason:       row.HoldReason,
		Amount:       amount,
		HeldSince:    row.CreatedAt.Time,
		Network:      networks.NetworkID(row.NetworkID),
		ExternalID:   row.ExternalID,
		ReportStatus: networks.Status(row.ReportStatus),
		Sale:         sale,
		Commission:   commission,
		TransactedAt: row.TransactedAt.Time,
	}, nil
}

// Release moves a held credit to pending, recording who and why.
//
// The posting key is derived from the transition that held the credit (D8):
// an entry can be held more than once in its life, and a key naming only
// the entry would make the second release a replay of the first, which the
// ledger would answer by moving nothing while the row moved.
func (r *Reviews) Release(ctx context.Context, review Review) (Released, error) {
	if err := review.Validate(); err != nil {
		return Released{}, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Released{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, review.Entry, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	credit, err := heldForDecision(ctx, queries, review.Entry)
	if err != nil {
		return Released{}, err
	}
	cause, err := queries.HeldTransition(ctx, pgUUID(credit.ID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// D7 forbids this: a held entry with no transition into held
			// is a state recorded without its posting.
			return Released{}, fmt.Errorf("%w: %s is held and no transition held it", ErrNotReviewed, credit.ID)
		}
		return Released{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, credit.ID, err)
	}
	entries, err := NewEntries(queries, r.ledger, r.receivable)
	if err != nil {
		return Released{}, err
	}
	// The transition comes back with the move, read from the row it wrote:
	// what is announced is what the database holds - the actor, the reason
	// and the instant included.
	released, recorded, err := entries.apply(ctx, tx, Move{
		Entry:  credit.ID,
		From:   StateHeld,
		To:     StatePending,
		Member: credit.Member,
		Amount: credit.Amount,
		Cause:  uuid.UUID(cause.Bytes),
		Reason: strings.TrimSpace(review.Reason),
		Actor:  review.Operator,
	})
	if err != nil {
		return Released{}, err
	}
	done := Released{
		Entry:      released,
		Rule:       credit.HoldRule,
		ReleasedBy: recorded.Actor,
		Reason:     recorded.Reason,
		Transfer:   recorded.Transfer,
		At:         recorded.At,
	}
	if err := r.announcer.HoldReleased(ctx, tx, done); err != nil {
		return Released{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Released{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, credit.ID, err)
	}
	return done, nil
}

// Reject undoes a held credit: a reversing entry citing the credit's own
// report, with the operator as its cause, and the money back to the
// receivable. The credit's own row is left exactly as it was (SC-010): it
// still reads held, and the reversing entry beside it is what says the
// decision was taken.
func (r *Reviews) Reject(ctx context.Context, review Review) (Rejected, error) {
	if err := review.Validate(); err != nil {
		return Rejected{}, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Rejected{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, review.Entry, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	credit, err := heldForDecision(ctx, queries, review.Entry)
	if err != nil {
		return Rejected{}, err
	}
	entries, err := NewEntries(queries, r.ledger, r.receivable)
	if err != nil {
		return Rejected{}, err
	}
	reversals, err := NewReversals(entries)
	if err != nil {
		return Rejected{}, err
	}
	reversing, recorded, err := reversals.reverse(ctx, tx, Reversal{
		Original: credit,
		Report:   credit.Report,
		Reason:   strings.TrimSpace(review.Reason),
		Actor:    review.Operator,
	})
	if err != nil {
		return Rejected{}, err
	}
	done := Rejected{
		Credit:     credit,
		Reversal:   reversing,
		Rule:       credit.HoldRule,
		RejectedBy: recorded.Actor,
		Reason:     recorded.Reason,
		At:         recorded.At,
	}
	if err := r.announcer.HoldRejected(ctx, tx, done); err != nil {
		return Rejected{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Rejected{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, credit.ID, err)
	}
	return done, nil
}

// heldForDecision reads and locks the credit a decision is about, and
// refuses one that is not an operator's to decide: unknown, not held, or
// already rejected.
//
// Locked, so two operators deciding one credit at the same instant
// serialise here and the second reads what the first did - a release moved
// it out of held, a rejection put a reversal beside it - and is told so.
func heldForDecision(ctx context.Context, queries *store.Queries, id uuid.UUID) (Entry, error) {
	row, err := queries.LockEntry(ctx, pgUUID(id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Entry{}, fmt.Errorf("%w: %s", ErrNoSuchEntry, id)
	case err != nil:
		return Entry{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, id, err)
	}
	credit, err := entryFrom(row)
	if err != nil {
		return Entry{}, err
	}
	if credit.State != StateHeld {
		return Entry{}, NotHeldError{ID: credit.ID, State: credit.State}
	}
	switch reversal, err := queries.ReversalOf(ctx, pgUUID(credit.ID)); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Entry{}, fmt.Errorf("%w: %s: %w", ErrNotReviewed, id, err)
	default:
		return Entry{}, fmt.Errorf("%w: %s is undone by %s", ErrAlreadyRejected, credit.ID, uuid.UUID(reversal.Bytes))
	}
	return credit, nil
}
