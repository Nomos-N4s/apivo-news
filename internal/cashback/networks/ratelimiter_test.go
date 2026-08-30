// This file holds the token bucket's tests, and the fakes every pacing
// test in the package is driven from: limiterFakeClock, whose time moves
// only when a test moves it, and the spin helpers that wait for the code
// under test to reach a state without ever sleeping on real time.
//
// The shared fakes live here rather than in a fakes file of their own
// because this is the lowest-dependency half of the split: the assertions
// below name [networks.RateLimiter] and nothing else, so backoff_test.go
// can be written on top of them while nothing here has to know that a
// retry policy exists.

package networks_test

import (
	"context"
	"errors"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// limiterAnchor is the fixed instant every injected clock in this file
// starts from, so a failing assertion prints offsets a reader can do
// arithmetic on rather than today's date.
var limiterAnchor = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// limiterSpinBudget bounds how long a helper below will wait for the code
// under test to reach a state. It is a watchdog on real time and nothing
// else: no assertion in this file depends on it, and its only job is to
// turn a limiter that deadlocks into a failing test rather than a suite
// that never finishes.
const limiterSpinBudget = 30 * time.Second

// limiterFakeSleeper is one goroutine parked inside limiterFakeClock.Sleep:
// the virtual instant it is due to wake at, and the channel that wakes it.
type limiterFakeSleeper struct {
	deadline time.Time
	wake     chan struct{}
}

// limiterFakeClock is a [networks.RateLimitClock] whose time only moves
// when a test moves it. It is the whole reason the assertions in this file
// can be exact: a limiter paced by real time can only be tested with real
// sleeps, and a test that sleeps is a test that is either slow or flaky,
// never both-proof. Nothing here reads the wall clock except the watchdog
// in limiterSpinUntil.
type limiterFakeClock struct {
	mu       sync.Mutex
	now      time.Time
	sleepers []*limiterFakeSleeper
	// asked records every duration Sleep was called with, in order, so a
	// test can assert the backoff sequence itself rather than its
	// after-effects.
	asked []time.Duration
}

// newLimiterFakeClock returns a clock frozen at limiterAnchor.
func newLimiterFakeClock() *limiterFakeClock {
	return &limiterFakeClock{now: limiterAnchor}
}

// Now reports the virtual instant.
func (c *limiterFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep parks the caller until the virtual clock reaches its deadline, or
// until ctx ends. It honours ctx for the same reason the production clock
// does: a test that could not cancel a wait could not tell a limiter that
// respects cancellation from one that ignores it.
func (c *limiterFakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.asked = append(c.asked, d)
	if d <= 0 {
		c.mu.Unlock()
		return ctx.Err()
	}
	sleeper := &limiterFakeSleeper{deadline: c.now.Add(d), wake: make(chan struct{})}
	c.sleepers = append(c.sleepers, sleeper)
	c.mu.Unlock()

	select {
	case <-sleeper.wake:
		return nil
	case <-ctx.Done():
		c.abandon(sleeper)
		return ctx.Err()
	}
}

// abandon drops a cancelled sleeper, so a later advance does not try to
// wake a goroutine that has already gone.
func (c *limiterFakeClock) abandon(sleeper *limiterFakeSleeper) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i := slices.Index(c.sleepers, sleeper); i >= 0 {
		c.sleepers = slices.Delete(c.sleepers, i, i+1)
	}
}

// releaseDueLocked removes and returns every sleeper whose deadline the
// virtual clock has reached. The caller holds the mutex and closes the
// returned channels after releasing it, so a woken goroutine never has to
// wait on the lock that woke it.
func (c *limiterFakeClock) releaseDueLocked() []*limiterFakeSleeper {
	var due []*limiterFakeSleeper
	remaining := make([]*limiterFakeSleeper, 0, len(c.sleepers))
	for _, sleeper := range c.sleepers {
		if sleeper.deadline.After(c.now) {
			remaining = append(remaining, sleeper)
			continue
		}
		due = append(due, sleeper)
	}
	c.sleepers = remaining
	return due
}

// advance moves the virtual clock forward and wakes whoever is now due. A
// negative duration moves it backwards, which is how a test states what a
// machine correcting its own wall clock does to a limiter.
func (c *limiterFakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	due := c.releaseDueLocked()
	c.mu.Unlock()
	for _, sleeper := range due {
		close(sleeper.wake)
	}
}

// advanceToNextSleeper moves the virtual clock to the earliest deadline
// anyone is waiting on and wakes them, reporting whether there was anyone
// to wake. It advances by exactly the wait that was asked for, which is
// what lets a test read an elapsed-time budget off the same sequence it
// asserts the delays from.
func (c *limiterFakeClock) advanceToNextSleeper() bool {
	c.mu.Lock()
	if len(c.sleepers) == 0 {
		c.mu.Unlock()
		return false
	}
	next := c.sleepers[0].deadline
	for _, sleeper := range c.sleepers[1:] {
		if sleeper.deadline.Before(next) {
			next = sleeper.deadline
		}
	}
	if next.After(c.now) {
		c.now = next
	}
	due := c.releaseDueLocked()
	c.mu.Unlock()
	for _, sleeper := range due {
		close(sleeper.wake)
	}
	return true
}

// sleeping reports how many goroutines are parked in Sleep.
func (c *limiterFakeClock) sleeping() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sleepers)
}

// waitsAsked returns every duration Sleep was called with, in order.
func (c *limiterFakeClock) waitsAsked() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

// limiterSpinUntil yields the processor until cond holds. It spins rather
// than sleeping because a sleep here would be the wall-clock dependency
// the injected clock exists to remove; the deadline is a watchdog, not a
// timing assumption.
func limiterSpinUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limiterSpinBudget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// limiterWaitInBackground starts one Wait on its own goroutine and returns
// the channel its result will arrive on.
//
// Every caller of it then spins for "granted or parked" rather than for
// "parked", and asserts on which of the two happened. The difference
// matters more than it looks: a limiter that wrongly grants the request
// leaves a test spinning for a sleeper that will never exist, so the
// defect reports as the whole package timing out - which in CI takes the
// other packages' results down with it and names nothing - instead of as
// one line saying what was granted that should not have been.
func limiterWaitInBackground(ctx context.Context, l *networks.RateLimiter) <-chan error {
	granted := make(chan error, 1)
	go func() { granted <- l.Wait(ctx) }()
	return granted
}

// TestRateLimiterHoldsTheDeclaredRateUnderConcurrency is the unit-level
// half of the conformance suite's "rate-limit adherence under concurrency"
// (ports.md section 2): many callers hit one limiter at once, and the
// grants they receive must satisfy the token bucket's exact guarantee -
// no window of length W contains more than rate*W + burst of them.
//
// The window is checked sliding rather than aligned. An aligned check
// passes for a limiter that fires six requests at the end of one second
// and six at the start of the next, which is twelve requests in a second
// by any measure the network cares about.
func TestRateLimiterHoldsTheDeclaredRateUnderConcurrency(t *testing.T) {
	const (
		// The reference network's published limit (research.md, D3).
		perSecond = 6.0
		// A burst of one is strict spacing, the safest reading of a limit
		// published per second.
		burst   = 1
		callers = 30
		// One millisecond per advance: fine enough that the grants land
		// at distinguishable instants instead of all being rounded onto
		// the same coarse tick, which would flatter the limiter.
		step = time.Millisecond
	)

	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(perSecond, burst, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	var (
		granted = make([]time.Time, callers)
		failed  = make([]error, callers)
		done    atomic.Int64
		wg      sync.WaitGroup
	)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			failed[i] = limiter.Wait(context.Background())
			granted[i] = clock.Now()
			done.Add(1)
		}()
	}

	// Time only moves when every caller that has not finished is parked
	// in Sleep. Advancing while one is between waking and reading the
	// clock would record its grant later than it happened, which would
	// spread the grants out and hide exactly the violation being hunted.
	for int(done.Load()) < callers {
		limiterSpinUntil(t, "every caller to be parked or finished", func() bool {
			return clock.sleeping()+int(done.Load()) >= callers
		})
		if int(done.Load()) >= callers {
			break
		}
		clock.advance(step)
	}
	wg.Wait()

	for i, err := range failed {
		if err != nil {
			t.Fatalf("caller %d: Wait: %v", i, err)
		}
	}

	times := slices.Clone(granted)
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })

	const window = time.Second
	allowed := int(perSecond*window.Seconds()) + burst
	for i, start := range times {
		end := start.Add(window)
		count := 0
		for _, at := range times {
			if !at.Before(start) && !at.After(end) {
				count++
			}
		}
		if count > allowed {
			t.Fatalf("window %d starting %v held %d grants, over the %d that %v requests/second with a burst of %d allows; grants at %v",
				i, start.Sub(limiterAnchor), count, allowed, perSecond, burst, times)
		}
	}
}

// TestRateLimiterGrantsItsBurstThenPaces pins the two halves of the bucket
// separately: the burst is spendable at once on a cold start, and the very
// next request is made to wait exactly one refill interval.
func TestRateLimiterGrantsItsBurstThenPaces(t *testing.T) {
	const (
		perSecond = 2.0
		burst     = 3
	)
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(perSecond, burst, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	for i := range burst {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("burst request %d: Wait: %v", i+1, err)
		}
	}
	if asked := clock.waitsAsked(); len(asked) != 0 {
		t.Fatalf("the burst was made to wait %v; a full bucket owes nobody a wait", asked)
	}

	granted := limiterWaitInBackground(context.Background(), limiter)
	limiterSpinUntil(t, "the request after the burst to park", func() bool { return clock.sleeping() == 1 })
	if got, want := clock.waitsAsked(), []time.Duration{500 * time.Millisecond}; !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v (one refill at %v requests/second)", got, want, perSecond)
	}
	clock.advance(500 * time.Millisecond)
	if err := <-granted; err != nil {
		t.Fatalf("request after the burst: Wait: %v", err)
	}
}

// TestRateLimiterRefillNeverExceedsItsBurst is the assertion the sliding
// window above cannot make. Every caller in that test arrives at once, so
// the bucket is never allowed to sit idle, and the cap on the refill is
// never reached - a limiter that accumulated tokens without bound would
// pass it untouched.
//
// The idle bucket is the production case, not an exotic one: a poller
// sleeps between cycles, so by the time the next cycle starts the bucket
// has had minutes to fill. Without the cap it holds rate*idle tokens and
// hands them all out in the same instant, which is a burst of hundreds of
// requests against a limit of a few per second - the ports.md section 2
// rule 3 violation this file exists to prevent, arriving at exactly the
// moment nobody is watching.
func TestRateLimiterRefillNeverExceedsItsBurst(t *testing.T) {
	const (
		perSecond = 4.0
		burst     = 2
		// Far longer than the burst takes to refill, so an uncapped
		// bucket would be holding forty tokens by the end of it.
		idle = 10 * time.Second
	)
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(perSecond, burst, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	for i := range burst {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("cold burst request %d: Wait: %v", i+1, err)
		}
	}
	clock.advance(idle)

	// The idle bucket holds a burst, and exactly a burst.
	for i := range burst {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("request %d after %v idle: Wait: %v", i+1, idle, err)
		}
	}
	if asked := clock.waitsAsked(); len(asked) != 0 {
		t.Fatalf("the refilled burst was made to wait %v; a bucket idle for %v owes nobody a wait for its first %d requests", asked, idle, burst)
	}

	granted := limiterWaitInBackground(context.Background(), limiter)
	limiterSpinUntil(t, "the request past the refilled burst to be granted or parked", func() bool {
		return len(granted) > 0 || clock.sleeping() > 0
	})
	if len(granted) > 0 {
		t.Fatalf("after %v idle the limiter granted %d requests in one instant on a %v requests/second limit with a burst of %d; a bucket that refills past its burst dumps rate*idle requests the moment a cycle starts",
			idle, burst+1, perSecond, burst)
	}
	if got, want := clock.waitsAsked(), []time.Duration{250 * time.Millisecond}; !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v (one refill at %v requests/second)", got, want, perSecond)
	}
	clock.advance(250 * time.Millisecond)
	if err := <-granted; err != nil {
		t.Fatalf("request past the refilled burst: Wait: %v", err)
	}
}

// TestRateLimiterClampsAWaitThatWouldOverflow covers the bound the type
// doc calls the exact inversion of the rule the limiter enforces. A
// float-to-int conversion that overflows is implementation defined and on
// amd64 lands on math.MinInt64, so an unbounded conversion turns the
// longest wait imaginable into no wait at all and the limiter paces
// nothing.
func TestRateLimiterClampsAWaitThatWouldOverflow(t *testing.T) {
	clock := newLimiterFakeClock()
	// Positive and finite, so the constructor accepts it, and slow enough
	// that one token's debt is a hundred billion seconds - which an int64
	// of nanoseconds cannot hold.
	limiter, err := networks.NewRateLimiter(1e-11, 1, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("burst request: Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waited := limiterWaitInBackground(ctx, limiter)
	limiterSpinUntil(t, "the request past the burst to be granted or to ask for its wait", func() bool {
		return len(waited) > 0 || len(clock.waitsAsked()) == 1
	})
	if len(waited) > 0 {
		t.Fatalf("the request past the burst was granted at once by a limiter of one token per hundred billion seconds; a wait that overflowed into a negative duration is no wait at all, which is the exact inversion of the rule this limiter enforces")
	}

	asked := clock.waitsAsked()[0]
	if asked <= 0 {
		t.Fatalf("the wait asked for was %v; a wait that overflowed into a negative duration is granted immediately by every clock here, so the slowest limit expressible becomes no limit at all", asked)
	}
	if want := time.Duration(math.MaxInt64); asked != want {
		t.Fatalf("wait asked = %v, want %v (bounded in float space before the conversion)", asked, want)
	}
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancellation = %v, want context.Canceled", err)
	}
}

// TestRateLimiterReturnsAnAbandonedWaitToTheBucket covers what a caller
// with a deadline costs everybody else. A wait cut short forfeits nothing
// but the part of its token the bucket has already refilled: the callers
// below issue no requests at all, so the bucket must end where it started.
//
// Charging them in full is the plausible mistake, and it is not the
// briefly-slower-than-declared it looks like. Debt accumulates and only
// wall-clock refill repays it, so one poll cycle abandoned at shutdown
// leaves the next cycle - healthy network, full budget - sitting idle for
// as long as it takes to pay off requests nobody ever made.
func TestRateLimiterReturnsAnAbandonedWaitToTheBucket(t *testing.T) {
	const (
		perSecond  = 1.0
		abandoners = 10
	)
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(perSecond, 1, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	// Spend the burst, so every caller below has to wait for its token.
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("burst request: Wait: %v", err)
	}

	for i := range abandoners {
		ctx, cancel := context.WithCancel(context.Background())
		waited := limiterWaitInBackground(ctx, limiter)
		limiterSpinUntil(t, "an abandoning caller to park", func() bool { return clock.sleeping() == 1 })
		cancel()
		if err := <-waited; !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoning caller %d: Wait = %v, want context.Canceled", i+1, err)
		}
	}

	granted := limiterWaitInBackground(context.Background(), limiter)
	limiterSpinUntil(t, "the honest caller to be granted or parked", func() bool {
		return len(granted) > 0 || clock.sleeping() > 0
	})

	// No virtual time has passed and no request was issued, so every one
	// of those waits - the abandoned ones and the honest one - is the
	// same single refill interval.
	want := make([]time.Duration, 0, abandoners+1)
	for range abandoners + 1 {
		want = append(want, time.Second)
	}
	if got := clock.waitsAsked(); !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v; %d callers that issued no request left the bucket in debt, and the next honest caller is paying it off",
			got, want, abandoners)
	}
	clock.advance(time.Second)
	if err := <-granted; err != nil {
		t.Fatalf("honest caller after %d abandoned waits: Wait: %v", abandoners, err)
	}
}

// TestRateLimiterIgnoresAClockThatRanBackwards covers the guard on the
// refill. Time going backwards is not hypothetical - a machine correcting
// its wall clock does it - and reading a negative elapsed as refill drains
// the bucket by rate*correction, so one hour of correction leaves an
// adapter waiting an hour for a token it was owed at once.
func TestRateLimiterIgnoresAClockThatRanBackwards(t *testing.T) {
	const burst = 2
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(1, burst, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	for i := range burst {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("burst request %d: Wait: %v", i+1, err)
		}
	}

	clock.advance(-time.Hour)

	granted := limiterWaitInBackground(context.Background(), limiter)
	limiterSpinUntil(t, "the request after the correction to be granted or parked", func() bool {
		return len(granted) > 0 || clock.sleeping() > 0
	})
	if got, want := clock.waitsAsked(), []time.Duration{time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v; an hour read backwards is an hour of drain, and the next request pays for a correction it had nothing to do with", got, want)
	}
	clock.advance(time.Second)
	if err := <-granted; err != nil {
		t.Fatalf("request after the correction: Wait: %v", err)
	}
}

// TestRateLimiterWaitReturnsPromptlyOnCancellation proves the promise a
// shutdown depends on: a caller told to stop does not first sit out the
// network's rate limit. Promptness is asserted on the injected clock -
// the wait was for a full second and not one virtual nanosecond of it
// passed - because an assertion on real elapsed time would be the
// wall-clock dependency this file avoids.
func TestRateLimiterWaitReturnsPromptlyOnCancellation(t *testing.T) {
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(1, 1, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waited := limiterWaitInBackground(ctx, limiter)
	limiterSpinUntil(t, "the second caller to park", func() bool { return clock.sleeping() == 1 })
	if got, want := clock.waitsAsked(), []time.Duration{time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v", got, want)
	}
	cancel()

	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancellation = %v, want context.Canceled", err)
	}
	if got := clock.Now(); !got.Equal(limiterAnchor) {
		t.Fatalf("clock moved to %v; a cancelled wait must not have waited out %v of it", got.Sub(limiterAnchor), time.Second)
	}
	if n := clock.sleeping(); n != 0 {
		t.Fatalf("%d goroutine(s) still parked after cancellation", n)
	}
}

// TestRateLimiterWaitRefusesAnAlreadyCancelledContext covers the ordering
// documented on Wait: the context is checked before a token is taken, so a
// caller that has already been told to stop does not spend budget that
// belongs to whichever caller is still running.
func TestRateLimiterWaitRefusesAnAlreadyCancelledContext(t *testing.T) {
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(1, 1, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on a cancelled context = %v, want context.Canceled", err)
	}
	if asked := clock.waitsAsked(); len(asked) != 0 {
		t.Fatalf("a cancelled caller was made to wait %v", asked)
	}

	// The token the refused caller did not take is still in the bucket:
	// a live caller gets it without waiting.
	granted := limiterWaitInBackground(context.Background(), limiter)
	limiterSpinUntil(t, "the live caller to be granted or parked", func() bool {
		return len(granted) > 0 || clock.sleeping() > 0
	})
	if clock.sleeping() > 0 {
		t.Fatalf("the live caller was made to wait %v; the refused caller consumed the bucket's token", clock.waitsAsked())
	}
	if err := <-granted; err != nil {
		t.Fatalf("Wait after a refused caller: %v", err)
	}
}

// TestRateLimiterReportsWhatItIsHoldingTo covers the accessors the
// conformance suite needs. The suite's rate-limit case runs against an
// adapter, so without them it can only be told the expected rate out of
// band, and an adapter that built its limiter from a mis-converted limit
// would pass a suite asserting against the number it was handed
// separately.
func TestRateLimiterReportsWhatItIsHoldingTo(t *testing.T) {
	limiter, err := networks.NewRateLimiter(6, 2)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	if got, want := limiter.Rate(), 6.0; got != want {
		t.Fatalf("Rate() = %v, want %v", got, want)
	}
	if got, want := limiter.Burst(), 2; got != want {
		t.Fatalf("Burst() = %v, want %v", got, want)
	}
}

// TestRateLimiterPaceTakesATokenBeforeEveryAttempt pins the composition
// rule 3 actually needs. A retry re-issues the request, so a limiter
// placed around a retry sequence spends one token on all of it; Pace puts
// the token inside the attempt, where the rule says it belongs.
func TestRateLimiterPaceTakesATokenBeforeEveryAttempt(t *testing.T) {
	clock := newLimiterFakeClock()
	limiter, err := networks.NewRateLimiter(1, 1, networks.WithRateLimiterClock(clock))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	var ran atomic.Int64
	paced := limiter.Pace(func(context.Context) error {
		ran.Add(1)
		return nil
	})

	if err := paced(context.Background()); err != nil {
		t.Fatalf("first paced call: %v", err)
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("the operation ran %d time(s), want 1", got)
	}

	// The bucket is empty now, so the second call must not reach the
	// operation until its token is real.
	done := make(chan error, 1)
	go func() { done <- paced(context.Background()) }()
	limiterSpinUntil(t, "the second paced call to park or finish", func() bool {
		return len(done) > 0 || clock.sleeping() == 1
	})
	if len(done) > 0 {
		t.Fatalf("the second paced call finished on an empty bucket without waiting for a token; an operation that runs before its token is paced by nothing, and a retry sequence built on it puts its whole budget on the wire at once")
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("the operation ran %d time(s) while its token was still owed; an operation that runs before its token is not paced by anything", got)
	}
	clock.advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("second paced call: %v", err)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("the operation ran %d time(s) after its token arrived, want 2", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := paced(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("paced call on a cancelled context = %v, want context.Canceled", err)
	}
	if got := ran.Load(); got != 2 {
		t.Fatalf("the operation ran %d time(s); a caller already told to stop must not reach it", got)
	}
}

// TestNewRateLimiterRefusesAnUnpaceableRate keeps the limiter a loud
// failure at wiring time rather than a poller that silently stops
// fetching hours later.
func TestNewRateLimiterRefusesAnUnpaceableRate(t *testing.T) {
	rates := map[string]float64{
		"zero":              0,
		"negative":          -6,
		"not a number":      math.NaN(),
		"positive infinity": math.Inf(1),
	}
	for name, rate := range rates {
		t.Run(name, func(t *testing.T) {
			if _, err := networks.NewRateLimiter(rate, 1); !errors.Is(err, networks.ErrRateLimiterRate) {
				t.Fatalf("NewRateLimiter(%v, 1) error = %v, want ErrRateLimiterRate", rate, err)
			}
		})
	}
	for _, burst := range []int{0, -1} {
		if _, err := networks.NewRateLimiter(6, burst); !errors.Is(err, networks.ErrRateLimiterBurst) {
			t.Fatalf("NewRateLimiter(6, %d) error = %v, want ErrRateLimiterBurst", burst, err)
		}
	}
}
