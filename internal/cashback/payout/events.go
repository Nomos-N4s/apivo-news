// What this module tells the rest of the world about a member's money
// leaving (T100, T146, contracts/events.md).
//
// Five facts, and each one is appended in the transaction that made it true:
// a member asked to be paid, an operator released it, an operator refused it,
// a payment will never happen, a payment reached the member. That placement
// is the whole of the contract's atomicity guarantee - there is no code path
// that publishes an event without its state change, or commits a state change
// without its event - which is why nothing here opens a transaction, and why
// an append that fails is fatal to the caller rather than logged.
//
// The last of them arrived late, with T146. Settlement comes from a rail
// through Rail.Status, and until a sweep asked there was no moment at which
// this fact became true, so an announcer method for it would have been a
// method nothing could call.

package payout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const (
	// EventProducer is the domain these facts belong to, and the first
	// segment of every type below - the writer refuses a type naming
	// another producer, because a domain publishes only its own facts.
	EventProducer = "cashback"
	// TypeWithdrawalRequested announces a member asking to be paid. It is
	// the first event in a chain that ends in money leaving the business,
	// and the only one in it a member causes.
	TypeWithdrawalRequested = EventProducer + ".withdrawal.requested"
	// TypeWithdrawalApproved announces the named human who released one
	// (C-4, FR-061). The actor is in the payload rather than left to a
	// lookup: an audit trail a consumer has to come back and ask about is
	// the synchronous call-back consumer rule 2 forbids.
	TypeWithdrawalApproved = EventProducer + ".withdrawal.approved"
	// TypeWithdrawalRejected announces one refused, with the reason the
	// member is owed (FR-061).
	TypeWithdrawalRejected = EventProducer + ".withdrawal.rejected"
	// TypePayoutFailed announces a payment that will never happen and the
	// money that went back because of it (FR-053).
	TypePayoutFailed = EventProducer + ".payout.failed"
	// TypePayoutSettled announces money that reached the member. It is the
	// last fact in the chain and the only one that says a member was
	// actually paid - everything before it says a payment was intended.
	//
	// It carries an actor when a human recorded it and none when a rail
	// reported it, which is the one thing a consumer cannot work out for
	// itself.
	TypePayoutSettled = EventProducer + ".payout.settled"
)

// ErrNotAnnounced reports an event that could not be appended beside the
// state change it describes.
//
// Always fatal to the caller that raised it. The append shares the caller's
// transaction, so a swallowed failure would commit a decided withdrawal with
// no event - the one thing the contract says no code path may do - and
// nothing would come back to notice.
var ErrNotAnnounced = errors.New("payout: the event could not be appended beside the state change")

// Announcer appends this module's facts to the outbox.
//
// It holds no database handle. Every method takes the transaction carrying
// the state change, because the placement of the append - not anything this
// type does - is what makes the event and the fact atomic.
type Announcer struct {
	writer *events.Writer
}

// NewAnnouncer builds the announcer for this domain.
func NewAnnouncer() (*Announcer, error) {
	writer, err := events.NewWriter(EventProducer)
	if err != nil {
		return nil, err
	}
	return &Announcer{writer: writer}, nil
}

// withdrawalPayload is the contract's shape for all three withdrawal types.
//
// One struct rather than three, because the contract gives them one payload
// and three that drifted apart would be three a consumer had to branch on.
// Actor and Reason are omitted where the fact does not have them: a request
// has no actor other than the member it already names, and only a refusal
// carries a reason.
type withdrawalPayload struct {
	RequestID uuid.UUID    `json:"request_id"`
	AccountID uuid.UUID    `json:"account_id"`
	Amount    money.Amount `json:"amount"`
	Actor     *uuid.UUID   `json:"actor,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	At        time.Time    `json:"at"`
}

// Requested announces one withdrawal a member has just asked for.
//
// The amount is what was RESERVED, which is what a payout will pay and not
// necessarily what was asked for (D9) - the same figure the endpoint returns
// as reserved_amount, for the same reason: a consumer that took the asking
// figure would be tracking money that was never moved.
//
// Keyed on the request, because a request is made once.
func (a *Announcer) Requested(ctx context.Context, db events.RowQuerier, made Withdrawal) error {
	if made.ID == uuid.Nil {
		return fmt.Errorf("%w: %s about a request the database did not insert", ErrNotAnnounced, TypeWithdrawalRequested)
	}
	return a.announceWithdrawal(ctx, db, TypeWithdrawalRequested, withdrawalPayload{
		RequestID: made.ID,
		AccountID: made.Member,
		Amount:    made.Amount,
		// The instant the ROW carries, read back rather than taken from the
		// clock, so the event and the request name one moment. A consumer
		// joining the two on time would otherwise find them disagreeing by
		// however long the transaction stayed open.
		At: made.RequestedAt,
	})
}

// Approved announces one withdrawal an operator has just released.
//
// Announced from the transaction that CLAIMS the payout - before the rail is
// asked, because that is where the decision is made and committed (FR-052).
// Waiting for the rail would mean an approval that never reached the stream
// whenever a submission timed out, which is precisely when somebody wants to
// know a decision was made.
//
// Keyed on the request: a request is approved once, and the generated payout
// key already refuses the second approval.
func (a *Announcer) Approved(ctx context.Context, db events.RowQuerier, released Withdrawal) error {
	switch {
	case released.ID == uuid.Nil:
		return fmt.Errorf("%w: %s about a request the database did not decide", ErrNotAnnounced, TypeWithdrawalApproved)
	case released.DecidedBy == uuid.Nil:
		// C-4: a decision with no name is one nobody can be asked about,
		// and the column is NOT NULL, so this is a fact the schema would
		// have refused.
		return fmt.Errorf("%w: %s naming no approver", ErrNotAnnounced, TypeWithdrawalApproved)
	}
	actor := released.DecidedBy
	return a.announceWithdrawal(ctx, db, TypeWithdrawalApproved, withdrawalPayload{
		RequestID: released.ID,
		AccountID: released.Member,
		Amount:    released.Amount,
		Actor:     &actor,
		At:        released.DecidedAt,
	})
}

// Rejected announces one withdrawal an operator has just refused, and the
// reason they gave.
//
// The reason travels because a member is owed one (FR-061) and because
// withdrawal_request_rejection_has_reason means a refusal without one does
// not exist to announce. Keyed on the request, which is refused once.
func (a *Announcer) Rejected(ctx context.Context, db events.RowQuerier, refused Withdrawal) error {
	switch {
	case refused.ID == uuid.Nil:
		return fmt.Errorf("%w: %s about a request the database did not decide", ErrNotAnnounced, TypeWithdrawalRejected)
	case refused.DecidedBy == uuid.Nil:
		return fmt.Errorf("%w: %s naming no operator", ErrNotAnnounced, TypeWithdrawalRejected)
	case refused.DecisionReason == "":
		return fmt.Errorf("%w: %s carrying no reason", ErrNotAnnounced, TypeWithdrawalRejected)
	}
	actor := refused.DecidedBy
	return a.announceWithdrawal(ctx, db, TypeWithdrawalRejected, withdrawalPayload{
		RequestID: refused.ID,
		AccountID: refused.Member,
		Amount:    refused.Amount,
		Actor:     &actor,
		Reason:    refused.DecisionReason,
		At:        refused.DecidedAt,
	})
}

// Failed announces a payment that will never happen.
//
// The classification is the rail's own verdict and the reason the money went
// back rather than staying reserved (FR-053), so it travels with the fact: a
// consumer that saw only "failed" could not tell this from a payment still
// being retried.
//
// Subject is the PAYOUT rather than the request, because this is a fact
// about the payment, and per-subject ordering is the only ordering the
// stream guarantees. Keyed on the payout, which fails once - payout_guard
// makes failed terminal.
func (a *Announcer) Failed(ctx context.Context, db events.RowQuerier, failed Payout, classification string, at time.Time) error {
	switch {
	case failed.ID == uuid.Nil:
		return fmt.Errorf("%w: %s about a payout the database did not record", ErrNotAnnounced, TypePayoutFailed)
	case classification == "":
		return fmt.Errorf("%w: %s with no classification", ErrNotAnnounced, TypePayoutFailed)
	}
	payload, err := json.Marshal(struct {
		PayoutID  uuid.UUID `json:"payout_id"`
		RequestID uuid.UUID `json:"request_id"`
		// RailReference is omitted when the rail never answered, which is
		// the ordinary shape of a payment that failed on submission. An
		// empty string would say the rail named it "".
		RailReference  string    `json:"rail_reference,omitempty"`
		Classification string    `json:"classification"`
		At             time.Time `json:"at"`
	}{
		PayoutID:       failed.ID,
		RequestID:      failed.Request,
		RailReference:  failed.RailReference,
		Classification: classification,
		// The DECISION's instant, passed in by the caller that holds it.
		// The payout row carries only when it was submitted, which is not
		// when it failed - and an event that used it would date the failure
		// to whenever the money was first sent.
		At: at,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypePayoutFailed, err)
	}
	return a.append(ctx, db, TypePayoutFailed, failed.ID, TypePayoutFailed+":"+failed.ID.String(), payload)
}

// Settled announces one payment that reached the member.
//
// The rail reference travels because it is what a member quotes to their
// bank and what an auditor follows, and by this point it always exists: a
// payout cannot settle without having been submitted, and a submission
// records what the rail called it.
//
// Subject is the payout, as for [Announcer.Failed]: both are facts about the
// payment rather than about the request. Keyed on the payout, which settles
// once - payout_guard makes settled terminal.
func (a *Announcer) Settled(ctx context.Context, db events.RowQuerier, settled Payout, actor uuid.UUID) error {
	switch {
	case settled.ID == uuid.Nil:
		return fmt.Errorf("%w: %s about a payout the database did not record", ErrNotAnnounced, TypePayoutSettled)
	case settled.RailReference == "":
		// A settled payout with no reference is a row the schema would not
		// hold: it settled, so it was submitted, so the rail named it.
		return fmt.Errorf("%w: %s about a payment the rail never named", ErrNotAnnounced, TypePayoutSettled)
	case settled.SettledAt.IsZero():
		// payout_settled_iff_settlement_time: settled without an instant is
		// a row the database refuses, so announcing one would put a fact in
		// the stream that is not in the schema.
		return fmt.Errorf("%w: %s about a payment with no settlement time", ErrNotAnnounced, TypePayoutSettled)
	}
	// Actor is present exactly when a HUMAN said the payment landed, and
	// absent when a rail reported it. Both are real settlements and neither
	// is more true, but only one has somebody to ask about it (FR-061), and
	// a consumer cannot tell them apart from the payload otherwise.
	var recordedBy *uuid.UUID
	if actor != uuid.Nil {
		recordedBy = &actor
	}
	payload, err := json.Marshal(struct {
		PayoutID      uuid.UUID  `json:"payout_id"`
		RequestID     uuid.UUID  `json:"request_id"`
		RailReference string     `json:"rail_reference"`
		Actor         *uuid.UUID `json:"actor,omitempty"`
		At            time.Time  `json:"at"`
	}{
		PayoutID:      settled.ID,
		RequestID:     settled.Request,
		RailReference: settled.RailReference,
		Actor:         recordedBy,
		// The settlement instant the ROW carries, for the reason every other
		// instant here is read back rather than taken from a clock.
		At: settled.SettledAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypePayoutSettled, err)
	}
	return a.append(ctx, db, TypePayoutSettled, settled.ID, TypePayoutSettled+":"+settled.ID.String(), payload)
}

// announceWithdrawal marshals and appends one of the three withdrawal facts.
func (a *Announcer) announceWithdrawal(ctx context.Context, db events.RowQuerier, eventType string, payload withdrawalPayload) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, eventType, err)
	}
	return a.append(ctx, db, eventType, payload.RequestID,
		eventType+":"+payload.RequestID.String(), encoded)
}

// append writes one event under the given key.
//
// An already-appended key is a FAILURE here, not a no-op. The outbox reports
// a collision as a unique violation, and a failed statement aborts the whole
// transaction - so a caller that shrugged it off would carry on through a
// transaction whose every later statement raises "current transaction is
// aborted", and would commit nothing while reporting success.
func (a *Announcer) append(ctx context.Context, db events.RowQuerier, eventType string, subject uuid.UUID, key string, payload json.RawMessage) error {
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:           eventType,
		Subject:        subject,
		IdempotencyKey: key,
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about %s: %w", ErrNotAnnounced, eventType, subject, err)
	}
	return nil
}
