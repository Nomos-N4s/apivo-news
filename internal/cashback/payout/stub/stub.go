// Package stub is the payout rail that moves no money and can be made to
// misbehave on purpose.
//
// It exists so the withdrawal path can be driven end to end - reserve,
// approve, submit, settle - without a bank, and more importantly so the three
// ways a real rail ruins a member's day can be exercised deliberately rather
// than waited for: a submission that times out, a submission sent twice, and
// one the rail will never carry.
//
// It is a rail and not a mock. It holds itself to the same contract as any
// other: C-5 on the idempotency key, every failure classified, and no
// knowledge of the member beyond the destination reference.
package stub

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// referencePrefix marks a reference as this rail's, so a payout row written
// during a test or a demo cannot be mistaken for one a bank issued.
const referencePrefix = "stub:"

// ErrRailTimedOut and ErrRailRefused are what the injected modes report
// beneath their classification, so a test can assert on the cause as well as
// on what the caller is meant to do about it.
var (
	// ErrRailTimedOut is the submission that may yet have landed. It is the
	// dangerous one: the caller must NOT release the reservation, because
	// the money may be in flight.
	ErrRailTimedOut = errors.New("stub: the rail did not answer in time")
	// ErrRailRefused is the submission that will never be carried.
	ErrRailRefused = errors.New("stub: the rail refused this payment")
)

// Rail is the stub payout rail.
//
// It remembers the keys it has accepted, which is what a real rail's vendor
// would do on its behalf. The map is the point rather than an implementation
// detail: contract rule 1 says a rail that cannot get idempotency from its
// vendor implements it locally, and this is what that looks like.
type Rail struct {
	mu sync.Mutex
	// submitted maps an idempotency key to the reference it landed on, so a
	// repeat submission returns the first answer instead of minting a
	// second payment.
	submitted map[string]string
	// settled marks the references a caller has declared settled, so
	// Status can move on without a bank.
	settled map[string]bool
	// broke marks the references a caller has declared failed AT THE RAIL:
	// a payment it accepted and could not complete. Distinct from a
	// submission it refused - that one never started and the money never
	// moved, this one started and ended badly - and the two are told apart
	// because only the first can be re-sent.
	broke map[string]bool
	// failNext, when set, is what the next Submit does instead of
	// succeeding.
	failNext error
}

// Option configures a [Rail] at construction.
type Option func(*Rail)

// New builds the stub rail.
func New(opts ...Option) *Rail {
	r := &Rail{submitted: map[string]string{}, settled: map[string]bool{}, broke: map[string]bool{}}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// WithTimeout makes the next submission time out: retryable, and the
// reservation must stay where it is because the payment may have landed.
func WithTimeout() Option {
	return func(r *Rail) { r.failNext = payout.Retryable(ErrRailTimedOut) }
}

// WithPermanentFailure makes the next submission refuse: terminal, and the
// reservation goes back to the member's confirmed balance (FR-053).
func WithPermanentFailure() Option {
	return func(r *Rail) { r.failNext = payout.Terminal(ErrRailRefused) }
}

// FailNext makes the next submission fail in the given way, on a rail that
// is already running.
//
// The options above only reach a rail at construction, and the ordering worth
// rehearsing cannot be set up that way: a rail that has ALREADY accepted a
// key and then starts misbehaving. That is the shape of a real timeout - the
// payment landed, the answer did not - and the retry must find the payment
// rather than the failure.
//
// Pass a classified error ([payout.Retryable] or [payout.Terminal]); nil
// clears a pending failure.
func (r *Rail) FailNext(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext = err
}

// Kind reports that this is the stub rail.
func (r *Rail) Kind() payout.Kind { return payout.KindStub }

// Submit accepts a payment, or fails in whichever way it was told to.
//
// The idempotency check happens BEFORE the injected failure, and that order
// is deliberate: a key this rail has already accepted names a payment that
// exists, and no amount of injected misbehaviour afterwards should make it
// look like it does not. The dangerous shape this models is a timeout on a
// submission that in fact landed - the retry must find the first payment.
func (r *Rail) Submit(ctx context.Context, instruction payout.Instruction) (payout.RailReference, error) {
	if err := instruction.Validate(); err != nil {
		return payout.RailReference{}, err
	}
	if err := ctx.Err(); err != nil {
		return payout.RailReference{}, payout.Retryable(err)
	}
	if instruction.Destination.Kind != payout.KindStub {
		return payout.RailReference{}, payout.Terminal(fmt.Errorf(
			"stub: this rail cannot carry a %s destination", instruction.Destination.Kind))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.submitted[instruction.IdempotencyKey]; ok {
		return payout.NewRailReference(ref)
	}
	if failure := r.failNext; failure != nil {
		// Spent, so a retry after a timeout succeeds - which is the whole
		// scenario worth rehearsing: the caller re-sends the same key and
		// must land on one payment.
		r.failNext = nil
		return payout.RailReference{}, failure
	}

	ref := referencePrefix + instruction.IdempotencyKey
	r.submitted[instruction.IdempotencyKey] = ref
	return payout.NewRailReference(ref)
}

// Status answers where a submitted payment has got to.
//
// A reference this rail never issued is TERMINAL rather than retryable: it
// will not start existing, and a caller that retried would ask forever about
// a payment that was never made.
func (r *Rail) Status(ctx context.Context, reference payout.RailReference) (payout.RailStatus, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", payout.Retryable(err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.knows(reference.Ref()) {
		return "", payout.Terminal(fmt.Errorf("stub: no payment was submitted under %s", reference))
	}
	switch {
	case r.settled[reference.Ref()]:
		return payout.StatusSettled, nil
	case r.broke[reference.Ref()]:
		return payout.StatusFailed, nil
	}
	return payout.StatusSubmitted, nil
}

// knows reports whether this rail issued the reference. The caller holds mu.
func (r *Rail) knows(ref string) bool {
	for _, issued := range r.submitted {
		if issued == ref {
			return true
		}
	}
	return false
}

// Fail marks a submitted payment as one this rail accepted and could not
// complete, so a test can drive the other far end of the journey.
//
// Not the same as [WithPermanentFailure], and the difference is the whole
// reason both exist: that one refuses the SUBMISSION, so no payment was ever
// made and the money never moved; this one is a payment that was made and
// ended badly. A caller learns the first from Submit and the second only by
// asking Status.
//
// Not part of the port, like [Rail.Settle]: a real rail is told by its bank.
func (r *Rail) Fail(reference payout.RailReference) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.knows(reference.Ref()) {
		return fmt.Errorf("stub: nothing was submitted under %s, so it cannot fail", reference)
	}
	r.broke[reference.Ref()] = true
	return nil
}

// Settle marks a submitted payment as having reached the member, so a test
// can drive the far end of the journey. It is not part of the port: a real
// rail is told by its bank, and nothing in the domain may call this.
func (r *Rail) Settle(reference payout.RailReference) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.knows(reference.Ref()) {
		return fmt.Errorf("stub: nothing was submitted under %s, so it cannot settle", reference)
	}
	r.settled[reference.Ref()] = true
	return nil
}
