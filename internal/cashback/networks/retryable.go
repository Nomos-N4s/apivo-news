// This file is the vocabulary an adapter uses to say what one failure was
// worth. It holds [RetryableError] and the only constructor for it, the
// unexported terminal marking [NewTerminalError] builds to veto one, and
// the two readers - [RetryableHTTPStatus] and [RetryAfterFromHeader] -
// that turn an ordinary HTTP refusal into either. It is one file because
// the marking, the veto that overrides it and the ask it carries are a
// single contract: classification is the caller's judgement, and these are
// the words it is given to make it in. Nothing here decides how long to
// wait - backoff.go does that, reading these markings.

package networks

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The sentinels the retryable-failure vocabulary reports: a Retry-After
// nobody can read, and a retryable failure built with nothing to retry.
// Reach for these with errors.Is rather than matching on message text, so
// the wording stays free to improve.
var (
	// ErrRetryAfterUnreadable reports a Retry-After header that was
	// present and could not be read as a delay this file will honour. It
	// is deliberately distinct from the network asking for nothing,
	// because the two call for different behaviour: nothing means the
	// adapter's own backoff is the whole answer, while unreadable means a
	// host tried to tell us how long to stay away and we did not
	// understand it - which is worth a log line and a metric now, rather
	// than a ban later that nothing recorded the cause of.
	ErrRetryAfterUnreadable = errors.New("networks: Retry-After header is not a delay we can honour")
	// ErrRetryableNoCause reports a failure marked retryable with nothing
	// underneath it. [RetryBackoff.Do] carries the last failure out with
	// every verdict precisely so an operator can tell a refusing network
	// from a spent budget; a marking with no cause would leave that
	// operator a give-up message whose entire diagnosis is the word
	// "retryable", which is the one outcome the marking exists to
	// prevent.
	ErrRetryableNoCause = errors.New("networks: a retryable failure carries no cause")
)

const (
	// retryAfterAskCeiling bounds what a network may ask for in a
	// Retry-After header. A network may reasonably ask to be left alone
	// for a day; a header that asks for a year is broken or hostile, and
	// converting it unbounded would overflow the arithmetic below. The
	// bound matches internal/ingestion's maxRetryAfterAsk, for the same
	// reason and to the same number.
	retryAfterAskCeiling = 24 * time.Hour
)

// RetryableError marks a failure worth trying again, and carries the wait
// the network asked for when it asked for one. [RetryBackoff.Do] retries
// exactly the failures wrapped in one of these and returns everything else
// untouched.
//
// Classification is the caller's, not this file's, and deliberately so: an
// adapter knows which of its network's refusals are transient, and a
// generic rule guessing from an HTTP status would either retry a rejected
// credential forever or give up on a transient one. [RetryableHTTPStatus]
// and [RetryAfterFromHeader] exist so the ordinary HTTP case is one line
// rather than a judgement call re-made per adapter.
//
// Its fields are unexported and [NewRetryableError] is the only way to
// build one, for the reason wallet.AccountRef gives for the same choice: a
// value that can be half-built eventually is. The plausible one-liner -
// marking a refusal with the ask filled in from the header and the cause
// forgotten, because in the author's head the status *was* the diagnosis -
// would leave an operator reading a five-attempt give-up with "retryable:
// <nil>" as the whole of it.
type RetryableError struct {
	err        error
	retryAfter time.Duration
}

// NewRetryableError marks err as worth trying again, carrying retryAfter
// as the wait the network asked for (zero when it asked for none).
//
// A nil err is refused rather than marked: the error returned instead
// wraps [ErrRetryableNoCause] and is deliberately *not* retryable, so a
// caller that lost its diagnosis gets one attempt and a message naming the
// mistake, rather than a whole budget spent against a network nobody can
// afterwards show was refusing anything. A negative ask is read as no ask:
// the header parser cannot produce one, so it can only come from
// arithmetic that went wrong, and a negative wait is a wait that is not
// taken.
func NewRetryableError(err error, retryAfter time.Duration) error {
	if err == nil {
		return ErrRetryableNoCause
	}
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &RetryableError{err: err, retryAfter: retryAfter}
}

// RetryAfter reports the wait the network asked for, zero when it asked
// for none.
func (e *RetryableError) RetryAfter() time.Duration { return e.retryAfter }

// Error describes the failure and the ask, if there was one.
func (e *RetryableError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("retryable after %v: %v", e.retryAfter, e.err)
	}
	return fmt.Sprintf("retryable: %v", e.err)
}

// Unwrap exposes the underlying failure, so errors.Is and errors.As see
// through the marking to the diagnosis.
func (e *RetryableError) Unwrap() error { return e.err }

// retryTerminalError marks a failure as final whatever is underneath it.
// It is unexported because the only things a caller does with one are
// build it ([NewTerminalError]) and see through it with errors.Is, and a
// type nobody has to name is a type whose shape nobody can come to depend
// on.
type retryTerminalError struct {
	err error
}

// NewTerminalError marks err as not worth trying again, even when
// something it wraps is marked retryable.
//
// It exists because a classification with no veto is a one-way ratchet. An
// adapter's transport helper marks every 5xx retryable, which is right at
// that layer, because at that layer a 503 is all there is to see; the
// caller then reads the body, finds the account suspended, and knows the
// condition is permanent. Without this it cannot say so - wrapping with %w
// leaves the [RetryableError] reachable and errors.As still finds it, so
// the full budget is spent against an account that will still be suspended
// at the end of it - and its only escape is to build a fresh error, which
// discards exactly the transport diagnosis the operator needs.
//
// [RetryBackoff.Do] looks for this marking before it looks for a retryable
// one, so a terminal marking anywhere in the chain wins. That direction is
// deliberate: getting it wrong in this direction costs one attempt and a
// poll cycle, and getting it wrong in the other costs a full budget of
// requests aimed at a host that is refusing us.
func NewTerminalError(err error) error {
	if err == nil {
		return nil
	}
	return &retryTerminalError{err: err}
}

// Error describes the failure and the verdict on it.
func (e *retryTerminalError) Error() string { return "terminal: " + e.err.Error() }

// Unwrap exposes the underlying failure, so errors.Is and errors.As see
// through the marking to the diagnosis.
func (e *retryTerminalError) Unwrap() error { return e.err }

// RetryableHTTPStatus reports whether an HTTP status is one worth meeting
// again with the same request.
//
// The set is 429 - the rate limit this whole file exists for - plus 408,
// which is a host saying in as many words that it gave up waiting, plus
// the 5xx range, which is the host reporting its own trouble rather than
// ours. 501 is excluded from that range because "not implemented" is a
// statement about the endpoint and will be just as true in thirty seconds.
// Every other 4xx is the request's own fault - a bad key, a malformed
// window, a merchant that does not exist - and retrying one is a way of
// turning a clear error into a slow one. That includes 499, which is not
// an HTTP status at all but nginx recording that the *client* hung up:
// retrying it asks the same question of a host that never answered the
// first one because we stopped listening.
func RetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout:
		return true
	case http.StatusNotImplemented:
		return false
	default:
		return status >= 500 && status <= 599
	}
}

// RetryAfterFromHeader reads the wait a network asked for. It reports
// three outcomes, not two, and the third is the point of the signature:
//
//   - (0, nil) - the header was absent, or asked for no wait at all. The
//     adapter's own backoff is the whole answer.
//   - (d, nil) - the network asked for d, bounded by retryAfterAskCeiling.
//   - (0, error wrapping [ErrRetryAfterUnreadable]) - the header was
//     present and we could not read it.
//
// Collapsing the third into the first is what a bare duration return would
// do, and it is the difference between correct behaviour and a ban. Only
// the delta-seconds form is read, matching what internal/ingestion and
// internal/translation already do with this header: the HTTP-date form
// would mean trusting the network's clock against ours to decide how long
// to wait. But the HTTP-date form is legal, and CDN and WAF front-ends do
// emit it on a 429 - so a host putting one in front of the reference
// network would have every "stay away for five minutes" read as "no ask",
// and with a bare duration nothing could count, log or alert on that,
// because the return value carried no way to tell it from silence. The
// error is that discriminant. An adapter that does not care may still
// ignore it and use the zero.
func RetryAfterFromHeader(header http.Header) (time.Duration, error) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		// An infinity is refused beside the unparsable rather than
		// clamped to the ceiling: "Inf" is not an ask a host meant to
		// make, and reading it as "stay away for a day" is a guess
		// dressed as an instruction. This is also what the repository's
		// existing parser in internal/translation does with it.
		return 0, fmt.Errorf("networks: Retry-After %q is not a delta-seconds value: %w", value, ErrRetryAfterUnreadable)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("networks: Retry-After %q asks for a negative wait: %w", value, ErrRetryAfterUnreadable)
	}
	// Bounded in float space, before the conversion: an ask large enough
	// to overflow int64 nanoseconds would otherwise convert to a negative
	// duration, which reads as "no ask" and would have us back on the
	// wire immediately.
	if seconds >= retryAfterAskCeiling.Seconds() {
		return retryAfterAskCeiling, nil
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
