// The [Rail] port (T088, contracts/ports.md section 3): what every way
// of moving money out of the business looks like from the domain's side, and
// how its failures are classified.
//
// One file, because the interface and the classification are one contract.
// A rail that returned an unclassified error would leave the caller with the
// only two choices it must never make freely - retry a payment that already
// left, or release a reservation for one that did.

package payout

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The classifications a rail failure carries, and the sentinels a caller
// decides on. Which one a failure wears is the difference between a member
// being paid twice and a member not being paid at all.
var (
	// ErrRailRetryable reports a submission that may yet succeed and must be
	// re-sent WITH THE SAME KEY: a timeout, a rail that was briefly
	// unreachable, a 5xx. The reservation stays where it is, because the
	// money may already be in flight and releasing it would let the member
	// spend a balance that is about to leave.
	//
	// The same key is the whole of why this is safe. Contract rule 1 makes
	// Submit idempotent on it, so a retry either finds the earlier
	// submission or makes the only one there will be.
	ErrRailRetryable = errors.New("payout: the rail may yet carry this payment; retry with the same key")
	// ErrRailTerminal reports a submission that will never succeed: a
	// rejected destination, a rail that refuses the amount, a credential
	// the rail will not accept. The reservation is released back to
	// confirmed balance and the member is told (FR-053, US4 scenario 5).
	//
	// Getting this wrong in either direction costs real money. Calling a
	// timeout terminal releases a reservation for a payment that then
	// settles, and the member spends money that has already left. Calling a
	// rejection retryable holds a member's balance hostage to a payment
	// that will never happen, silently, until somebody looks.
	ErrRailTerminal = errors.New("payout: the rail will never carry this payment")
	// ErrInvalidInstruction reports an instruction no rail could act on. It
	// is refused before any rail is contacted, because a submission that
	// reached a rail malformed is one whose outcome nobody can classify.
	ErrInvalidInstruction = errors.New("payout: the instruction does not describe a payment")
	// ErrInvalidRailReference reports a reference a rail did not really
	// give: blank, or the zero value. It mirrors
	// payout_rail_reference_not_blank, refused here so the failure names the
	// rail rather than a constraint.
	ErrInvalidRailReference = errors.New("payout: the rail gave no reference to the payment it made")
)

// Retryable marks a rail failure as one to re-send with the same key.
func Retryable(cause error) error {
	if cause == nil {
		return ErrRailRetryable
	}
	return fmt.Errorf("%w: %w", ErrRailRetryable, cause)
}

// Terminal marks a rail failure as one that will never succeed.
func Terminal(cause error) error {
	if cause == nil {
		return ErrRailTerminal
	}
	return fmt.Errorf("%w: %w", ErrRailTerminal, cause)
}

// RailStatus is where a submitted payment has got to, in the vocabulary
// payout_state_known accepts. Closed, with no valid zero value: a payment
// whose state nobody stated is one an operator cannot act on.
type RailStatus string

const (
	// StatusSubmitted is a payment the rail has accepted and not finished.
	StatusSubmitted RailStatus = "submitted"
	// StatusSettled is a payment that reached the member.
	StatusSettled RailStatus = "settled"
	// StatusFailed is a payment the rail will not complete. Unlike
	// [ErrRailTerminal], which is a submission that never started, this is
	// one that started and ended badly - and the money did not move.
	StatusFailed RailStatus = "failed"
)

// Valid reports whether s is one of the three states. The zero value is not.
func (s RailStatus) Valid() bool {
	switch s {
	case StatusSubmitted, StatusSettled, StatusFailed:
		return true
	default:
		return false
	}
}

// String renders the status for errors, logs and test failures.
func (s RailStatus) String() string { return string(s) }

// RailReference is the rail's own identifier for a payment: a bank's payment
// id, or the reference an operator writes down after making a transfer by
// hand. It is what a later question about the payment is asked with, and what
// an auditor follows out of this system into somebody else's.
type RailReference struct {
	ref string
}

// NewRailReference builds a reference, refusing one no rail really gave.
func NewRailReference(ref string) (RailReference, error) {
	if strings.TrimSpace(ref) == "" {
		return RailReference{}, fmt.Errorf("%w: %s", ErrInvalidRailReference, strconv.Quote(ref))
	}
	return RailReference{ref: ref}, nil
}

// Ref is the reference as the rail gave it, for storing in
// cashback.payout.rail_reference.
func (r RailReference) Ref() string { return r.ref }

// Validate reports whether a reference was given at all, returning an error
// wrapping [ErrInvalidRailReference] for the zero value.
func (r RailReference) Validate() error {
	if r.ref == "" {
		return fmt.Errorf("%w: the zero value", ErrInvalidRailReference)
	}
	return nil
}

// String renders the reference for logs and test failures, printing the zero
// value as "(none)" so a payment recorded out of order reads as one.
func (r RailReference) String() string {
	if r.ref == "" {
		return "(none)"
	}
	return strconv.Quote(r.ref)
}

// DestinationRef is everything a rail is told about where the money goes,
// and it is deliberately not much.
//
// Contract rule 3: a rail never sees a member's identity beyond the reference
// it needs. So there is no account id here and no destination id - only which
// rail should carry it and the pointer to the details that rail will resolve.
// A rail that cannot name the member cannot log the member, cannot leak the
// member, and cannot make a decision about the member.
type DestinationRef struct {
	// Kind is which rail carries this. It is on the reference rather than
	// inferred by the caller, so a mismatch between the destination and the
	// rail it was handed to is checkable.
	Kind Kind
	// DetailsRef is the reference into the store holding the actual
	// details - cashback.payout_destination.details_ref. Never the details.
	DetailsRef string
}

// Validate refuses a reference no rail could resolve, wrapping
// [ErrInvalidInstruction].
func (d DestinationRef) Validate() error {
	if !d.Kind.Valid() {
		return fmt.Errorf("%w: %s is not a payout kind", ErrInvalidInstruction, strconv.Quote(d.Kind.String()))
	}
	if strings.TrimSpace(d.DetailsRef) == "" {
		return fmt.Errorf("%w: nothing says where the %s details live", ErrInvalidInstruction, d.Kind)
	}
	return nil
}

// ToRef renders a stored [Destination] as the little a rail is allowed to
// see. It is a method on the destination so the narrowing happens in one
// place: a caller that assembled a DestinationRef by hand could put an
// account id in it by accident, and this is where that stops being possible.
func (d Destination) ToRef() DestinationRef {
	return DestinationRef{Kind: d.Kind, DetailsRef: d.DetailsRef}
}

// Instruction is one payment, as the domain hands it to a rail.
//
// contracts/ports.md section 3 calls it PayoutInstruction. It is Instruction
// here because Go reads it as payout.Instruction, and payout.PayoutInstruction
// stutters; the spec's name is written down so a reader matching one to the
// other finds it.
type Instruction struct {
	// IdempotencyKey is derived from the withdrawal request id (D8, C-5) and
	// passed unchanged to the ledger and the rail. A caller cannot choose
	// it: derivation is what makes a retry safe, and a caller-chosen key is
	// exactly how a retry stops being one.
	IdempotencyKey string
	// Amount is what leaves, in minor units and a currency (C-6). Strictly
	// positive: a rail is not how money comes back, and
	// payout_amount_positive says the same in the schema.
	Amount money.Amount
	// Destination is where it goes, narrowed to what a rail may see.
	Destination DestinationRef
	// Descriptor is what the member reads on their statement, rendered from
	// brand configuration (FR-070, FR-073). It is on the instruction rather
	// than in the rail because no rail implementation may contain a product
	// name - that is the rebrandability rule, and a rail that hard-coded one
	// would be a rail that cannot be reused by a second brand.
	Descriptor string
}

// Validate refuses an instruction no rail could act on, wrapping
// [ErrInvalidInstruction].
//
// Every rail calls it before contacting anything, so a malformed submission
// never reaches a rail: an outcome nobody can classify is worse than a
// refusal, because the caller then cannot tell whether to retry.
func (p Instruction) Validate() error {
	if strings.TrimSpace(p.IdempotencyKey) == "" {
		// The one field whose absence is unrecoverable rather than merely
		// wrong: without it a retry is a second payment.
		return fmt.Errorf("%w: no idempotency key, so a retry could not be told from a second payment", ErrInvalidInstruction)
	}
	if err := p.Amount.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInstruction, err)
	}
	if !p.Amount.IsPositive() {
		return fmt.Errorf("%w: %s is not an amount to pay out", ErrInvalidInstruction, p.Amount)
	}
	if err := p.Destination.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(p.Descriptor) == "" {
		// A blank descriptor is a line on a member's bank statement they
		// cannot recognise, which is how a legitimate payment becomes a
		// chargeback.
		return fmt.Errorf("%w: nothing to show on the member's statement", ErrInvalidInstruction)
	}
	return nil
}

// Rail is one way of moving money out of the business.
//
// contracts/ports.md section 3 calls it PayoutRail, for the same reason and
// with the same answer as [Instruction]: payout.Rail is the name Go wants.
//
// The domain owns this interface and an adapter owns the vendor
// (contracts/ports.md section 4): a change of rail is a new package and a
// wiring line in cmd, never a change here.
type Rail interface {
	// Kind names the rail, and must equal the [Kind] of every destination
	// it is handed. It is constant for the life of the rail: the kind is
	// what a stored payout row records in cashback.payout.rail, so a rail
	// whose answer varied would mislabel its own history.
	Kind() Kind

	// Submit sends one payment and answers the rail's reference for it.
	//
	// Idempotent on [Instruction.IdempotencyKey] (contract rule 1). A
	// rail whose vendor cannot promise that implements it locally before
	// returning - "the vendor does not support it" is not an answer,
	// because the alternative is paying a member twice.
	//
	// Every failure is classified [Retryable] or [Terminal]. An
	// unclassified error leaves the caller choosing between retrying a
	// payment that may already have left and releasing a reservation for
	// one that did.
	Submit(ctx context.Context, instruction Instruction) (RailReference, error)

	// Status answers where a submitted payment has got to.
	//
	// It exists because Submit's answer is not the end of the story: a
	// transfer is accepted long before it settles, and a rail that only
	// said "accepted" would leave a member's money in a state nobody could
	// resolve. Failures are classified the same way.
	Status(ctx context.Context, reference RailReference) (RailStatus, error)
}
