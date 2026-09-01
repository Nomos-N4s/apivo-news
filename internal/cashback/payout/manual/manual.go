// Package manual is the payout rail a person carries.
//
// It moves no money. An operator makes the transfer at their bank and records
// what the bank called it; this package's job is to make that arrangement
// hold the same two invariants a real rail does - C-4, that nothing moves
// without a named approver, and C-5, that a submission carried out twice
// moves money once.
//
// The alpha ships it beside any real rail (FR-052), and not as a placeholder:
// a network that pays by bank transfer once a month is a manual rail whether
// or not anybody calls it one, and the failure it has to survive is an
// operator who runs the same instruction twice.
package manual

import (
	"context"
	"fmt"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// referencePrefix marks a reference as one nobody's bank issued yet.
//
// It is a visible difference on purpose. A manual payout carries this until
// an operator replaces it with what their bank actually called the transfer,
// and an operator reading a payout row has to be able to tell "I have not
// done this yet" from "I did it and here is the proof" at a glance.
const referencePrefix = "manual:"

// Rail is the operator-carried payout rail.
//
// It holds nothing. That is not a simplification: a rail's job under C-5 is
// to make a repeat submission land on the same payment, and this one achieves
// that by DERIVING its reference from the idempotency key rather than by
// remembering which keys it has seen. Derivation cannot forget, cannot be
// lost with a process, and cannot disagree with itself across two replicas -
// which a remembered set can do all three of.
type Rail struct{}

// New builds the manual rail.
func New() *Rail { return &Rail{} }

// Kind reports that this is the manual rail.
func (r *Rail) Kind() payout.Kind { return payout.KindManual }

// Submit records the intent to pay and answers the reference an operator
// quotes while they carry it out.
//
// Nothing is contacted and nothing moves. The instruction is validated -
// including the descriptor, which a person will type onto a transfer - and
// the reference comes back derived from the idempotency key, so submitting
// the same instruction twice yields the same reference and the second is
// visibly not a second payment.
//
// This is the whole of C-5 for this rail, and it is stronger than a
// remembered set of keys would be: there is no state to lose, to fall behind,
// or to disagree with itself in another process.
func (r *Rail) Submit(ctx context.Context, instruction payout.Instruction) (payout.RailReference, error) {
	if err := instruction.Validate(); err != nil {
		return payout.RailReference{}, err
	}
	if err := ctx.Err(); err != nil {
		// Nothing was submitted, and nothing here can have half-happened -
		// so this is retryable with the same key, which is exactly what
		// that key is for.
		return payout.RailReference{}, payout.Retryable(err)
	}
	if instruction.Destination.Kind != payout.KindManual {
		// A destination for another rail. Terminal: handing it to this one
		// again cannot start working.
		return payout.RailReference{}, payout.Terminal(fmt.Errorf(
			"manual: this rail cannot carry a %s destination", instruction.Destination.Kind))
	}
	return payout.NewRailReference(referencePrefix + instruction.IdempotencyKey)
}

// Status answers where a manual payment has got to, which is always
// submitted.
//
// That is not a gap. What moves a manual payout on is a person going to a
// bank, and a person tells the system by recording the settlement against the
// payout row - the schema leaves state, settled_at and rail_reference movable
// for exactly that. Asking this rail would be asking the wrong party: it has
// no way to learn what somebody did, and a rail that guessed "settled"
// because time had passed would be reporting money as delivered on the
// strength of a clock.
func (r *Rail) Status(ctx context.Context, reference payout.RailReference) (payout.RailStatus, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", payout.Retryable(err)
	}
	return payout.StatusSubmitted, nil
}
