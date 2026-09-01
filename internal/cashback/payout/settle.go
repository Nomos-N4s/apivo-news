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
	"strings"
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
	// ErrNotAcknowledged reports a payout the rail never named: the state
	// a submission that timed out leaves behind. There is nothing to ask
	// about - a rail cannot answer about a payment it did not confirm
	// taking - so it belongs to the retry path (FR-053), which re-sends
	// under the same key, and not to this sweep.
	ErrNotAcknowledged = errors.New("payout: the rail never acknowledged this payment")
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

	var failures, unacknowledged int
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
		case errors.Is(err, ErrNotAcknowledged):
			// The retry path's work, not this sweep's. Counted rather than
			// logged one by one: a deployment with a backlog of timed-out
			// submissions wants one line saying how many, not a page of
			// identical errors every five minutes.
			unacknowledged++
		case err != nil:
			failures++
			s.log.ErrorContext(ctx, "asking the rail about a payment", "request", request, "error", err)
		case settled.Status == StatusSettled:
			s.log.InfoContext(ctx, "a payment reached the member",
				"request", request, "payout", settled.Payout.ID)
		}
	}
	if unacknowledged > 0 {
		// WARN rather than ERROR: nothing is broken here, and nothing this
		// sweep does will fix it. It is a queue for the retry path, and an
		// operator seeing it grow knows what to look at.
		s.log.WarnContext(ctx, "payments the rail never acknowledged, waiting on a retry",
			"count", unacknowledged, "of", len(waiting))
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
	if waiting.RailReference == "" {
		// A submission that timed out. The payout is real and the money
		// may be in flight, but the rail never said what it called the
		// payment, so there is nothing to ask it about. A retry re-sends
		// under the same key and gets the reference; only then can this
		// sweep do anything.
		return Settlement{}, fmt.Errorf("%w: %s", ErrNotAcknowledged, waiting.ID)
	}
	reference, err := NewRailReference(waiting.RailReference)
	if err != nil {
		// Not blank - the column refuses that - so this is a stored value
		// the port will not accept, which is a row that should not exist.
		return Settlement{}, fmt.Errorf("%w: %s carries an unusable rail reference: %w", ErrNotSettled, waiting.ID, err)
	}

	status, err := s.rail.Status(ctx, reference)
	if err != nil {
		return Settlement{}, fmt.Errorf("payout: asking the %s rail about %s: %w", s.rail.Kind(), reference, err)
	}

	switch status {
	case StatusSettled:
		// Nobody told this service: the rail did, and it has no name.
		return s.recordArrival(ctx, request, recorded{})
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

// recorded is what an operator supplied when they told this service a manual
// payment had landed. Zero when a rail reported it instead: a rail has no
// name, and it already said what it called the payment.
type recorded struct {
	Operator  uuid.UUID
	Reference string
}

// Recording is an operator saying a payment they made by hand has landed.
type Recording struct {
	// Request is the withdrawal whose payment settled.
	Request uuid.UUID
	// Operator is the human saying so (FR-061). Never taken from a body -
	// the endpoint takes it from the token, as every operator action does.
	Operator uuid.UUID
	// Reference is what the BANK called the transfer, replacing the
	// manual: placeholder the submission recorded. It is what a member
	// quotes and what an auditor follows, so the placeholder standing in
	// for it is a payment nobody can trace.
	Reference string
}

// Record settles a payment an operator made by hand.
//
// This is the other half of closing the money loop, and on the manual rail
// it is the ONLY half that works: manual.Status always answers submitted,
// deliberately, because a rail that guessed "settled" because time had
// passed would report money as delivered on the strength of a clock. A
// person went to a bank, so a person says so.
//
// The rail is not asked, because there is nothing to ask. Everything else -
// the lock, the two writes, the event - is the sweep's path exactly, so a
// settlement recorded by hand and one reported by a rail leave the same rows
// behind.
func (s *Settlements) Record(ctx context.Context, recording Recording) (Settlement, error) {
	reference := strings.TrimSpace(recording.Reference)
	switch {
	case recording.Request == uuid.Nil:
		return Settlement{}, fmt.Errorf("%w: no payment to record", ErrNotSettled)
	case recording.Operator == uuid.Nil:
		// FR-061 in the one place a caller could get it wrong. Recording
		// money as delivered is an operator action and an anonymous one is
		// a bug in the gate above.
		return Settlement{}, fmt.Errorf("%w: no operator is recording it", ErrNotSettled)
	case reference == "":
		// The placeholder means "nobody has done this yet". Settling
		// without replacing it would say a payment landed and leave
		// nothing to trace it by.
		return Settlement{}, fmt.Errorf("%w: no reference: what did the bank call the transfer?", ErrNotSettled)
	}
	// No NewRailReference check here, deliberately: it refuses exactly the
	// blank string, which the trim above has already refused. A second
	// guard for the same thing would be a branch nothing could reach.
	return s.recordArrival(ctx, recording.Request, recorded{
		Operator: recording.Operator, Reference: reference,
	})
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
func (s *Settlements) recordArrival(ctx context.Context, request uuid.UUID, told recorded) (Settlement, error) {
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
		// Null leaves the reference the submission recorded, which is what
		// a rail-reported settlement wants: the rail already named it. An
		// operator recording a manual payment supplies what their BANK
		// called it, replacing the manual: placeholder that says "nobody
		// has done this yet".
		RailReference: pgtype.Text{String: told.Reference, Valid: told.Reference != ""},
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

	if err := s.announcer.Settled(ctx, tx, arrived, told.Operator); err != nil {
		return Settlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Settlement{}, fmt.Errorf("%w: %s: %w", ErrNotSettled, request, err)
	}
	return Settlement{Payout: arrived, Status: StatusSettled}, nil
}
