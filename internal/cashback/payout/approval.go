// Approving a withdrawal, which is where money actually leaves (T092, C-4,
// C-5, D8).
//
// TWO TRANSACTIONS, and the split is the whole design rather than an
// accident of structure.
//
// The first writes the decision and the payout row and COMMITS. The payout
// row is the approval (C-4) and its idempotency_key is generated from the
// request (C-5, D8), so committing it is what CLAIMS the key - before the
// rail has been contacted, before any money can move. A second operator
// approving the same request then loses on a unique violation rather than
// making a second payment.
//
// The rail is contacted after that commit, outside any transaction. Doing it
// inside one would mean a rollback could erase a payout row while the rail
// held the payment: the money would be gone and nothing in this database
// would say so. The reverse - a committed payout the rail never received -
// is recoverable, and is exactly the state a retry picks up (FR-053): the
// row is there, its key is fixed, and re-submitting under it either finds the
// earlier submission or makes the only one there will be.
//
// The second transaction records what the rail gave back. If the process
// dies before it, the payout sits submitted with no reference, which is the
// same recoverable state.

package payout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoRail reports an approver built with no rail to pay through.
	// Refused at construction: discovered later, it is discovered with a
	// request already moved out of the approval queue.
	ErrNoRail = errors.New("payout: approving needs a rail to pay through")
	// ErrNoDescriptor reports one built with nothing to put on a member's
	// statement (FR-070, FR-073). Not cosmetic: a payment a member cannot
	// recognise is a payment they charge back.
	ErrNoDescriptor = errors.New("payout: approving needs the descriptor a member reads on their statement")
	// ErrNoSuchWithdrawal reports an id that names no request. Distinct
	// from a request that cannot be approved: one is a wrong id and the
	// other is a decision somebody already made.
	ErrNoSuchWithdrawal = errors.New("payout: no such withdrawal request")
	// ErrNotAwaitingApproval reports a request that has already been
	// decided. Ordinary under two operators working one queue, and never a
	// defect: it is the second one being told, before the rail is touched.
	ErrNotAwaitingApproval = errors.New("payout: this withdrawal is no longer awaiting approval")
	// ErrAlreadyApproved reports a request that already has a payout. It is
	// C-5 doing its job: the unique key refused the second approval, and
	// this is what that refusal is called on the way out.
	ErrAlreadyApproved = errors.New("payout: this withdrawal has already been approved and paid")
	// ErrNotApproved reports an approval the database refused. It wraps the
	// refusal unchanged - including payout_insert_guard's, which is how an
	// approver without the operator role is told (C-4).
	ErrNotApproved = errors.New("payout: the approval could not be recorded")
	// ErrBrandUnresolved reports a request whose reserved entries name no
	// brand, or more than one.
	//
	// Neither is payable as one payout. No brand means the reservation's
	// entries cannot be found from its transfer, which is C-7's own join
	// failing and a far larger problem than this request. More than one
	// means the member earned under two brands, and payout.brand_id is a
	// single frozen column: picking either would attribute half the money
	// to a brand that did not earn it (ADR-0004).
	ErrBrandUnresolved = errors.New("payout: the brand this withdrawal pays out for cannot be resolved from its reserved entries")
)

// Payout is one outbound money movement as the database holds it.
type Payout struct {
	ID      uuid.UUID
	Request uuid.UUID
	Brand   string
	// ApprovedBy is the named human this payment rests on (C-4). Never the
	// zero uuid: the column is NOT NULL, so a payout without one cannot
	// exist to be read.
	ApprovedBy uuid.UUID
	// IdempotencyKey is the database's own derivation from the request
	// (C-5, D8), read back rather than computed here. It is what the rail
	// was given, and what a retry must give it again.
	IdempotencyKey string
	Amount         money.Amount
	Rail           Kind
	// RailReference is what the rail called this payment, and is empty
	// until the rail has answered. An empty one on a submitted payout is
	// the recoverable state a retry picks up, not a defect.
	RailReference string
	State         RailStatus
	SubmittedAt   time.Time
	SettledAt     time.Time
}

// Approval is one operator releasing one withdrawal.
type Approval struct {
	// Request is the withdrawal being approved.
	Request uuid.UUID
	// Operator is the named human approving it, taken from the token
	// subject and never from the body (C-4). The database checks the role
	// behind it in payout_insert_guard, with a locking read.
	Operator uuid.UUID
}

// Approvals releases withdrawals through a rail.
type Approvals struct {
	db         Beginner
	rail       Rail
	descriptor string
}

// NewApprovals builds the approver, refusing one that is missing a part.
func NewApprovals(db Beginner, rail Rail, descriptor string) (*Approvals, error) {
	switch {
	case db == nil:
		return nil, ErrNoWithdrawalStore
	case rail == nil:
		return nil, ErrNoRail
	case descriptor == "":
		return nil, ErrNoDescriptor
	}
	return &Approvals{db: db, rail: rail, descriptor: descriptor}, nil
}

// Approve records the decision, claims the idempotency key, and submits.
//
// The rail's answer does not decide whether the approval happened. By the
// time it is asked, the decision and the payout row are committed - so a
// rail failure is reported to the operator AND the payout stands, waiting
// for the retry that carries it (FR-053). Rolling the approval back on a
// rail timeout would be the one mistake this whole arrangement exists to
// prevent: the payment may already be in flight.
func (a *Approvals) Approve(ctx context.Context, approval Approval) (Payout, error) {
	if approval.Request == uuid.Nil {
		return Payout{}, fmt.Errorf("%w: no withdrawal to approve", ErrNotApproved)
	}
	if approval.Operator == uuid.Nil {
		// C-4 in the one place a caller could get it wrong. The database
		// would refuse it too - approved_by is NOT NULL - but a request
		// that reached here anonymously is a bug in the gate above, and it
		// should not be reported as a constraint violation.
		return Payout{}, fmt.Errorf("%w: no operator is approving it", ErrNotApproved)
	}

	written, instruction, err := a.claim(ctx, approval)
	if err != nil {
		return Payout{}, err
	}

	// Past this line the key is claimed and committed. Everything that
	// follows can fail without costing correctness: the payout stands and
	// the retry re-submits under the same key.
	reference, err := a.rail.Submit(ctx, instruction)
	if err != nil {
		return written, fmt.Errorf("payout: submitting %s to the %s rail: %w",
			written.IdempotencyKey, a.rail.Kind(), err)
	}
	return a.recordSubmission(ctx, written, reference)
}

// claim runs the first transaction: decide, write the payout, commit.
func (a *Approvals) claim(ctx context.Context, approval Approval) (Payout, Instruction, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Payout{}, Instruction{}, fmt.Errorf("%w: %w", ErrNotApproved, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	request, err := awaitingDecision(ctx, queries, approval.Request)
	if err != nil {
		return Payout{}, Instruction{}, err
	}
	brand, err := a.brandOf(ctx, queries, request)
	if err != nil {
		return Payout{}, Instruction{}, err
	}
	destination, err := a.destinationOf(ctx, tx, request)
	if err != nil {
		return Payout{}, Instruction{}, err
	}

	if _, err := queries.RecordWithdrawalDecision(ctx, store.RecordWithdrawalDecisionParams{
		ID:        pgtype.UUID{Bytes: request.ID, Valid: true},
		FromState: string(StateAwaitingApproval),
		ToState:   string(StateApproved),
		DecidedBy: pgtype.UUID{Bytes: approval.Operator, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The row moved between the locking read and here, which the
			// lock should have made impossible. Reported rather than
			// retried: something else is writing this table.
			return Payout{}, Instruction{}, fmt.Errorf("%w: %s", ErrNotAwaitingApproval, request.ID)
		}
		return Payout{}, Instruction{}, fmt.Errorf("%w: deciding %s: %w", ErrNotApproved, request.ID, err)
	}

	row, err := queries.CreatePayout(ctx, store.CreatePayoutParams{
		RequestID:   pgtype.UUID{Bytes: request.ID, Valid: true},
		BrandID:     brand,
		ApprovedBy:  pgtype.UUID{Bytes: approval.Operator, Valid: true},
		AmountMinor: request.Amount.Minor,
		Currency:    string(request.Amount.Currency),
		Rail:        a.rail.Kind().String(),
	})
	if err != nil {
		return Payout{}, Instruction{}, a.explainCreate(ctx, request.ID, err)
	}
	written, err := payoutFrom(row)
	if err != nil {
		return Payout{}, Instruction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payout{}, Instruction{}, fmt.Errorf("%w: %s: %w", ErrNotApproved, request.ID, err)
	}

	return written, Instruction{
		// The database's derivation, read back from the row it generated
		// (D8). Computing the same string here would be a second authority
		// on the one thing C-5 rests on.
		IdempotencyKey: written.IdempotencyKey,
		Amount:         written.Amount,
		Destination:    destination.ToRef(),
		Descriptor:     a.descriptor,
	}, nil
}

// brandOf answers which brand the reserved entries were earned under,
// refusing a request that names none or more than one.
func (a *Approvals) brandOf(ctx context.Context, queries *store.Queries, request Withdrawal) (string, error) {
	brands, err := queries.BrandOfReservedEntries(ctx, string(request.ReservedTransfer))
	if err != nil {
		return "", fmt.Errorf("%w: reading the brand of %s: %w", ErrNotApproved, request.ID, err)
	}
	if len(brands) != 1 {
		return "", fmt.Errorf("%w: %s reserved entries naming %d brand(s)", ErrBrandUnresolved, request.ID, len(brands))
	}
	return brands[0], nil
}

// destinationOf reads where the money goes, inside the same transaction.
func (a *Approvals) destinationOf(ctx context.Context, tx pgx.Tx, request Withdrawal) (Destination, error) {
	destinations, err := NewDestinations(tx)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: %w", ErrNotApproved, err)
	}
	destination, err := destinations.Get(ctx, request.Member, request.Destination)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: %w", ErrNotApproved, err)
	}
	if destination.Kind != a.rail.Kind() {
		// The rail records itself in payout.rail and payout_guard freezes
		// it, so a mismatch here would be a payment permanently labelled as
		// carried by a rail that never saw it.
		return Destination{}, fmt.Errorf("%w: %s is a %s destination and this deployment pays through %s",
			ErrNotApproved, request.ID, destination.Kind, a.rail.Kind())
	}
	if !destination.Verified() {
		// Already refused when the request was made (FR-051), and checked
		// again because approval is a later moment: this is the last read
		// before money leaves.
		return Destination{}, fmt.Errorf("%w: %w", ErrNotApproved, ErrDestinationNotVerified)
	}
	return destination, nil
}

// explainCreate turns the unique violation C-5 raises into the answer an
// operator can act on, and leaves anything else as it found it.
func (a *Approvals) explainCreate(ctx context.Context, request uuid.UUID, cause error) error {
	var violation *pgconn.PgError
	if !errors.As(cause, &violation) || violation.Code != uniqueViolation {
		return fmt.Errorf("%w: paying %s: %w", ErrNotApproved, request, cause)
	}
	// A separate connection, because the transaction that raised this is
	// aborted and can answer nothing further.
	existing, err := store.New(a.db).GetPayoutForRequest(ctx, pgtype.UUID{Bytes: request, Valid: true})
	if err != nil {
		return fmt.Errorf("%w: %s", ErrAlreadyApproved, request)
	}
	return fmt.Errorf("%w: %s is payout %s", ErrAlreadyApproved, request, uuid.UUID(existing.ID.Bytes))
}

// uniqueViolation is Postgres's SQLSTATE for a unique or primary-key
// violation. Named because C-5's whole defence is that this code, and not a
// second payment, is what a double approval produces.
const uniqueViolation = "23505"

// recordSubmission runs the second transaction: store what the rail called
// the payment.
//
// A failure here is reported with the payout as it stands, not as an
// approval that did not happen: the money has been submitted, and an
// operator told otherwise would submit it again.
func (a *Approvals) recordSubmission(ctx context.Context, written Payout, reference RailReference) (Payout, error) {
	row, err := store.New(a.db).RecordPayoutSubmission(ctx, store.RecordPayoutSubmissionParams{
		ID:            pgtype.UUID{Bytes: written.ID, Valid: true},
		RailReference: pgtype.Text{String: reference.Ref(), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already carries a reference: a concurrent retry recorded the
			// rail's answer first. Nothing is wrong - the key made both
			// submissions the same payment - so the payout is read back
			// rather than reported as a failure.
			return a.reread(ctx, written)
		}
		return written, fmt.Errorf("payout: recording the rail reference for %s: %w", written.ID, err)
	}
	return payoutFrom(row)
}

// reread answers the payout as it now stands.
func (a *Approvals) reread(ctx context.Context, written Payout) (Payout, error) {
	row, err := store.New(a.db).GetPayoutForRequest(ctx, pgtype.UUID{Bytes: written.Request, Valid: true})
	if err != nil {
		return written, fmt.Errorf("payout: re-reading %s: %w", written.ID, err)
	}
	return payoutFrom(row)
}

// payoutFrom turns one stored row into the value a caller reads.
//
// The amount is built through [money.New] rather than assembled, because a
// row is only ever as good as the currency beside it and this is the last
// place that can be checked before somebody decides money on it (C-6).
func payoutFrom(row store.CashbackPayout) (Payout, error) {
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		return Payout{}, fmt.Errorf("payout: payout %v: %w", row.ID, err)
	}
	kind, err := ParseKind(row.Rail)
	if err != nil {
		return Payout{}, fmt.Errorf("payout: payout %v: %w", row.ID, err)
	}
	return Payout{
		ID:             uuid.UUID(row.ID.Bytes),
		Request:        uuid.UUID(row.RequestID.Bytes),
		Brand:          row.BrandID,
		ApprovedBy:     uuid.UUID(row.ApprovedBy.Bytes),
		IdempotencyKey: row.IdempotencyKey,
		Amount:         amount,
		Rail:           kind,
		RailReference:  row.RailReference.String,
		State:          RailStatus(row.State),
		SubmittedAt:    row.SubmittedAt.Time,
		SettledAt:      row.SettledAt.Time,
	}, nil
}
