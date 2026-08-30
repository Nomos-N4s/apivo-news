// This file is the retry policy: [RetryBackoffPolicy] with the defaults
// its zero fields take, [RetryBackoff] with its injected clock, jitter
// source and on-retry hook, the full-jitter [RetryBackoff.Delay] that
// turns a failed attempt number into the wait before the next one, and
// [RetryBackoff.Do], which spends that wait against two ceilings while
// reading the markings retryable.go defines. It is one file because the
// policy, the wait it produces and the loop that spends it are one budget:
// split up, an adapter wanting the whole of it would re-make the attempt
// ceiling, the elapsed ceiling and the classification for itself, which is
// the per-adapter judgement call this type exists to prevent.

package networks

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// The sentinels a retry policy refuses with: a policy that could never
// terminate, a budget already spent, and an attempt number a zero-based
// caller would have passed. Reach for these with errors.Is rather than
// matching on message text, so the wording stays free to improve.
var (
	// ErrRetryBackoffPolicy reports a policy whose numbers contradict each
	// other or run backwards - a negative delay, a ceiling below the base.
	// Zero fields are not this error: they take the documented defaults,
	// because "unset" is a legitimate thing for a caller to mean and
	// "negative" is not.
	ErrRetryBackoffPolicy = errors.New("networks: retry policy is not usable")
	// ErrRetryBudgetExhausted reports a retry sequence that ran out of
	// attempts or out of time with the operation still failing. It is
	// deliberately distinct from the failure underneath it, which is
	// wrapped alongside: an operator needs to tell "the network is
	// refusing us" from "we stopped asking", because only the second is
	// a reason to widen the budget.
	ErrRetryBudgetExhausted = errors.New("networks: retry budget exhausted")
	// ErrRetryAttemptNumber reports a [RetryBackoff.Delay] asked for an
	// attempt below one. Attempt numbers are one-based, and the two
	// conventions are not interchangeable: a caller counting from zero
	// would be handed the base ceiling for its first two waits, so its
	// whole escalation would run one step behind and put an extra early
	// request into a host that is already refusing us. Refusing the
	// argument is what turns that into a failure on the first call rather
	// than a schedule that is quietly wrong forever.
	ErrRetryAttemptNumber = errors.New("networks: retry attempt numbers are one-based")
)

// Defaults applied to a [RetryBackoffPolicy]'s zero fields. They mirror the
// numbers the repository already settled on for outbound HTTP in
// internal/ingestion and internal/translation - a sub-second first wait, a
// ceiling of half a minute on any single wait - because a network adapter
// is the same kind of client talking to the same kind of host, and two
// different sets of numbers for one behaviour is two things to reason
// about instead of one.
const (
	// DefaultRetryBaseDelay is the ceiling on the wait before the second
	// attempt; every further ceiling doubles it.
	DefaultRetryBaseDelay = 500 * time.Millisecond
	// DefaultRetryDelayCap is the ceiling no single wait exceeds, however
	// many attempts have failed. Without it the doubling reaches hours,
	// and a poll cycle that sleeps for hours is indistinguishable from a
	// poll cycle that died.
	DefaultRetryDelayCap = 30 * time.Second
	// DefaultRetryAttempts is the total number of attempts, the first
	// included. Small on purpose: a window that will not fetch now is
	// re-read by the next scheduled cycle, and FR-031's promise is that no
	// window is lost or double-counted across restarts - so giving up
	// early costs a delay, never data.
	DefaultRetryAttempts = 5
	// DefaultRetryElapsed bounds the whole sequence in wall-clock terms,
	// which the attempt count alone cannot: one Retry-After of an hour
	// would otherwise be honoured inside a budget of five attempts.
	DefaultRetryElapsed = 5 * time.Minute
)

// RetryBackoffPolicy is the shape of one adapter's retry budget: how long
// the waits may grow, and when to stop growing them and give up. Every
// field may be left zero, in which case the matching Default constant
// applies; a negative field is refused, because "unset" and "minus one
// second" are different mistakes and only the first has an obvious
// intention behind it.
type RetryBackoffPolicy struct {
	// BaseDelay is the ceiling on the wait before the second attempt.
	// Each subsequent ceiling doubles it, up to DelayCap.
	BaseDelay time.Duration
	// DelayCap is the ceiling no single wait exceeds. Zero takes the
	// larger of [DefaultRetryDelayCap] and BaseDelay, so raising the base
	// alone can never produce a policy that contradicts itself.
	DelayCap time.Duration
	// MaxAttempts is the total number of attempts, the first included. It
	// is one of the two ceilings the contract asks for; the other is
	// MaxElapsed, and both are needed because a single honoured
	// Retry-After can spend more time than five ordinary waits.
	MaxAttempts int
	// MaxElapsed bounds the wall-clock span of the whole sequence,
	// measured from the first attempt, and it bounds both halves of that
	// span: a wait that would leave nothing of the budget for the attempt
	// it precedes is not slept, and every attempt runs under a context
	// carrying whatever is left. Bounding only the waits would make this
	// field a promise it could not keep - a network that accepts the
	// connection and never answers would hold [RetryBackoff.Do] open for
	// as long as the adapter's own transport allowed, while the budget it
	// claimed to be working to had long since expired.
	//
	// It is a ceiling on this sequence, not a substitute for a per-request
	// timeout: an adapter that wants an individual request cut off sooner
	// than the whole budget still sets that on its own client.
	MaxElapsed time.Duration
}

// withDefaults fills the zero fields and reports a policy that cannot be
// made sensible. Ordering matters: DelayCap defaults against the base that
// is already in place, so a caller who set only a large base gets a
// ceiling above it rather than a refusal.
func (p RetryBackoffPolicy) withDefaults() (RetryBackoffPolicy, error) {
	switch {
	case p.BaseDelay < 0:
		return p, fmt.Errorf("networks: base delay %v must not be negative: %w", p.BaseDelay, ErrRetryBackoffPolicy)
	case p.DelayCap < 0:
		return p, fmt.Errorf("networks: delay cap %v must not be negative: %w", p.DelayCap, ErrRetryBackoffPolicy)
	case p.MaxAttempts < 0:
		return p, fmt.Errorf("networks: max attempts %d must not be negative: %w", p.MaxAttempts, ErrRetryBackoffPolicy)
	case p.MaxElapsed < 0:
		return p, fmt.Errorf("networks: max elapsed %v must not be negative: %w", p.MaxElapsed, ErrRetryBackoffPolicy)
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = DefaultRetryBaseDelay
	}
	if p.DelayCap == 0 {
		p.DelayCap = max(DefaultRetryDelayCap, p.BaseDelay)
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultRetryAttempts
	}
	if p.MaxElapsed == 0 {
		p.MaxElapsed = DefaultRetryElapsed
	}
	if p.DelayCap < p.BaseDelay {
		return p, fmt.Errorf("networks: delay cap %v is below base delay %v, so the first wait would already be capped: %w", p.DelayCap, p.BaseDelay, ErrRetryBackoffPolicy)
	}
	return p, nil
}

// RetryBackoff turns a failed request into the wait before the next one.
// It exists to solve a problem a [RateLimiter] cannot: the limiter paces a
// healthy adapter, and this paces an unhealthy one, which is the case
// spec.md's US2 scenario 5 names - "the network API errors or rate-limits
// ... it backs off and retries without losing or double-counting a window".
//
// # Full jitter, and why the multiplier alone is a bug
//
// The wait before retry n is drawn uniformly from [0, ceiling(n)), where
// ceiling(n) is BaseDelay doubled n-1 times and clamped to DelayCap. That
// is "full jitter" from the well-known exponential-backoff-and-jitter
// analysis, and it is chosen over decorrelated jitter because the failure
// being prevented here is a thundering herd rather than slow recovery of a
// single client: every poller replica that met the same 429 at the same
// moment must come back at *different* moments, and full jitter is the
// variant that spreads them across the entire window instead of clustering
// them near its end.
//
// A fixed multiplier - the same doubling with no randomness - is not a
// milder version of this. Two replicas that failed together would retry
// together, forever, at exactly the instants their shared schedule
// dictates, converting one rate-limit response into a synchronised burst
// that earns the next one. The randomness is the mechanism, not a
// refinement of it.
//
// # Honouring Retry-After
//
// When the failure carries a wait the network asked for, that ask becomes
// a floor and the jitter is added on top rather than replacing it.
// Retrying before the network's own deadline is a request guaranteed to
// fail and to count against us; retrying at exactly that deadline puts
// every client that read the same header back on the wire in the same
// millisecond, which is the herd again. Floor plus jitter obeys the
// instruction and still arrives spread out.
//
// # Determinism
//
// Both the clock and the randomness are injected
// ([WithRetryBackoffClock], [WithRetryBackoffRandom]), so a test asserts
// the backoff *sequence* exactly rather than observing that something
// slept. A jitter whose bound is only claimed in a comment is a jitter
// nobody has checked.
//
// Like [RateLimiter], this type names no Limits and no port type: it takes
// a policy of plain durations and counts, and the poller adapts whatever
// the port declares into one.
//
// The zero value is not usable. Build one with [NewRetryBackoff].
type RetryBackoff struct {
	policy RetryBackoffPolicy
	clock  RateLimitClock
	// random supplies the jitter fraction in [0, 1). A field so tests can
	// pin it, matching how internal/platform/scheduler and
	// internal/ingestion pin theirs. Whatever it returns is bounded by
	// retryBackoffFraction before it is used, because an injected source
	// is an input like any other.
	random func() float64
	// onRetry is told about each failed attempt before its wait. Never
	// nil - an unset one observes nothing - so the call site needs no
	// guard.
	onRetry func(attempt int, wait time.Duration, err error)
}

// RetryBackoffOption configures the backoff [NewRetryBackoff] returns.
type RetryBackoffOption func(*RetryBackoff)

// WithRetryBackoffClock replaces the source of time and of waiting, so a
// test can assert both the delays chosen and the elapsed budget without a
// real second passing. A nil clock keeps the default.
func WithRetryBackoffClock(clock RateLimitClock) RetryBackoffOption {
	return func(b *RetryBackoff) {
		if clock != nil {
			b.clock = clock
		}
	}
}

// WithRetryBackoffRandom replaces the jitter source. It is what makes the
// sequence assertable: a test hands in a scripted fraction and knows the
// exact duration that must come back. A nil source keeps the default -
// installing none would freeze the jitter at zero and quietly reintroduce
// the synchronised retry the jitter exists to prevent.
//
// A source is expected to return values in [0, 1), and one that does not
// is bounded rather than believed; see [retryBackoffFraction] for what
// happens to a source that goes outside its range, and why the range is
// not left to prose.
func WithRetryBackoffRandom(random func() float64) RetryBackoffOption {
	return func(b *RetryBackoff) {
		if random != nil {
			b.random = random
		}
	}
}

// WithRetryBackoffOnRetry installs a function told about each failed
// attempt: which attempt it was, how long the sequence is about to wait,
// and what the failure was. It runs after the wait is chosen and before it
// is slept, once per retry, and never for the attempt that ends the
// sequence.
//
// It is here because [RetryBackoff.Do] is an iteration whose intermediate
// outcomes are otherwise unreachable: only the last failure survives, and
// the wait is chosen inside the loop, so "attempt 2 of 5 failed with 503,
// retrying in 412ms" - the line that lets an operator watch a network
// degrade before it fails outright - cannot be written by any caller. The
// alternative was for an adapter that wanted that line to abandon Do and
// drive [RetryBackoff.Delay] itself, which means re-implementing the
// attempt ceiling, the elapsed ceiling, the retryable classification and
// the sentinel, once per adapter: exactly the judgement call re-made per
// adapter this file exists to prevent.
//
// The function runs on the caller's goroutine, so a slow one lengthens the
// sequence it is describing. A nil function keeps the default of observing
// nothing.
func WithRetryBackoffOnRetry(onRetry func(attempt int, wait time.Duration, err error)) RetryBackoffOption {
	return func(b *RetryBackoff) {
		if onRetry != nil {
			b.onRetry = onRetry
		}
	}
}

// NewRetryBackoff builds a backoff from policy, filling its zero fields
// with the documented defaults and refusing a policy that contradicts
// itself.
func NewRetryBackoff(policy RetryBackoffPolicy, opts ...RetryBackoffOption) (*RetryBackoff, error) {
	resolved, err := policy.withDefaults()
	if err != nil {
		return nil, err
	}
	b := &RetryBackoff{
		policy: resolved,
		clock:  rateLimitSystemClock{},
		//nolint:gosec // G404: the jitter spreads retries; it guards nothing.
		random:  rand.Float64,
		onRetry: func(int, time.Duration, error) {},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// Policy reports the policy in force, defaults resolved. It is here so an
// operator-facing log line, or a test, can state the budget it is working
// to rather than restating the constants and hoping they still match.
func (b *RetryBackoff) Policy() RetryBackoffPolicy { return b.policy }

// retryBackoffFraction bounds a jitter source's output to the [0, 1) that
// [WithRetryBackoffRandom] documents.
//
// The bound is code rather than prose because both ways out of the range
// fail silently and catastrophically. A negative fraction makes
// [RetryBackoff.Delay] return a negative duration; every clock in this
// file treats a non-positive wait as no wait at all, and
// [RetryBackoff.Do]'s elapsed check is made *smaller* by it, so the whole
// retry sequence runs as an unpaced burst against a host that is already
// refusing us - the one behaviour this file exists to prevent, produced by
// the mechanism that exists to prevent it. A fraction of one or more
// scales the wait past its own documented ceiling and, on a large ceiling,
// overflows the float-to-int conversion back into a negative duration by
// the other road. Every other input this file takes - the rate, the burst,
// four policy fields - is refused when it cannot work; this one was left
// to a sentence in a doc comment.
//
// It is bounded rather than refused because the signature has nowhere to
// put a refusal, and because the safe answer is obvious: the nearest legal
// fraction. NaN, which compares false against every bound, lands on zero
// through the negated test below rather than travelling into the
// multiplication.
func retryBackoffFraction(fraction float64) float64 {
	if !(fraction > 0) {
		return 0
	}
	if fraction >= 1 {
		return math.Nextafter(1, 0)
	}
	return fraction
}

// Delay is the wait before the attempt following a failed attempt number
// n (one-based), given the wait the network asked for in a Retry-After, or
// zero when it asked for none. An attempt below one is refused wrapping
// [ErrRetryAttemptNumber] rather than treated as the first: a caller
// counting from zero would otherwise get the base ceiling twice and run
// its whole escalation one step behind, which no test of its own could
// see.
//
// It is pure apart from the injected randomness, which is what lets a test
// assert the whole sequence: with the jitter fraction pinned, the returned
// durations are a fixed list of numbers, and both the doubling and the cap
// are visible in it.
//
// The ask is clamped to retryAfterAskCeiling before anything is added to
// it, so a broken or hostile header cannot overflow the arithmetic. It is
// not otherwise capped here: whether an honest but very long ask is worth
// waiting for is a budget question, and [RetryBackoff.Do] answers it
// against MaxElapsed rather than silently shortening what the network
// asked for.
func (b *RetryBackoff) Delay(attempt int, retryAfter time.Duration) (time.Duration, error) {
	if attempt < 1 {
		return 0, fmt.Errorf("networks: attempt %d: %w", attempt, ErrRetryAttemptNumber)
	}
	// The doubling is a loop that stops at the cap rather than a shift,
	// and it clamps *before* the multiplication rather than after it. A
	// ceiling already past half the cap can only double into a number
	// above the cap, so clamping it there costs nothing - and if the cap
	// is large enough, the doubling it replaces would have overflowed
	// int64 into a negative ceiling, which is a wait of no time at all
	// dressed as a very long one.
	ceiling := b.policy.BaseDelay
	for i := 1; i < attempt && ceiling < b.policy.DelayCap; i++ {
		if ceiling > b.policy.DelayCap/2 {
			ceiling = b.policy.DelayCap
			break
		}
		ceiling *= 2
	}
	ceiling = min(ceiling, b.policy.DelayCap)

	jitter := time.Duration(retryBackoffFraction(b.random()) * float64(ceiling))
	if retryAfter <= 0 {
		return jitter, nil
	}
	// Saturating rather than wrapping, for the same reason the conversion
	// above is bounded before it happens: an ask clamped to a day, added
	// to a jitter drawn from a ceiling near the end of the duration
	// range, would otherwise sum to a negative wait - which every clock
	// here reads as "no wait" and puts us straight back on the wire.
	ask := min(retryAfter, retryAfterAskCeiling)
	if jitter > rateLimiterMaxWait-ask {
		return rateLimiterMaxWait, nil
	}
	return ask + jitter, nil
}

// Do runs op, retrying it under the policy for as long as op keeps failing
// with a [RetryableError] and the budget allows another attempt.
//
// It returns nil on the first success, the failure itself when op fails
// with anything not marked retryable or marked terminal, ctx.Err() wrapped
// alongside the last failure when the context ends, and
// [ErrRetryBudgetExhausted] wrapped alongside the last failure when the
// attempts or the elapsed ceiling run out. The last failure travels in
// every one of those cases because an operator reading "gave up" still
// needs to know what was refusing us.
//
// Every attempt runs under a context bounded by what remains of
// MaxElapsed, so the budget is a promise about when the caller gets an
// answer rather than only about how long this type is willing to sleep.
//
// Do does not pace successful calls: that is [RateLimiter.Wait]'s job.
// The composition a poller wants is the limiter *inside* the attempt -
// backoff.Do(ctx, limiter.Pace(fetch)) - because Do re-issues op, and a
// limiter placed outside it would spend one token on the whole retry
// sequence. [RateLimiter.Pace] exists to make that composition the easy
// one to write.
func (b *RetryBackoff) Do(ctx context.Context, op func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := b.clock.Now()

	var lastErr error
	for attempt := 1; ; attempt++ {
		remaining := b.policy.MaxElapsed - b.clock.Now().Sub(started)
		if remaining <= 0 {
			// Only reachable when the attempts themselves spent the
			// budget: a wait is measured against it before it is
			// slept, so nothing else can carry the sequence here.
			if lastErr == nil {
				return fmt.Errorf("networks: the %v budget was gone before the first attempt: %w", b.policy.MaxElapsed, ErrRetryBudgetExhausted)
			}
			return fmt.Errorf("networks: the %v budget ran out inside attempt %d: %w (last failure: %w)", b.policy.MaxElapsed, attempt-1, ErrRetryBudgetExhausted, lastErr)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		lastErr = op(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		// Cancellation observed after a failed attempt is cancellation,
		// not a verdict on the network: reporting it as a budget failure
		// would have an operator widening a budget that was never the
		// problem.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("networks: retry abandoned after attempt %d: %w (last failure: %w)", attempt, err, lastErr)
		}

		var terminal *retryTerminalError
		if errors.As(lastErr, &terminal) {
			return lastErr
		}
		var retryable *RetryableError
		if !errors.As(lastErr, &retryable) {
			return lastErr
		}
		if attempt >= b.policy.MaxAttempts {
			return fmt.Errorf("networks: %d attempt(s) all failed: %w (last failure: %w)", attempt, ErrRetryBudgetExhausted, lastErr)
		}

		wait, err := b.Delay(attempt, retryable.RetryAfter())
		if err != nil {
			return fmt.Errorf("networks: choosing the wait after attempt %d: %w (last failure: %w)", attempt, err, lastErr)
		}
		// The budget is checked against the wait *before* sleeping it,
		// and a wait that would consume all of what is left fails the
		// check too: sleeping it would spend the remainder of the budget
		// on waiting and leave none of it for the asking, which is a
		// ceiling honoured by wasting the thing it protects.
		if elapsed := b.clock.Now().Sub(started) + wait; elapsed >= b.policy.MaxElapsed {
			return fmt.Errorf("networks: waiting %v after attempt %d would carry the sequence to %v, at or past the %v budget: %w (last failure: %w)", wait, attempt, elapsed, b.policy.MaxElapsed, ErrRetryBudgetExhausted, lastErr)
		}
		b.onRetry(attempt, wait, lastErr)
		if err := b.clock.Sleep(ctx, wait); err != nil {
			return fmt.Errorf("networks: retry wait interrupted after attempt %d: %w (last failure: %w)", attempt, err, lastErr)
		}
	}
}
