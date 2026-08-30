// [Stage], where the recorded lifecycle has got to, and the clock that moves
// it. One file, because the stage is meaningless without the rule that
// advances it: a fixture whose clock ran on wall time could never show a
// confirmation, and one whose clock ran on the CALL rather than on the
// completed read would break contract rule 4's resumability in the one place
// it matters.

package fixture

import (
	"strconv"
	"sync"
)

// Stage names how far the recorded lifecycle has been played: which of the
// four observed polls the next read of a transaction window will answer from.
// It is the fixture's entire notion of time.
//
// The four are the lifecycle the ingestion chain exists to handle, and they
// are what a poller and the supersede logic (T054) have to be demonstrated
// against: a click that the network has not yet joined to its sale, the
// pending transaction that attribution then appears on, the advertiser's
// validation, and the reversal that can follow it - which the port's Status
// type reaches from confirmed as well as from declined, and which is why a
// payout can outrun a reversal.
//
// The zero value is [StageClick] deliberately: a freshly built adapter is one
// nobody has polled yet, which is the beginning of the story rather than a
// missing value.
type Stage int

const (
	// StageClick is the first poll: the network reports the sale, and has not
	// yet joined it to the click reference the redirect carried. The
	// transaction is real, ours, and unattributable - which FR-034 makes
	// ordinary rather than an error, and which is exactly the state a poller
	// must be able to store without crediting anybody.
	StageClick Stage = iota
	// StagePending is the second poll: the same transaction, still awaiting
	// the advertiser, now carrying [RecordedClickRef]. A second transaction
	// has appeared in the same window, unattributed and staying that way.
	StagePending
	// StageApproved is the third poll: the advertiser validated the first
	// transaction. The second is re-reported completely unchanged, which is
	// US2 scenario 3 - polling again must create no duplicate.
	StageApproved
	// StageReversed is the fourth and last poll: the network took the first
	// transaction back, and the second was declined with both its amounts
	// restated. It is the end of the recording, and re-reading a window here
	// answers from it forever, because a real network goes on reporting a
	// reversal rather than forgetting it.
	StageReversed
)

// stageCount is how many observations the recording must hold. It is derived
// from the constants rather than written beside the files, so a recording
// that gained or lost an observation without the lifecycle being extended is
// refused by [newRecording] instead of being silently unreachable.
const stageCount = int(StageReversed) + 1

// Valid reports whether s names one of the four recorded polls. It exists for
// [WithStage], which is handed a value by a caller rather than by this
// package: a Stage is an int, and an adapter started at stage 9 would index
// past the recording on its first read.
func (s Stage) Valid() bool { return s >= StageClick && s <= StageReversed }

// String names the stage, so a test failure says which poll answered rather
// than printing an integer nobody can place.
func (s Stage) String() string {
	switch s {
	case StageClick:
		return "click"
	case StagePending:
		return "pending"
	case StageApproved:
		return "approved"
	case StageReversed:
		return "reversed"
	default:
		return "stage(" + strconv.Itoa(int(s)) + ")"
	}
}

// stageClock holds which observation the next read answers from, and is the
// one piece of the adapter that a read changes.
//
// It advances on a COMPLETED read and on nothing else. That is contract rule
// 4 in three lines: a caller that stopped halfway - because a write failed,
// or the process is shutting down - must be able to run that window again
// from the beginning and miss nothing, and it can only do that if the
// fixture has not moved on behind its back. A clock that ticked when
// FetchTransactions was CALLED would look identical in every happy-path test
// and would silently skip an observation for exactly the caller that needed
// it most.
type stageClock struct {
	mu sync.Mutex
	at Stage
}

// now reports the observation the next read answers from.
func (c *stageClock) now() Stage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// set moves the clock, and is how [WithStage] starts a fixture partway
// through the lifecycle so a test about confirmed money does not have to
// poll its way there first.
func (c *stageClock) set(s Stage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = s
}

// advanceFrom moves the clock one observation on, but only if it is still
// where the completed read found it, and never past the last observation.
//
// The comparison is what makes the clock right when two callers read at once:
// each advances only the stage it actually read, so two concurrent reads of
// the same stage move the lifecycle on by one rather than by two, and neither
// caller can skip an observation the other one consumed. Stopping at the last
// observation is deliberate too - a network does not forget a reversal, so a
// poller that keeps running finds the same final answer rather than falling
// off the end of the recording.
func (c *stageClock) advanceFrom(from Stage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == from && from < StageReversed {
		c.at = from + 1
	}
}
