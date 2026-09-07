package wallet

// What cashback does when an account stops existing upstream (T126).
//
// The contract fixes the answer, and it is deliberately narrow:
//
//	identity.account.deleted -> cashback does NOT delete financial rows; it
//	closes participation and flags any in-flight withdrawal for operator
//	attention.
//
// The schema had already made the first half the only possibility. Every
// cashback table naming a member holds a foreign key into public.account -
// participation, click, entry, withdrawal request, payout destination - so
// Postgres refuses to delete an account row the moment that member has
// clicked once. Deletion upstream can only mean the identity is anonymised
// and the participation closed, which is exactly what 0017's own comment
// says leaving is for: "it never deletes the financial record built on it
// (FR-003), because entries, payouts and evidence outlive participation by
// law and by accounting".
//
// The second half is DERIVED and not stored. cashback.withdrawal_request
// gains no column and no state: its machine is tight, a flag beside it
// would be a second truth to drift, and whether a request is in flight is
// a question about the row that a read answers. What is written is the
// announcement, which is how every other operator-relevant fact in this
// system becomes visible.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

const (
	// TypeAccountDeleted is the identity fact this module reacts to. It is
	// another producer's type, named here because a consumer subscribes by
	// type and this is the one string that decides whether any of this
	// runs.
	TypeAccountDeleted = "identity.account.deleted"
	// AccountClosuresSubscriber is this consumer's durable name. It keys
	// the checkpoint, the delivery records and the dead letters, so it must
	// stay stable across releases: renaming it re-delivers the whole stream
	// from the beginning under a name with no checkpoint.
	AccountClosuresSubscriber = "cashback-account-closures"
	// ReasonAccountDeleted is why a withdrawal was flagged, in the
	// vocabulary of the cause rather than as a sentence somebody wrote.
	ReasonAccountDeleted = "account_deleted"
)

var (
	// ErrNoAccountClosures reports a consumer built without a part it
	// cannot work without.
	ErrNoAccountClosures = errors.New("wallet: closing a deleted account needs a database, a participation service and a store")
	// ErrUnhandledEvent reports an event this consumer was handed but does
	// not answer for. It means the subscription and the handler disagree
	// about what this consumer is for, which is a wiring defect rather
	// than anything the stream did.
	ErrUnhandledEvent = errors.New("wallet: this consumer does not handle that event type")
	// ErrUnreadableEvent reports a payload this consumer cannot act on: a
	// deletion naming no account, or naming something that is not one.
	//
	// Returned as a failure rather than swallowed, so the delivery spends
	// its budget and parks in the dead-letter table for an operator. A
	// payload that cannot be parsed will never parse, so the retries buy
	// nothing - but the alternative is acknowledging a deletion nobody
	// acted on, and a member's participation left open with no record that
	// anything went wrong.
	ErrUnreadableEvent = errors.New("wallet: the deletion says nothing this consumer can act on")
)

// ClosureStore is the one read this file needs, named here per the boundary
// rules. *store.Queries satisfies it over a pool.
type ClosureStore interface {
	InFlightWithdrawalsForAccount(ctx context.Context, accountID pgtype.UUID) ([]store.InFlightWithdrawalsForAccountRow, error)
}

// AccountClosures reacts to an account this deployment no longer has.
//
// It is a subscriber, so [AccountClosures.Handle] is called at least once
// per event and must be idempotent. Both halves are, and by construction
// rather than by a processed-id table: closing a participation narrows on
// status = 'active' and so closes one exactly once, and the flag carries an
// idempotency key so a second announcement of the same fact is refused by
// the outbox rather than shown to an operator twice.
type AccountClosures struct {
	log            *slog.Logger
	db             TxBeginner
	participations *Participations
	store          ClosureStore
	announcer      *Announcer
}

// NewAccountClosures builds the consumer, refusing one missing a part.
//
// The announcer is built here rather than injected, for the reason
// [NewParticipations] builds one: a deletion that closed a member's
// participation and told nobody would be a fact only this process ever
// knew.
func NewAccountClosures(log *slog.Logger, db TxBeginner, participations *Participations, s ClosureStore) (*AccountClosures, error) {
	if log == nil || db == nil || participations == nil || s == nil {
		return nil, ErrNoAccountClosures
	}
	announcer, err := NewAnnouncer()
	if err != nil {
		return nil, err
	}
	return &AccountClosures{log: log, db: db, participations: participations, store: s, announcer: announcer}, nil
}

// deletion is the payload of identity.account.deleted, as the contract
// writes it.
type deletion struct {
	AccountID uuid.UUID `json:"account_id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// Handle closes the member's participation and says out loud any withdrawal
// of theirs still moving money. It is the [events.Handler] this module
// registers.
//
// Order matters and it is this way round. Closing first means a redelivery
// that fails on the second half still leaves the member out of cashback,
// which is the half the contract is emphatic about; flagging first would
// leave a deleted member able to click through the catalogue for as long as
// the announcement kept failing.
//
// The two are separate transactions, deliberately. Leave owns its own, and
// wrapping the pair would mean either reaching into it or announcing the
// flags in a transaction that has already committed the close. Delivery is
// at-least-once, so the pair is resumable instead of atomic: a redelivery
// re-runs a close that does nothing and re-announces flags the outbox
// already refuses.
func (c *AccountClosures) Handle(ctx context.Context, event events.Event) error {
	if event.Type != TypeAccountDeleted {
		return fmt.Errorf("%w: %s", ErrUnhandledEvent, event.Type)
	}
	var deleted deletion
	if err := json.Unmarshal(event.Payload, &deleted); err != nil {
		return fmt.Errorf("%w: event %s: %w", ErrUnreadableEvent, event.EventID, err)
	}
	if deleted.AccountID == uuid.Nil {
		return fmt.Errorf("%w: event %s names no account", ErrUnreadableEvent, event.EventID)
	}

	if err := c.close(ctx, deleted.AccountID); err != nil {
		return err
	}
	return c.flagInFlight(ctx, deleted)
}

// close ends the member's participation, treating a member who never
// joined - and one who had already left - as the state this deletion is
// asking for rather than as a failure.
func (c *AccountClosures) close(ctx context.Context, member uuid.UUID) error {
	left, err := c.participations.Leave(ctx, member)
	switch {
	case errors.Is(err, ErrNotJoined):
		// An account deleted upstream that never opted into cashback. There
		// is nothing here that belongs to them, and that is ordinary.
		return nil
	case err != nil:
		return fmt.Errorf("wallet: closing deleted account %s's participation: %w", member, err)
	}
	c.log.InfoContext(ctx, "a deleted account's cashback participation is closed",
		"member", member, "left_at", left.LeftAt)
	return nil
}

// flagInFlight announces every withdrawal of this member's that is still
// moving money, so an operator can decide what happens to it.
//
// Nothing here refuses, approves or settles anything. What should happen to
// a payment owed to somebody who has deleted their account is a decision
// with money and law in it, and the contract puts it in front of a person
// rather than in a rule.
func (c *AccountClosures) flagInFlight(ctx context.Context, deleted deletion) error {
	rows, err := c.store.InFlightWithdrawalsForAccount(ctx, pgtype.UUID{Bytes: deleted.AccountID, Valid: true})
	if err != nil {
		return fmt.Errorf("wallet: reading deleted account %s's withdrawals: %w", deleted.AccountID, err)
	}
	for _, row := range rows {
		flagged := FlaggedWithdrawal{
			Request: uuid.UUID(row.ID.Bytes),
			Member:  deleted.AccountID,
			State:   row.State,
			Reason:  ReasonAccountDeleted,
			At:      deleted.DeletedAt,
		}
		if err := c.announce(ctx, flagged); err != nil {
			return err
		}
	}
	return nil
}

// announce writes one flag in a transaction of its own.
//
// One transaction per flag rather than one for all of them, because an
// idempotency collision aborts the transaction it happens in: a member with
// three in-flight requests, redelivered after two were announced, would
// otherwise lose the third to the second's collision. Each flag is
// independently durable, and each redelivery re-announces only what is
// genuinely missing.
func (c *AccountClosures) announce(ctx context.Context, flagged FlaggedWithdrawal) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("wallet: flagging withdrawal %s: %w", flagged.Request, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	switch err := c.announcer.WithdrawalFlagged(ctx, tx, flagged); {
	case errors.Is(err, events.ErrAlreadyAppended):
		// Said already, on an earlier delivery of this same deletion. The
		// transaction is aborted and there is nothing to commit.
		return nil
	case err != nil:
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("wallet: flagging withdrawal %s: %w", flagged.Request, err)
	}
	c.log.WarnContext(ctx, "a withdrawal is still in flight for a deleted account",
		"request", flagged.Request, "member", flagged.Member, "state", flagged.State)
	return nil
}

// Subscribe registers this consumer with the process's event registry, so
// the composition root names the subscriber once and in one place.
func (c *AccountClosures) Subscribe(r *events.Registry) error {
	return r.Subscribe(AccountClosuresSubscriber, []string{TypeAccountDeleted}, c.Handle)
}
