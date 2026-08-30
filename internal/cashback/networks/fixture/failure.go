// [Failure], the network failure a fixture can be told to report, and the
// injector that hands it out. One file, because the knob and the port's
// contract rule 9 classification are the same idea: a caller cannot be shown
// to tell a blip from a wall unless something can produce one on demand.

package fixture

import (
	"fmt"
	"sync"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// Failure names a network failure this adapter can be told to report instead
// of answering from the recording. It exists because contract rule 9's
// classification is otherwise untestable: the retry paths in the conformance
// suite (T051), in the limiter (T056) and in the poller all branch on which
// of the port's three sentinels came back, and there is no way to make a real
// network produce one of them to order.
//
// The three are not interchangeable, and the distinction is the reason this
// knob names them separately rather than offering one "make it fail". Two of
// them mean run the same window again - rule 4 offers no resumption point
// inside a window, so re-running the whole thing is the only response
// available - and the third means stop, leave the cursor where it is, and
// raise the publisher account to a human. Getting that wrong in either
// direction is expensive: retrying a revoked credential is an infinite loop
// with a frozen cursor, and giving up on a 503 loses every transaction in the
// window until somebody notices.
//
// The zero value is [FailureNone], so a fixture nobody configured answers
// from the recording.
type Failure string

const (
	// FailureNone is the absence of an injected failure: the adapter answers
	// from the recording. It is the zero value.
	FailureNone Failure = ""
	// FailureUnavailable makes the adapter report [networks.ErrNetworkUnavailable]:
	// the network could not be reached, or failed in a way that may clear on
	// its own. RETRYABLE - the same window, run again later.
	FailureUnavailable Failure = "unavailable"
	// FailureRateLimited makes the adapter report [networks.ErrNetworkRateLimited]:
	// the request was refused for being asked too often. RETRYABLE after
	// waiting, and the signal a limiter widens its own interval on rather
	// than merely re-running the window at the same pace (contract rule 3).
	FailureRateLimited Failure = "rate_limited"
	// FailureRefused makes the adapter report [networks.ErrNetworkRefused]:
	// the publisher account was refused and will keep being refused until
	// somebody changes a credential. TERMINAL - a poller that retries this
	// one loops forever with a cursor that never moves.
	FailureRefused Failure = "refused"
)

// FailureAlways is the count that means "every call until told otherwise",
// and is what a terminal failure actually looks like: a revoked credential
// does not clear itself after three attempts. A finite count is for the other
// two, where the point of the test is that the caller retried and eventually
// got through.
const FailureAlways = 0

// String names the failure, so a test that got the wrong one says which.
func (f Failure) String() string {
	if f == FailureNone {
		return "(none)"
	}
	return string(f)
}

// classify turns the knob's setting into the port's own sentinel, which is
// the only form a caller is ever allowed to see: contract rule 9 is a promise
// about which sentinel comes back, and an adapter that invented an error of
// its own here would satisfy the knob and break the contract.
//
// [FailureNone] has no error, and an unrecognised Failure is refused rather
// than defaulted, for the same reason a status word is (contract rule 2): a
// caller that asked for a failure this package does not have is asking for a
// retry decision nobody defined, and quietly handing back "unavailable" would
// make a test about terminal failures pass while testing the retryable path.
func (f Failure) classify() error {
	switch f {
	case FailureNone:
		return nil
	case FailureUnavailable:
		return networks.ErrNetworkUnavailable
	case FailureRateLimited:
		return networks.ErrNetworkRateLimited
	case FailureRefused:
		return networks.ErrNetworkRefused
	default:
		return fmt.Errorf("%w: the fixture was told to report %q, which names no failure this network classifies",
			networks.ErrNetworkUnavailable, string(f))
	}
}

// injector holds the failure the adapter has been told to report and how many
// more calls it applies to. It is the one piece of the adapter that a caller
// changes from outside a read, so it carries its own mutex rather than
// sharing the clock's: the two pieces of state are touched at different
// moments by different callers, and one lock over both would make a test that
// injects a failure while a read is in flight deadlock on itself.
type injector struct {
	mu   sync.Mutex
	kind Failure
	left int
}

// set records what to report and for how many more network-touching calls,
// where a count at or below [FailureAlways] means every call until set is
// called again.
func (i *injector) set(kind Failure, times int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.kind = kind
	i.left = times
}

// take reports the injected failure for one network-touching call, consuming
// one of a finite count, and returns nil when there is none.
//
// Consuming on TAKE rather than on success is deliberate: a caller driving a
// retry path wants "fail twice, then answer", and a counter that only came
// down when the call succeeded would never come down at all.
func (i *injector) take() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.kind == FailureNone {
		return nil
	}
	kind := i.kind
	if i.left > 0 {
		i.left--
		if i.left == 0 {
			i.kind = FailureNone
		}
	}
	return kind.classify()
}

// peek reports what is currently set without consuming it, for the tests and
// for a caller that wants to know whether it is about to be refused.
func (i *injector) peek() (Failure, int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.kind, i.left
}
