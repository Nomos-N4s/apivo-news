// This file is the pacing half of the cashback network adapters, and it is
// deliberately the part that knows least. A [RateLimiter] keeps one adapter
// inside the request rate its network declared - ports.md section 2, rule
// 3, which the conformance suite checks as "rate-limit adherence under
// concurrency" - and a [RetryBackoff] decides how long to wait after a
// network errors or rate limits us, which is spec.md's US2 scenario 5: the
// poller "backs off and retries without losing or double-counting a
// window".
//
// Neither type mentions the Network port. They take plain numbers - a
// float64 of requests per second, a burst, a policy of durations - and the
// poller adapts whatever the port's Limits declares into them. That keeps
// the port, the poller and the pacing three separately-owned pieces rather
// than one thing that has to change together.
//
// The package's own doc comment lives in doc.go. This comment is
// deliberately detached from the package clause below, so that a reader
// looking for what this package is finds one statement of it rather than
// several, and so that adding this file beside the port and the poller
// never requires somebody to delete a paragraph by hand.

// This file is the token bucket. It holds [RateLimitClock] - the view of
// time both pacing types are given, so neither is at the mercy of a real
// second - the ceiling on any wait either of them may compute, and
// [RateLimiter] itself: construction, the two accessors that make rule 3
// checkable from outside an adapter, Wait, Pace, and the reservation
// arithmetic underneath them. It is one file because that arithmetic is
// only correct as a set: reserve takes the debt, restore gives back
// exactly the part refill has not already covered, and refillLocked is the
// single place the level moves. The retry policy is in backoff.go and the
// failure vocabulary it classifies is in retryable.go.

package networks

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// The sentinels a limiter refuses its own construction with: a rate or a
// burst that could never pace anything. A caller reaches for these with
// errors.Is rather than matching on message text, so the wording stays free
// to improve.
var (
	// ErrRateLimiterRate reports a request rate that cannot pace anything:
	// zero, negative, or not a finite number. A limiter built on one would
	// either divide by zero or compute an infinite wait, and both are
	// worse than refusing at construction - the failure would surface as
	// a poller that silently stopped fetching, hours after the mistake.
	ErrRateLimiterRate = errors.New("networks: rate limiter needs a positive, finite request rate")
	// ErrRateLimiterBurst reports a burst below one. A bucket that can
	// never hold a whole token grants nothing, ever; a caller asking for
	// one has confused burst with rate, and a limiter that quietly
	// deadlocked its poller would be the last place they looked.
	ErrRateLimiterBurst = errors.New("networks: rate limiter needs a burst of at least one request")
)

const (
	// rateLimiterMaxWait is the longest wait this file will ever compute.
	// It exists for overflow, not policy: a time.Duration is an int64 of
	// nanoseconds and cannot express more than about 292 years, and a
	// float-to-int conversion that overflows is implementation defined -
	// on amd64 it lands on math.MinInt64, a negative wait that would
	// grant the request immediately. That is the exact inversion of the
	// rule this file enforces, so every bound below is applied before the
	// conversion or the addition that could overflow, never after it.
	rateLimiterMaxWait = time.Duration(math.MaxInt64)
)

// RateLimitClock is the view of time a [RateLimiter] and a [RetryBackoff]
// have. It is an interface rather than a pair of function fields because
// pacing needs two operations that must agree with each other: reading the
// clock and waiting on it. A test that could replace only the reader would
// still be at the mercy of real sleeps, and a suite that asserts an exact
// backoff sequence cannot be at the mercy of anything.
//
// The repository already injects clocks for exactly this reason - see
// wallet.WithClock and its adapters - and this is that habit extended by
// the one operation a rate limiter needs and a ledger does not.
type RateLimitClock interface {
	// Now reports the current instant.
	Now() time.Time
	// Sleep waits for d, or until ctx ends, whichever comes first. It
	// returns ctx.Err() in the second case and nil in the first, and a
	// non-positive d returns without waiting at all. An implementation
	// that ignores ctx breaks the promise [RateLimiter.Wait] makes about
	// cancellation, which is the promise that keeps a shutdown from
	// waiting out a network's rate limit.
	Sleep(ctx context.Context, d time.Duration) error
}

// rateLimitSystemClock is the production clock: real time, real timers.
type rateLimitSystemClock struct{}

// Now reports the wall-clock instant.
func (rateLimitSystemClock) Now() time.Time { return time.Now() }

// Sleep waits on a timer, cancelled by ctx. The timer is stopped on every
// path, so a cancelled wait does not leave a timer alive until it fires.
func (rateLimitSystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Still an observation point for cancellation: a caller that
		// asked to wait for nothing while its context was already dead
		// should learn that here rather than proceed.
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RateLimiter paces one adapter's outbound requests so it never exceeds the
// rate that network declared (ports.md section 2, rule 3: FetchTransactions
// "never exceeds the declared request rate"). One limiter belongs to one
// adapter: rates are per network - the reference network allows six
// requests a second (research.md, D3) - and a limiter shared between two
// networks would either throttle the fast one or overrun the slow one.
//
// It is a token bucket, hand-rolled rather than taken from
// golang.org/x/time/rate: that module is not a dependency of this
// repository and adding one to pace a handful of HTTP calls is not a trade
// worth making. The bucket holds at most burst tokens, refills at the
// declared rate, and every granted request removes one. A request that
// finds the bucket empty takes its token anyway - driving the count
// negative - and waits out the debt, which is what makes concurrent
// callers queue rather than all wake to an empty bucket and race.
//
// Burst is the caller's decision and has no default, because there is no
// safe guess: a burst of one is strict spacing, which is what a limit
// published as "six requests per second" means if the network measures it
// over a sliding window, while a larger burst trades that safety for a
// faster start on a cold cycle. The conformance suite's rate-limit case
// counts grants per window under concurrency, and a burst is the only
// thing that legitimately lets more than rate*window grants through it.
//
// # Decoupling from the port
//
// This type deliberately knows nothing about the Network port. It takes
// plain numbers - a float64 of requests per second and an int of burst -
// and never mentions, imports or reflects on a Limits type. The reason is
// ownership: the port and its Limits belong to network.go, the poller
// belongs to poller.go, and a pacing primitive that named either would tie
// three separately-owned files into one change whenever a limit gained a
// field. The poller reads Limits() and adapts it into [NewRateLimiter];
// that adaptation is the poller's job and it is one line. What this type
// owes that arrangement is a way to check the adaptation from outside,
// which is [RateLimiter.Rate] and [RateLimiter.Burst].
//
// The zero value is not usable. Build one with [NewRateLimiter].
type RateLimiter struct {
	// perSecond is the declared rate: how fast the bucket refills. Set at
	// construction and never written again, so it is read without the
	// mutex below.
	perSecond float64
	// burst is the bucket's capacity, held as a float64 because every
	// comparison it takes part in is against a fractional token count.
	// Immutable after construction, like perSecond.
	burst float64
	// clock reads time and waits on it. A field rather than direct calls
	// to time.Now and time.Sleep so a test can drive both exactly
	// ([WithRateLimiterClock]).
	clock RateLimitClock

	// mu guards the two fields below. One mutex over the whole
	// reservation is what makes the rate promise true under concurrency:
	// refilling and taking a token is one indivisible act, so no two
	// callers can both see the same token and both take it.
	mu sync.Mutex
	// tokens is the current bucket level. It goes negative, and that is
	// the design: a negative level is the queue of callers already
	// waiting, and it is what stops ten simultaneous callers from all
	// computing the same short wait and then all firing together.
	tokens float64
	// last is the instant tokens was last refilled to.
	last time.Time
}

// RateLimiterOption configures the limiter [NewRateLimiter] returns.
type RateLimiterOption func(*RateLimiter)

// WithRateLimiterClock replaces the source of time, so a test can grant
// tokens against instants of its own choosing and assert exactly how many
// were granted in a window. A nil clock keeps the default: an option that
// could silently install no clock at all would panic on the first Wait,
// far from the line that caused it.
func WithRateLimiterClock(clock RateLimitClock) RateLimiterOption {
	return func(l *RateLimiter) {
		if clock != nil {
			l.clock = clock
		}
	}
}

// NewRateLimiter builds a limiter that grants perSecond requests a second,
// allowing burst of them to be taken at once. The bucket starts full, so a
// cold poll cycle is not made to wait for a rate it has not yet used.
//
// It refuses a rate or burst that could not pace anything, rather than
// quietly substituting a default: a limiter is a safety device, and the
// one number it is built from arriving as zero means the caller's Limits
// were never populated - which is worth a loud failure at wiring time, not
// a plausible-looking rate nobody chose.
func NewRateLimiter(perSecond float64, burst int, opts ...RateLimiterOption) (*RateLimiter, error) {
	if !(perSecond > 0) || math.IsInf(perSecond, 0) {
		return nil, fmt.Errorf("networks: request rate %v: %w", perSecond, ErrRateLimiterRate)
	}
	if burst < 1 {
		return nil, fmt.Errorf("networks: burst %d: %w", burst, ErrRateLimiterBurst)
	}
	l := &RateLimiter{
		perSecond: perSecond,
		burst:     float64(burst),
		clock:     rateLimitSystemClock{},
		tokens:    float64(burst),
	}
	for _, opt := range opts {
		opt(l)
	}
	// Read after the options, so the first refill is measured from the
	// clock the caller actually installed rather than from whatever the
	// default clock said a moment earlier.
	l.last = l.clock.Now()
	return l, nil
}

// Rate reports the requests per second this limiter is holding to.
//
// It is here for the reason [RetryBackoff.Policy] gives for itself, and
// for one more that is specific to the limiter. The conformance suite's
// rate-limit case runs against an *adapter*, so without an accessor it can
// only be told the expected rate out of band and hope the adapter wired
// the same number: an adapter that passed its network's declared limit
// through a unit-conversion bug would build a limiter at sixty requests a
// second and still pass a suite asserting against the six it was handed
// separately. With this, the suite can compare what the limiter is holding
// to against what the port declared, which is the only comparison that
// makes rule 3 checkable from outside the adapter.
func (l *RateLimiter) Rate() float64 { return l.perSecond }

// Burst reports how many requests an idle limiter grants at once. See
// [RateLimiter.Rate] for why the pair is exposed.
func (l *RateLimiter) Burst() int { return int(l.burst) }

// Wait blocks until this adapter may issue one more request, and returns
// ctx.Err() if the context ends first.
//
// A context that is already done is refused before a token is taken. That
// ordering matters during shutdown: a caller that has been told to stop
// should not consume a token it will never spend, because the token would
// come out of the budget of whichever caller is still running.
//
// A wait cut short by cancellation gives back the part of its token the
// bucket has not yet refilled, and nothing more. The naive alternatives
// are both wrong in ways that matter. Returning the whole token would
// credit refill that has already happened and let an extra request into
// the window, which is the one outcome rule 3 forbids. Forfeiting it
// entirely - which reads as the safe choice, and was - makes every
// abandoned wait a permanent debt against a bucket that only wall-clock
// refill can repay: a hundred callers abandoning a five-millisecond
// deadline against a six-a-second limit issue no requests at all and still
// leave the next honest caller sitting for seventeen seconds, one poll
// cycle's cancellation stalling the next cycle. Returning exactly the
// unearned remainder is neither: the level ends where it would have been
// had this caller never asked.
func (l *RateLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wait, ready := l.reserve()
	if wait <= 0 {
		return nil
	}
	if err := l.clock.Sleep(ctx, wait); err != nil {
		l.restore(ready)
		return err
	}
	return nil
}

// Pace wraps op so that every call to it takes a token from this limiter
// first, and reports the limiter's refusal without running op at all.
//
// It exists because the composition *is* rule 3, and prose cannot hold
// anyone to a composition. [RetryBackoff.Do] re-issues the operation it is
// given, so a poller that paced from outside the retry - one Wait, then Do
// around the request - would spend a single token on as many requests as
// the budget allows; full jitter makes the early waits legitimately
// sub-millisecond, so five requests can land inside a few milliseconds
// against a limit of six a second, and every test of either type would
// still pass. Passing limiter.Pace(op) to Do puts the token inside the
// attempt, where the rule says it belongs, and puts it there in a
// signature rather than in a comment somebody has to have read.
func (l *RateLimiter) Pace(op func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := l.Wait(ctx); err != nil {
			return err
		}
		return op(ctx)
	}
}

// reserve takes one token and reports how long the caller must wait before
// spending it, together with the instant that wait ends. It is the whole
// of the rate arithmetic, held in one place and under one lock so that
// refilling and taking can never interleave.
func (l *RateLimiter) reserve() (wait time.Duration, ready time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.refillLocked()
	l.tokens--
	if l.tokens >= 0 {
		return 0, now
	}
	// The debt divided by the refill rate is exactly how long until the
	// bucket is level again, which is when this caller's token becomes
	// real. Bounded in float space before the conversion - see
	// rateLimiterMaxWait for why the order matters.
	seconds := -l.tokens / l.perSecond
	if seconds >= rateLimiterMaxWait.Seconds() {
		return rateLimiterMaxWait, now.Add(rateLimiterMaxWait)
	}
	wait = time.Duration(seconds * float64(time.Second))
	return wait, now.Add(wait)
}

// restore gives back the part of an abandoned reservation the bucket has
// not refilled by itself yet, where ready is the instant the abandoned
// wait would have ended.
//
// The remainder is the arithmetic that makes this safe. A caller that
// waited three quarters of its debt away and then cancelled has already
// been repaid three quarters by refill; crediting a whole token would put
// the bucket a fraction above where it should be, and a bucket above where
// it should be is an extra request inside the window. Crediting only what
// is left to run - capped at the one token this caller took, and at the
// burst the bucket can hold - lands the level exactly where it would be
// had the caller never arrived.
func (l *RateLimiter) restore(ready time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.refillLocked()
	unearned := ready.Sub(now).Seconds() * l.perSecond
	if unearned <= 0 {
		// The wait had already run its course: the token is real, this
		// caller simply did not spend it, and the bucket has already
		// paid for it. Nothing to give back.
		return
	}
	l.tokens = min(l.burst, l.tokens+min(1, unearned))
}

// refillLocked brings the bucket up to date and reports the instant it is
// now level at. The caller holds the mutex.
//
// A clock that did not advance - or went backwards, which a test clock
// replaying an instant can do, and which a machine correcting its own
// wall clock can do in production - refills nothing. Reading it as a
// negative elapsed time would drain the bucket instead, so one backwards
// step of an hour would leave an adapter waiting an hour for a token it
// was owed immediately.
func (l *RateLimiter) refillLocked() time.Time {
	now := l.clock.Now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens = min(l.burst, l.tokens+elapsed.Seconds()*l.perSecond)
		l.last = now
	}
	return now
}
