// Asking to be paid (T091, D9, FR-050, FR-051).
//
// A withdrawal request is one transaction that does four things in an order
// that is forced rather than chosen:
//
//  1. read the destination, narrowed on the member. Ownership and
//     verification are checked here so a member gets a 403 or a 409 rather
//     than a constraint violation - not INSTEAD of the database enforcing
//     them, which withdrawal_request_destination_is_the_members and
//     withdrawal_request_guard still do.
//  2. read and LOCK what the member has confirmed. This is what makes two
//     concurrent requests sequential: the second waits here and reads what
//     the first left behind.
//  3. reserve. D9 moves the money before any human reviews it, because the
//     double-spend window is between request and approval.
//  4. write the request, naming the transfer that already exists.
//
// Every check that can refuse comes before the ledger is asked for anything.
// That matters more here than anywhere else in this repository: the ledger is
// not in the transaction, so a posted transfer is not rolled back by a
// failing INSERT. A refusal after step 3 leaves money in a reserved account
// with no request naming it, which is the one outcome this file is arranged
// to avoid.

package payout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoWithdrawalStore reports a [Withdrawals] built without a database
	// to begin transactions on. A wiring mistake, refused at construction.
	ErrNoWithdrawalStore = errors.New("payout: no database handle to record withdrawals with")
	// ErrNoLedger reports one built without a ledger to reserve in. Without
	// it there is nothing to move money with, and a request that reserved
	// nothing could be approved twice over one balance.
	ErrNoLedger = errors.New("payout: withdrawing needs a ledger to reserve in")
	// ErrNoReceivable reports a deployment that has not named the house
	// account earnings are credited from (HOUSE_ACCOUNT_NETWORK_RECEIVABLE).
	//
	// Never defaulted - an account nobody named is one nobody meant to open,
	// and money credited from one would be money the business cannot find.
	// But discovered on REQUEST rather than at construction, for the reason
	// [ErrNoThreshold] is: production refuses to start without it, and in the
	// environments that can start without it the honest answer to a
	// withdrawal is 503 naming the key. Refusing to build would take the
	// wallet, the click-out and the operator queue down with it, over a
	// deployment that simply cannot pay out yet.
	ErrNoReceivable = errors.New("payout: this deployment has not named the account earnings are paid out of")
	// ErrNoThreshold reports a deployment that has not configured the
	// confirmed balance a withdrawal is checked against (FR-050).
	//
	// Discovered on request rather than at construction, exactly as
	// [wallet.ErrNoThreshold] is: nothing is broken, the deployment is
	// incomplete, and the endpoint answers 503 so whoever is paged can tell
	// the two apart.
	ErrNoThreshold = errors.New("payout: this deployment has not configured a withdrawal threshold")
	// ErrCurrencyNotPaid reports a withdrawal asked for in a currency this
	// deployment does not pay out in.
	//
	// The threshold states one currency (FR-050), and a request in another
	// has no threshold to be checked against. Refused rather than compared
	// numerically against a figure in a different currency, which is the
	// mistake C-6 exists to make impossible.
	ErrCurrencyNotPaid = errors.New("payout: this deployment does not pay out in that currency")
	// ErrNotRequested reports a withdrawal the database refused to record.
	// It wraps the refusal unchanged.
	ErrNotRequested = errors.New("payout: the withdrawal could not be recorded")
	// ErrWithdrawalNotFound reports a request this member does not have -
	// whether because it does not exist or because it is somebody else's,
	// deliberately one error for the reason [ErrDestinationNotFound] is.
	ErrWithdrawalNotFound = errors.New("payout: no such withdrawal request for this member")
)

// BelowThreshold reports a confirmed balance that has not reached the figure
// this deployment requires before a withdrawal may be asked for (FR-050, US4
// scenario 1).
//
// Distinct from [earnings.ShortConfirmedBalance], which is "you asked for
// more than you have". A member can be short of the threshold while holding
// more than they asked for, and telling them they have too little for a 10.00
// withdrawal when what is wrong is that payouts start at 25.00 would send
// them back to try 9.00.
type BelowThreshold struct {
	// Threshold is what confirmed must reach, Confirmed what it is, and
	// Short the difference - the figure that reaches the member.
	Threshold, Confirmed, Short money.Amount
}

func (e BelowThreshold) Error() string {
	return fmt.Sprintf("payout: withdrawals start at %s and confirmed balance is %s, %s short",
		e.Threshold, e.Confirmed, e.Short)
}

// State is where a withdrawal request stands, in the vocabulary
// withdrawal_request_state_known accepts.
type State string

const (
	// StateAwaitingApproval is a request whose money is already reserved and
	// which no human has decided yet (D9).
	StateAwaitingApproval State = "awaiting_approval"
	// StateApproved is a request a named operator released (C-4).
	StateApproved State = "approved"
	// StateRejected is a request an operator refused, with a reason.
	StateRejected State = "rejected"
	// StatePaid is a request whose payout settled.
	StatePaid State = "paid"
	// StateFailed is a request whose payout will never settle (FR-053).
	StateFailed State = "failed"
)

// String is the state as the schema spells it.
func (s State) String() string { return string(s) }

// Request is a member asking to be paid.
type Request struct {
	// Member is who is asking, taken from the token and never from the body.
	Member uuid.UUID
	// Destination is where they want the money, and must be theirs and
	// verified (FR-051).
	Destination uuid.UUID
	// Amount is what they asked for. What is actually reserved is at least
	// this and may exceed it, because entries are reserved whole - see
	// [earnings.Covering].
	Amount money.Amount
}

// Withdrawal is a request as the database holds it.
type Withdrawal struct {
	ID          uuid.UUID
	Member      uuid.UUID
	Destination uuid.UUID
	// Amount is what was RESERVED, which is what a payout will pay. It is
	// not necessarily what was asked for, and the endpoint returns it as
	// reserved_amount for exactly that reason.
	Amount      money.Amount
	State       State
	RequestedAt time.Time
	// ReservedTransfer is the ledger transfer that moved the money out of
	// confirmed when this request was made (D9). C-7 finds the entries this
	// request will pay by matching it.
	ReservedTransfer wallet.TransferRef
	// DecidedBy, DecidedAt and DecisionReason are the operator's decision,
	// and are zero until one is made.
	DecidedBy      uuid.UUID
	DecidedAt      time.Time
	DecisionReason string
}

// Beginner is a database handle that can start a transaction. A withdrawal
// needs one: the reservation, every entry it moved and the request naming it
// are one commit or none.
type Beginner interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Withdrawals records what members ask to be paid.
//
// It holds the ledger and the receivable account rather than a built
// [earnings.Entries], because the entry machine is bound to a store and the
// store here must be bound to THIS request's transaction - the lock
// Confirmed takes and the writes Reserve makes have to be the same one. So
// the machine is built per request, from parts that are wiring.
type Withdrawals struct {
	db         Beginner
	ledger     wallet.Ledger
	receivable string
	threshold  money.Amount
}

// NewWithdrawals builds the service. The threshold may be the zero Amount
// and the receivable may be blank - see [ErrNoThreshold] and
// [ErrNoReceivable] for why an incomplete deployment answers 503 on the
// endpoint rather than failing to start.
func NewWithdrawals(db Beginner, ledger wallet.Ledger, receivable string, threshold money.Amount) (*Withdrawals, error) {
	switch {
	case db == nil:
		return nil, ErrNoWithdrawalStore
	case ledger == nil:
		return nil, ErrNoLedger
	}
	return &Withdrawals{db: db, ledger: ledger, receivable: receivable, threshold: threshold}, nil
}

// Request reserves the money and records the ask.
//
// The request's id is minted here rather than defaulted by the database, and
// that is the one thing about this function that looks odd. It follows from
// two facts that meet: reserved_transfer_ref is NOT NULL, so the transfer
// must exist before the row; and the transfer's idempotency key is derived
// from the request (D8), so the request's identity must exist before the
// transfer. Minting the id is the only thing that satisfies both.
func (w *Withdrawals) Request(ctx context.Context, req Request) (Withdrawal, error) {
	if err := w.acceptable(req); err != nil {
		return Withdrawal{}, err
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("%w: %w", ErrNotRequested, err)
	}
	// A rollback after a successful commit is a no-op, so every early return
	// below leaves the database as it found it.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := w.payable(ctx, tx, req); err != nil {
		return Withdrawal{}, err
	}

	entries := earningsstore.New(tx)
	machine, err := earnings.NewEntries(entries, w.ledger, w.receivable)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("%w: %w", ErrNotRequested, err)
	}
	confirmed, err := machine.Confirmed(ctx, entries, req.Member, req.Amount.Currency)
	if err != nil {
		return Withdrawal{}, err
	}
	if err := w.reachesTheThreshold(confirmed); err != nil {
		return Withdrawal{}, err
	}
	taking, _, err := earnings.Covering(confirmed, req.Amount)
	if err != nil {
		return Withdrawal{}, err
	}

	// Past this line the ledger has been touched and the transaction can no
	// longer undo it, which is why every refusal above is above it.
	id := uuid.New()
	reserved, err := machine.Reserve(ctx, tx, entries, taking, id)
	if err != nil {
		return Withdrawal{}, err
	}

	row, err := store.New(tx).CreateWithdrawalRequest(ctx, store.CreateWithdrawalRequestParams{
		ID:                  pgtype.UUID{Bytes: id, Valid: true},
		AccountID:           pgtype.UUID{Bytes: req.Member, Valid: true},
		DestinationID:       pgtype.UUID{Bytes: req.Destination, Valid: true},
		AmountMinor:         reserved.Amount.Minor,
		Currency:            string(reserved.Amount.Currency),
		ReservedTransferRef: string(reserved.Transfer),
	})
	if err != nil {
		return Withdrawal{}, fmt.Errorf("%w: %s for member %s: %w", ErrNotRequested, id, req.Member, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, fmt.Errorf("%w: %s: %w", ErrNotRequested, id, err)
	}
	return withdrawalFrom(row)
}

// acceptable refuses what can be refused without reading anything.
func (w *Withdrawals) acceptable(req Request) error {
	switch {
	case req.Member == uuid.Nil:
		return fmt.Errorf("%w: no member is asking", ErrNotRequested)
	case req.Destination == uuid.Nil:
		return fmt.Errorf("%w: no destination to pay", ErrNotRequested)
	}
	if w.receivable == "" {
		return ErrNoReceivable
	}
	if !w.threshold.Currency.Valid() {
		return ErrNoThreshold
	}
	if err := req.Amount.Validate(); err != nil {
		return fmt.Errorf("%w: %w", earnings.ErrNothingToReserve, err)
	}
	if !req.Amount.IsPositive() {
		return fmt.Errorf("%w: %s", earnings.ErrNothingToReserve, req.Amount)
	}
	if req.Amount.Currency != w.threshold.Currency {
		return fmt.Errorf("%w: %s was asked for and payouts are in %s",
			ErrCurrencyNotPaid, req.Amount.Currency, w.threshold.Currency)
	}
	return nil
}

// payable reads the destination inside the caller's transaction and refuses
// one that is not this member's or not verified.
//
// Both refusals are the database's too - the composite foreign key and
// withdrawal_request_guard - and this is not a substitute for either. It runs
// first so a member reads "that destination is not verified" instead of a
// constraint name, and so the refusal happens before the ledger is asked to
// move anything.
func (w *Withdrawals) payable(ctx context.Context, tx pgx.Tx, req Request) error {
	destinations, err := NewDestinations(tx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotRequested, err)
	}
	destination, err := destinations.Get(ctx, req.Member, req.Destination)
	if err != nil {
		return err
	}
	if !destination.Verified() {
		return fmt.Errorf("%w: %s", ErrDestinationNotVerified, destination.ID)
	}
	return nil
}

// reachesTheThreshold refuses a member whose confirmed balance has not
// reached the figure withdrawals start at (FR-050).
//
// Computed from the entries the locked read returned rather than from the
// ledger, and the two must agree: a projection that disagreed with the
// entries behind it would be the disagreement D7 exists to prevent. Using the
// locked set is what makes this check and the reservation see one balance.
func (w *Withdrawals) reachesTheThreshold(confirmed []earnings.Entry) error {
	total, err := earnings.Total(confirmed, w.threshold.Currency)
	if err != nil {
		return err
	}
	if total.Minor >= w.threshold.Minor {
		return nil
	}
	short, err := w.threshold.Sub(total)
	if err != nil {
		return err
	}
	return BelowThreshold{Threshold: w.threshold, Confirmed: total, Short: short}
}

// Get answers one request, if it is this member's.
func (w *Withdrawals) Get(ctx context.Context, member, id uuid.UUID) (Withdrawal, error) {
	if member == uuid.Nil || id == uuid.Nil {
		return Withdrawal{}, fmt.Errorf("%w: a request is read for a member, by id", ErrWithdrawalNotFound)
	}
	row, err := store.New(w.db).GetWithdrawalRequestForAccount(ctx, store.GetWithdrawalRequestForAccountParams{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		AccountID: pgtype.UUID{Bytes: member, Valid: true},
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Withdrawal{}, fmt.Errorf("%w: %s", ErrWithdrawalNotFound, id)
	case err != nil:
		return Withdrawal{}, fmt.Errorf("payout: reading withdrawal %s for member %s: %w", id, member, err)
	}
	return withdrawalFrom(row)
}

// List answers every request this member has made, newest first.
func (w *Withdrawals) List(ctx context.Context, member uuid.UUID) ([]Withdrawal, error) {
	if member == uuid.Nil {
		return nil, fmt.Errorf("%w: no member to list withdrawals for", ErrWithdrawalNotFound)
	}
	rows, err := store.New(w.db).ListWithdrawalRequestsForAccount(ctx, pgtype.UUID{Bytes: member, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("payout: listing withdrawals for member %s: %w", member, err)
	}
	withdrawals := make([]Withdrawal, 0, len(rows))
	for _, row := range rows {
		withdrawal, err := withdrawalFrom(row)
		if err != nil {
			return nil, err
		}
		withdrawals = append(withdrawals, withdrawal)
	}
	return withdrawals, nil
}

// withdrawalFrom turns one stored row into the value a caller reads.
//
// The amount is built through [money.New] rather than assembled, because a
// row is only ever as good as the currency beside it and this is the last
// place that can be checked before somebody decides money on it (C-6).
func withdrawalFrom(row store.CashbackWithdrawalRequest) (Withdrawal, error) {
	amount, err := money.New(row.AmountMinor, money.Currency(row.Currency))
	if err != nil {
		return Withdrawal{}, fmt.Errorf("payout: withdrawal %v: %w", row.ID, err)
	}
	return Withdrawal{
		ID:               uuid.UUID(row.ID.Bytes),
		Member:           uuid.UUID(row.AccountID.Bytes),
		Destination:      uuid.UUID(row.DestinationID.Bytes),
		Amount:           amount,
		State:            State(row.State),
		RequestedAt:      row.RequestedAt.Time,
		ReservedTransfer: wallet.TransferRef(row.ReservedTransferRef),
		DecidedBy:        uuid.UUID(row.DecidedBy.Bytes),
		DecidedAt:        row.DecidedAt.Time,
		DecisionReason:   row.DecisionReason.String,
	}, nil
}
