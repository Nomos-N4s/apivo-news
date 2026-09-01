// Closing the money loop (T146, FR-053).
//
// Approving submits a payment; retrying re-sends one; refusing and abandoning
// put the money back. None of those observes a payment ARRIVING, and until
// something does, cashback.payout has a settled state nothing can reach,
// cashback.withdrawal_request has a paid state nothing can reach, and the
// paid_out figure on a member's wallet - which is read from settled payouts -
// reports zero for every member forever. Phase 6's checkpoint says the money
// loop closes; this is the half that closes it.
//
// The rail is the only thing that knows. A bank tells its customer a transfer
// landed; nothing tells this service. So this asks, on a schedule, about
// every payment the rail has taken and not finished.
//
// What it does NOT do is decide anything. A settlement is an observation, and
// the two writes it makes are shaped by that: the payout gets its terminal
// state and its instant, and the request moves to paid without touching the
// approver or their timestamp. The one human decision in this chain stays
// exactly as they left it.

package payout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

const (
	// SettlementJobName identifies the sweep in the scheduler, and names the
	// fleet-wide lock that stops two instances asking the rail about the
	// same payments at once.
	SettlementJobName = "cashback-payout-settlement"
	// SettlementInterval is how long a member may wait after their money
	// has actually arrived before this service knows. Minutes rather than
	// seconds: settlement is a bank's timescale, and asking a rail every
	// few seconds about payments that take hours is a rate limit spent on
	// nothing.
	SettlementInterval = 5 * time.Minute
	// settlementTimeout bounds one sweep. Every payment in it costs a call
	// to a rail, so the bound is generous - but it exists, because a rail
	// that stops answering must not hold the lock until the process dies.
	settlementTimeout = 10 * time.Minute
)

var (
	// ErrNotSettled reports a settlement that could not be recorded.
	ErrNotSettled = errors.New("payout: the settlement could not be recorded")
	// ErrNotSubmitted reports a payout the rail cannot be asked about,
	// because it is not waiting on one - already settled, already failed,
	// or never sent. Ordinary in a sweep that raced another, and never a
	// defect.
	ErrNotSubmitted = errors.New("payout: this payout is not waiting on a rail")
)

// Settlement is what one asking produced.
type Settlement struct {
	// Payout is the row as it now stands. On a payment still in flight it
	// is unchanged, which is the answer.
	Payout Payout
	// Status is what the rail said.
	Status RailStatus
	// Released is what went back to the member when the rail reported a
	// failure, and zero otherwise.
	Released Outcome
}

// Abandoner puts the money back when a payment will never arrive.
//
// A port rather than a *Retries, because what the sweep needs is one
// behaviour and taking the whole service would let it retry as well - and a
// sweep that could re-send payments is a sweep that pays twice the day
// somebody wires it wrong. [Retries.Abandon] implements it.
type Abandoner interface {
	Abandon(ctx context.Context, request uuid.UUID, cause error) (Outcome, error)
}

// Settlements asks the rail what became of the payments it took.
type Settlements struct {
	log       *slog.Logger
	db        Beginner
	rail      Rail
	abandon   Abandoner
	announcer *Announcer
}

// NewSettlements builds the sweep.
func NewSettlements(log *slog.Logger, db Beginner, rail Rail, abandon Abandoner) (*Settlements, error) {
	switch {
	case log == nil:
		return nil, errors.New("payout: the settlement sweep needs a logger")
	case db == nil:
		return nil, ErrNoWithdrawalStore
	case rail == nil:
		return nil, ErrNoRail
	case abandon == nil:
		return nil, errors.New("payout: the settlement sweep needs somewhere to put the money back")
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &Settlements{log: log, db: db, rail: rail, abandon: abandon, announcer: announcer}, nil
}

// Register puts the sweep on the scheduler.
func (s *Settlements) Register(jobs *scheduler.Scheduler) error {
	return jobs.Register(scheduler.Job{
		Name:     SettlementJobName,
		Interval: SettlementInterval,
		Timeout:  settlementTimeout,
		Run:      s.Sweep,
	})
}

// Sweep asks about every payment the rail has taken and not finished.
//
// One payment's failure does not stop the others: they are independent
// payments to independent members, and a rail that cannot answer about one
// says nothing about the rest. Every failure is logged and the sweep carries
// on, returning an error at the end only so the scheduler records the run as
// failed - a sweep that silently half-worked every tick is a sweep nobody
// notices is broken.
func (s *Settlements) Sweep(ctx context.Context) error {
	waiting, err := store.New(s.db).SubmittedPayouts(ctx)
	if err != nil {
		return fmt.Errorf("%w: reading the payments still in flight: %w", ErrNotSettled, err)
	}

	var failures int
	for _, id := range waiting {
		if err := ctx.Err(); err != nil {
			// The sweep was cut short. What is left is still submitted, so
			// the next tick starts where this one stopped.
			return fmt.Errorf("%w: %w", ErrNotSettled, err)
		}
		request := uuid.UUID(id.Bytes)
		switch settled, err := s.Settle(ctx, request); {
		case errors.Is(err, ErrNotSubmitted):
			// Another sweep, a retry or an operator got there first.
			// Nothing to report.
		case err != nil:
			failures++
			s.log.ErrorContext(ctx, "asking the rail about a payment", "request", request, "error", err)
		case settled.Status == StatusSettled:
			s.log.InfoContext(ctx, "a payment reached the member",
				"request", request, "payout", settled.Payout.ID)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%w: %d of %d payments could not be resolved", ErrNotSettled, failures, len(waiting))
	}
	return nil
}

// Settle asks the rail about one payment and records the answer.
//
// The rail is asked OUTSIDE any transaction, for the reason every other rail
// call in this package is: a lock held across a call to a payment rail is a
// lock held for as long as that rail feels like taking. The state is re-read
// under lock afterwards, so a payout that moved while the rail was thinking
// is refused rather than written over.
func (s *Settlements) Settle(ctx context.Context, request uuid.UUID) (Settlement, error) {
	if request == uuid.Nil {
		return Settlement{}, fmt.Errorf("%w: no payment to ask about", ErrNotSettled)
	}

	waiting, err := s.submitted(ctx, request)
	if err != nil {
		return Settlement{}, err
	}
	reference, err := NewRailReference(waiting.RailReference)
	if err != nil {
		// A submitted payout with no usable reference cannot be asked
		// about. Loud, because it is a row that should not exist: a
		// submission records what the rail called it.
		return Settlement{}, fmt.Errorf("%w: %s carries no rail reference: %w", ErrNotSettled, waiting.ID, err)
	}

	status, err := s.rail.Status(ctx, reference)
	if err != nil {
		return Settlement{}, fmt.Errorf("payout: asking the %s rail about %s: %w", s.rail.Kind(), reference, err)
	}

	switch status {
	case StatusSettled:
		return s.recordArrival(ctx, request)
	case StatusFailed:
		// The rail took the payment and could not complete it. Same end as
		// a refused submission, so the same code puts the money back.
		released, err := s.abandon.Abandon(ctx, request, Terminal(
			fmt.Errorf("the %s rail reports %s did not complete", s.rail.Kind(), reference)))
		if err != nil {
			return Settlement{}, fmt.Errorf("%w: putting %s back: %w", ErrNotSettled, request, err)
		}
		return Settlement{Payout: released.Payout, Status: status, Released: released}, nil
	default:
		// Still in flight. Nothing to write, and nothing to say: a member
		// waiting is not news.
		return Settlement{Payout: waiting, Status: status}, nil
	}
}

// submitted reads the payout this sweep may ask about, refusing one that is
// not waiting on a rail.
func (s *Settlements) submitted(ctx context.Context, request uuid.UUID) (Payout, error) {
	row, err := store.New(s.db).GetPayoutForRequest(ctx, pgtype.UUID{Bytes: request, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Payout{}, fmt.Errorf("%w: %s", ErrNoPayout, request)
	case err != nil:
		return Payout{}, fmt.Errorf("%w: reading the payout for %s: %w", ErrNotSettled, request, err)
	}
	waiting, err := payoutFrom(row)
	if err != nil {
		return Payout{}, err
	}
	if waiting.State != StatusSubmitted {
		return Payout{}, fmt.Errorf("%w: %s is %s", ErrNotSubmitted, waiting.ID, waiting.State)
	}
	return waiting, nil
}

// recordArrival writes the settlement: the payout, the request, the event.
//
// One transaction, and nothing outside this database is touched inside it -
// the rail has already answered. The three writes are one commit because a
// payout marked settled whose request still reads approved is a member being
// told two different things by two screens.
func (s *Settlements) recordArrival(ctx context.Context, request uuid.UUID) (Settlement, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: %w", ErrNotSettled, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	// Re-read under lock: the rail was asked outside this transaction, so
	// the payout may have moved while it was thinking.
	locked, err := queries.LockPayoutForRequest(ctx, pgtype.UUID{Bytes: request, Valid: true})
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: reading the payout for %s: %w", ErrNotSettled, request, err)
	}
	waiting, err := payoutFrom(locked)
	if err != nil {
		return Settlement{}, err
	}
	if waiting.State != StatusSubmitted {
		return Settlement{}, fmt.Errorf("%w: %s is %s", ErrNotSubmitted, waiting.ID, waiting.State)
	}

	// settled_at is the database's, computed by the statement itself so it
	// cannot disagree with the state payout_settled_iff_settlement_time
	// ties it to.
	arrivedRow, err := queries.RecordPayoutOutcome(ctx, store.RecordPayoutOutcomeParams{
		ID:        pgtype.UUID{Bytes: waiting.ID, Valid: true},
		FromState: string(StatusSubmitted),
		ToState:   string(StatusSettled),
	})
	if err != nil {
		return Settlement{}, fmt.Errorf("%w: settling %s: %w", ErrNotSettled, waiting.ID, err)
	}
	arrived, err := payoutFrom(arrivedRow)
	if err != nil {
		return Settlement{}, err
	}

	// The request follows, without touching decided_by or decided_at: the
	// human decided when they approved, and a sweep's clock is not that.
	if _, err := queries.MarkWithdrawalPaid(ctx, pgtype.UUID{Bytes: request, Valid: true}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The payout says submitted and the request does not say
			// approved. Nothing here can reconcile that, and guessing
			// would move money's paper trail on a hunch.
			return Settlement{}, fmt.Errorf("%w: %s is settled and its request is not approved", ErrNotSettled, waiting.ID)
		}
		return Settlement{}, fmt.Errorf("%w: marking %s paid: %w", ErrNotSettled, request, err)
	}

	if err := s.announcer.Settled(ctx, tx, arrived); err != nil {
		return Settlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Settlement{}, fmt.Errorf("%w: %s: %w", ErrNotSettled, request, err)
	}
	return Settlement{Payout: arrived, Status: StatusSettled}, nil
}
