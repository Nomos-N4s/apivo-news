// How iteration ends and how a failure against a network is classified:
// [AbandonedIteration] for an answer that is not whole, and the sentinels
// that tell a blip from a wall. One file, because they answer one question -
// may the caller advance its cursor, and may it try the window again?

package networks

import (
	"errors"
	"fmt"
)

// The sentinels an iteration reports when it ends for its own reason, and
// the classification a caller retries on. Which one a failure carries decides
// whether re-running the window can ever succeed.
var (
	// ErrIterationAbandoned reports iteration that ended before the answer
	// was whole - a cancelled context, a shutting-down process, an exhausted
	// retry budget. It is yielded by adapters rather than produced here, and
	// it exists because the alternative is silence: a range loop that ends
	// having yielded no error is the caller's ONLY evidence that a window was
	// read to the end, and that evidence is what a durable cursor advances on
	// (contract rule 4, FR-031). An adapter that simply returned on
	// ctx.Err() would let a poller record a window it read half of, and every
	// member owed cashback on the other half would never be credited, with
	// nothing logged. [AbandonedIteration] builds the value; contract rule 8
	// is the rule.
	ErrIterationAbandoned = errors.New("networks: iteration ended before the answer was whole")
	// ErrNetworkUnavailable reports a network that could not be reached, or
	// that failed in a way which may clear on its own. RETRYABLE: the same
	// window, run again later, is the right response.
	//
	// It is returned by adapters, not by validation here - only an adapter
	// knows what its network's 503 looks like - and is defined in the port so
	// that every adapter classifies the same failure the same way and the
	// conformance suite can assert on it. The sibling PayoutRail port
	// classifies its failures for the same reason (contracts/ports.md section
	// 3); this port needs it more, because rule 4 offers no resumption point
	// inside a window, so the only response to a mid-window failure is to run
	// the whole window again - which is correct for this class and an
	// infinite loop for the next one.
	ErrNetworkUnavailable = errors.New("networks: the network could not be reached")
	// ErrNetworkRefused reports a network that refused us and will keep
	// refusing until somebody changes something outside this process: a
	// revoked or rejected credential, an account without permission for what
	// was asked. TERMINAL: re-running the window produces the same refusal
	// forever, so the poller stops, leaves its cursor where it is and raises
	// the account to an operator rather than looping. Without this class a
	// revoked Awin credential reads as "network error" for a condition that
	// will never clear, and ingestion halts silently.
	ErrNetworkRefused = errors.New("networks: the network refused this publisher account")
	// ErrNetworkRateLimited reports a request the network refused for being
	// asked too often. RETRYABLE after waiting, which is what distinguishes
	// it from [ErrNetworkUnavailable] - it says the pacing was wrong rather
	// than that anything is broken, and it is the signal the per-adapter
	// limiter needs to widen its own interval rather than merely re-running
	// the window at the same rate (contract rule 3).
	ErrNetworkRateLimited = errors.New("networks: the network refused this request as too frequent")
)

// AbandonedIteration is the terminal error an adapter yields when it stops
// before the answer is whole: a cancelled context, a shutting-down process,
// a retry budget spent against the network. It wraps [ErrIterationAbandoned]
// and the reason, so a caller learns both that the answer is partial and why.
//
// It exists because a range-over-func has no other way to say so. A loop that
// ends having yielded no error is the caller's only evidence that a window
// was read to the end, and contract rule 4 makes that evidence the trigger
// for advancing a durable cursor. An adapter that answered a cancelled
// context with a bare return - the idiomatic thing to write - would hand a
// poller a half-read window that looks exactly like a whole one: the cursor
// moves past the transactions never fetched, every member owed cashback on
// them is silently never credited, and nothing is logged. Yielding this once
// and returning turns that into an error the caller cannot miss.
//
// Contract rule 8 is the obligation; this is the value that satisfies it.
func AbandonedIteration(cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: the adapter gave no reason", ErrIterationAbandoned)
	}
	return fmt.Errorf("%w: %w", ErrIterationAbandoned, cause)
}
