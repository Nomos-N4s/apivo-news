// What states an entry can be in, and which may follow which (T069, FR-042,
// D7).
//
// A file of its own because the table is the part a person reasons about, and
// it should be readable without the machinery that applies it. Everything
// here is pure: no database, no ledger, no clock. Whether a transition is
// LEGAL is answered here; whether it SUCCEEDED is answered by the two writes
// that carry it out.

package earnings

import "fmt"

// State is where an entry sits in its lifecycle (FR-042).
//
// The strings are the schema's, matched exactly against
// entry_state_known (0013). They are not an encoding detail: the same six
// words appear in entry.state, in entry_transition.from_state and to_state,
// and in the operator surface, and a seventh spelling anywhere would be a
// state the database refuses to store after the ledger has already moved.
type State string

const (
	// StateHeld is money credited under a hold rule and counted toward
	// nothing member-facing until an operator releases it (US7).
	StateHeld State = "held"
	// StatePending is money the network has reported but not confirmed.
	// Visible to the member, never spendable.
	StatePending State = "pending"
	// StateConfirmed is money the network has confirmed and reconciled: the
	// only state that counts toward the withdrawal threshold (FR-043).
	StateConfirmed State = "confirmed"
	// StateReserved is money a withdrawal request has claimed (D9).
	StateReserved State = "reserved"
	// StatePaid is money that has left for the member's destination.
	StatePaid State = "paid"
	// StateReversed is money the network took back. It is an end state: a
	// reversal is a new pair of postings and never an edit (SC-010), so
	// nothing follows it.
	StateReversed State = "reversed"
)

// Valid reports whether s is one of the six the schema stores.
func (s State) Valid() bool {
	_, known := transitions[s]
	return known
}

// String is the state as the schema spells it.
func (s State) String() string { return string(s) }

// transitions is the whole state machine: for each state, the states that may
// follow it. A state with an empty list is an end state.
//
// Written as data rather than as a switch because this is the design, and it
// should be possible to read the design in one screen and compare it with
// FR-042 without following control flow. It is also what makes Valid total:
// every state the schema knows is a key here, so a state missing from the
// table cannot silently be treated as legal-from-nowhere.
//
// THE ARROWS THAT ARE ABSENT ARE THE POINT.
//
//   - Nothing leaves reversed or paid. Both are end states, and the money has
//     already moved; a further transition would be an edit of a settled fact
//     (C-3), not a correction. A network that reverses after payout is Q3's
//     absorbed loss, recorded against the house, never a state change here.
//   - Confirmed never returns to pending. The network confirming and then
//     un-confirming is a reversal, which is what reversed is for; letting
//     money slide back would make a confirmed balance mean "confirmed for
//     now", and FR-043 rests on it meaning more than that.
//   - Reserved returns only to confirmed, and only when a withdrawal is
//     rejected or fails terminally (D8). It never goes to pending: the money
//     was confirmed before it was reserved and nothing about a failed payout
//     unconfirms it.
//   - Held goes to pending, not straight to confirmed. Releasing a hold says
//     the entry is ordinary again, not that the network has confirmed it -
//     those are two different facts and an operator can only assert the first.
var transitions = map[State][]State{
	StateHeld:      {StatePending, StateReversed},
	StatePending:   {StateHeld, StateConfirmed, StateReversed},
	StateConfirmed: {StateReserved, StateReversed},
	StateReserved:  {StateConfirmed, StatePaid, StateReversed},
	StatePaid:      {},
	StateReversed:  {},
}

// opening is the set of states an entry may be created in.
//
// An entry begins where the network's own report puts it: held when a rule
// caught it, pending otherwise. Never confirmed - that would mean Apivo
// decided a commission was final, which is the network's to say - and never
// reserved, paid or reversed, each of which describes something that has
// already happened to money this entry has not yet held.
var opening = map[State]bool{StateHeld: true, StatePending: true}

// CanOpen reports whether an entry may be created in this state.
func CanOpen(s State) bool { return opening[s] }

// CanFollow reports whether an entry in from may move to to.
//
// It answers false for a state that is not one of the six, and for a move to
// the state the entry is already in: entry_transition_states_differ refuses
// the second in the schema, and refusing it here as well is what turns a
// no-op retry into a caller's answer rather than a constraint violation deep
// inside a write.
func CanFollow(from, to State) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ErrIllegalTransition is what a refused move reports. It names both states,
// because "illegal transition" without them is a message somebody has to
// reproduce to act on.
type ErrIllegalTransition struct {
	From, To State
}

func (e ErrIllegalTransition) Error() string {
	switch {
	case !e.From.Valid():
		return fmt.Sprintf("earnings: %q is not a state an entry can be in", e.From)
	case !e.To.Valid():
		return fmt.Sprintf("earnings: %q is not a state an entry can move to", e.To)
	case len(transitions[e.From]) == 0:
		return fmt.Sprintf("earnings: an entry that is %s is settled and nothing follows it, so it cannot become %s", e.From, e.To)
	default:
		return fmt.Sprintf("earnings: an entry that is %s cannot become %s", e.From, e.To)
	}
}
