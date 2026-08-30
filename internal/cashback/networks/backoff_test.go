// This file holds the retry policy's tests: the delay sequence and its
// bounds, the two ceilings, the observer hook, and the cross-cutting cases
// that exercise both pacing types at once because one test function
// covers both. The jitter sources live here because these are the only
// tests that inject randomness.
//
// It also holds the two tests that state a vocabulary contract through the
// loop that enforces it - a terminal marking beating a retryable cause,
// and a marking with no cause costing exactly one attempt - because
// neither can be written without [networks.RetryBackoff].

package networks_test

import (
	"context"
	"errors"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// limiterFixedRandom is a jitter source that always returns fraction, so a
// delay sequence is a fixed list of numbers.
func limiterFixedRandom(fraction float64) func() float64 {
	return func() float64 { return fraction }
}

// limiterScriptedRandom hands out fractions in order and fails the test if
// it is asked for more than were scripted - a backoff that drew more
// jitter than the test expected is a backoff the test was not describing.
func limiterScriptedRandom(t *testing.T, fractions ...float64) func() float64 {
	t.Helper()
	var mu sync.Mutex
	next := 0
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(fractions) {
			t.Errorf("jitter source asked for fraction %d, only %d were scripted", next+1, len(fractions))
			return 0
		}
		fraction := fractions[next]
		next++
		return fraction
	}
}

// limiterDrainSleeps advances the injected clock to each successive
// deadline until finished is closed, so an operation that waits between
// attempts runs to completion without a real second passing.
func limiterDrainSleeps(t *testing.T, clock *limiterFakeClock, finished <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(limiterSpinBudget)
	for {
		select {
		case <-finished:
			return
		default:
		}
		if !clock.advanceToNextSleeper() {
			if time.Now().After(deadline) {
				t.Errorf("timed out driving the injected clock")
				return
			}
			runtime.Gosched()
		}
	}
}

// TestRetryBackoffDelayGrowsExponentiallyAndIsCapped asserts the sequence
// itself, not that something slept. With the jitter fraction pinned at a
// half, every delay is exactly half its ceiling, so the doubling and the
// clamp are both readable straight off the list.
func TestRetryBackoffDelayGrowsExponentiallyAndIsCapped(t *testing.T) {
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		DelayCap:    2 * time.Second,
		MaxAttempts: 8,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	want := []time.Duration{
		50 * time.Millisecond,   // ceiling 100ms
		100 * time.Millisecond,  // ceiling 200ms
		200 * time.Millisecond,  // ceiling 400ms
		400 * time.Millisecond,  // ceiling 800ms
		800 * time.Millisecond,  // ceiling 1.6s
		1000 * time.Millisecond, // ceiling clamped from 3.2s to 2s
		1000 * time.Millisecond, // and stays clamped
		1000 * time.Millisecond,
	}
	got := make([]time.Duration, 0, len(want))
	for attempt := 1; attempt <= len(want); attempt++ {
		delay, err := backoff.Delay(attempt, 0)
		if err != nil {
			t.Fatalf("Delay(%d, 0): %v", attempt, err)
		}
		got = append(got, delay)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("delay sequence = %v, want %v", got, want)
	}
}

// TestRetryBackoffDelayRefusesAnAttemptBelowOne keeps the one-based
// contract out of the prose. A caller counting from zero writes the
// ordinary Go loop and gets the base ceiling for its first two waits, so
// its whole escalation runs one step behind and puts an extra early
// request into a host that is already refusing us - a defect no test of
// that caller's own could see, because every number it got back was a
// legal delay.
func TestRetryBackoffDelayRefusesAnAttemptBelowOne(t *testing.T) {
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{BaseDelay: 100 * time.Millisecond},
		networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}
	for _, attempt := range []int{-1, 0} {
		got, err := backoff.Delay(attempt, 0)
		if !errors.Is(err, networks.ErrRetryAttemptNumber) {
			t.Fatalf("Delay(%d, 0) error = %v, want ErrRetryAttemptNumber", attempt, err)
		}
		if got != 0 {
			t.Fatalf("Delay(%d, 0) = %v alongside its refusal; a refused call must not also look like an answer", attempt, got)
		}
	}
	if _, err := backoff.Delay(1, 0); err != nil {
		t.Fatalf("Delay(1, 0): %v; one is the first attempt, not an out-of-range one", err)
	}
}

// TestRetryBackoffDelayBoundsAnOutOfRangeJitterSource covers the one input
// this file used to take on trust. The source is injected, so it is an
// input like the rate and the policy - and both ways out of [0, 1) fail
// silently: a negative fraction returns a negative duration, which every
// clock here treats as no wait at all, so the retry sequence becomes the
// unpaced burst the jitter exists to prevent; a fraction at or above one
// scales the wait past its own documented ceiling.
func TestRetryBackoffDelayBoundsAnOutOfRangeJitterSource(t *testing.T) {
	const base = 100 * time.Millisecond
	// The bound is the nearest legal fraction, not a refusal: there is
	// nowhere in this signature to put a refusal, and the safe answer is
	// obvious.
	justInside := time.Duration(math.Nextafter(1, 0) * float64(base))

	cases := map[string]struct {
		fraction float64
		want     time.Duration
	}{
		"negative":          {fraction: -0.5, want: 0},
		"not a number":      {fraction: math.NaN(), want: 0},
		"negative infinity": {fraction: math.Inf(-1), want: 0},
		"exactly one":       {fraction: 1, want: justInside},
		"far above one":     {fraction: 7, want: justInside},
		"positive infinity": {fraction: math.Inf(1), want: justInside},
		"inside the range":  {fraction: 0.5, want: base / 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{BaseDelay: base},
				networks.WithRetryBackoffRandom(limiterFixedRandom(tc.fraction)))
			if err != nil {
				t.Fatalf("NewRetryBackoff: %v", err)
			}
			got, err := backoff.Delay(1, 0)
			if err != nil {
				t.Fatalf("Delay(1, 0): %v", err)
			}
			if got < 0 {
				t.Fatalf("Delay(1, 0) = %v; a negative wait is granted at once by every clock here, so the whole retry sequence runs unpaced against a host that is already refusing us", got)
			}
			if got >= base {
				t.Fatalf("Delay(1, 0) = %v, outside the documented bound [0, %v)", got, base)
			}
			if got != tc.want {
				t.Fatalf("Delay(1, 0) = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("Do never sleeps a wait it could not have drawn", func(t *testing.T) {
		clock := newLimiterFakeClock()
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay:   100 * time.Millisecond,
			DelayCap:    time.Second,
			MaxAttempts: 4,
			MaxElapsed:  time.Hour,
		}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(-0.5)))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		busy := errors.New("network is busy")
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_ = backoff.Do(context.Background(), func(context.Context) error {
				return networks.NewRetryableError(busy, 0)
			})
		}()
		limiterDrainSleeps(t, clock, finished)
		<-finished
		for i, asked := range clock.waitsAsked() {
			if asked < 0 {
				t.Fatalf("wait %d was %v; the waits asked for were %v", i+1, asked, clock.waitsAsked())
			}
		}
	})
}

// TestRetryBackoffDelayCannotOverflowItsCeiling covers the two additions
// the doubling makes on its way to the cap. The inputs are absurd and the
// policy is legal: withDefaults asks only that the cap is not below the
// base, so a base of a century and a half is accepted, and a doubling that
// overshoots int64 turns the longest ceiling expressible into a negative
// duration - a wait of no time dressed as a very long one.
func TestRetryBackoffDelayCannotOverflowItsCeiling(t *testing.T) {
	const year = 365 * 24 * time.Hour

	t.Run("the doubling clamps before it multiplies", func(t *testing.T) {
		const ceiling = 200 * year
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay: 150 * year,
			DelayCap:  ceiling,
		}, networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		for attempt := 1; attempt <= 5; attempt++ {
			got, err := backoff.Delay(attempt, 0)
			if err != nil {
				t.Fatalf("Delay(%d, 0): %v", attempt, err)
			}
			if got < 0 || got > ceiling {
				t.Fatalf("Delay(%d, 0) = %v, outside [0, %v]; the doubling overflowed on its way to the clamp", attempt, got, ceiling)
			}
		}
	})

	t.Run("the ask plus the jitter saturates", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay: 200 * year,
			DelayCap:  time.Duration(math.MaxInt64),
		}, networks.WithRetryBackoffRandom(limiterFixedRandom(math.Nextafter(1, 0))))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		got, err := backoff.Delay(2, 24*time.Hour)
		if err != nil {
			t.Fatalf("Delay(2, 24h): %v", err)
		}
		if got <= 0 {
			t.Fatalf("Delay(2, 24h) = %v; a day added to a jitter near the end of the duration range wrapped into a wait that is no wait", got)
		}
		if want := time.Duration(math.MaxInt64); got != want {
			t.Fatalf("Delay(2, 24h) = %v, want %v", got, want)
		}
	})
}

// TestRetryBackoffJitterSpansItsWholeBound is the assertion a comment
// cannot make: the delay is a real function of the randomness, spread
// across the entire window from zero up to but never reaching the
// ceiling. A fixed multiplier - the thundering herd this type exists to
// prevent - fails it, because every element would be the same.
//
// The equality below is the whole assertion. Range and distinctness
// checks alongside it would read as independent guards and be incapable
// of firing, since pinning every element exactly already pins both.
func TestRetryBackoffJitterSpansItsWholeBound(t *testing.T) {
	const (
		base    = 100 * time.Millisecond
		attempt = 3
	)
	// Binary-exact fractions, so the expected products are exact too.
	fractions := []float64{0, 0.25, 0.5, 0.75, 0.875}
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   base,
		DelayCap:    2 * time.Second,
		MaxAttempts: 8,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffRandom(limiterScriptedRandom(t, fractions...)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	var got []time.Duration
	for range fractions {
		delay, err := backoff.Delay(attempt, 0)
		if err != nil {
			t.Fatalf("Delay(%d, 0): %v", attempt, err)
		}
		got = append(got, delay)
	}
	// Zero up to seven eighths of the ceiling, which is the base doubled
	// twice: the draw spans the window rather than clustering in it.
	want := []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 350 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Fatalf("jittered delays = %v, want %v; a jitter that ignores its source is a fixed multiplier, which retries every replica in lockstep", got, want)
	}
}

// TestRetryBackoffDelayAddsJitterOnTopOfRetryAfter pins the documented
// treatment of the header: the network's ask is a floor, never the whole
// answer. Returning the ask alone would put every client that read the
// same header back on the wire in the same millisecond.
func TestRetryBackoffDelayAddsJitterOnTopOfRetryAfter(t *testing.T) {
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		DelayCap:    2 * time.Second,
		MaxAttempts: 8,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	cases := []struct {
		attempt    int
		retryAfter time.Duration
		want       time.Duration
	}{
		{attempt: 1, retryAfter: 30 * time.Second, want: 30*time.Second + 50*time.Millisecond},
		{attempt: 4, retryAfter: 2 * time.Second, want: 2*time.Second + 400*time.Millisecond},
		// A broken or hostile ask is clamped before anything is added to
		// it, so the arithmetic cannot overflow into a negative wait.
		{attempt: 1, retryAfter: 400 * 24 * time.Hour, want: 24*time.Hour + 50*time.Millisecond},
	}
	for _, tc := range cases {
		got, err := backoff.Delay(tc.attempt, tc.retryAfter)
		if err != nil {
			t.Fatalf("Delay(%d, %v): %v", tc.attempt, tc.retryAfter, err)
		}
		if got != tc.want {
			t.Fatalf("Delay(%d, %v) = %v, want %v", tc.attempt, tc.retryAfter, got, tc.want)
		}
	}
}

// TestRetryBackoffDoSleepsTheJitteredSequence drives the whole wrapper
// over an injected clock and asserts the exact waits it asked for, which
// is the property the poller inherits.
func TestRetryBackoffDoSleepsTheJitteredSequence(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		DelayCap:    2 * time.Second,
		MaxAttempts: 4,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	busy := errors.New("network is busy")
	var attempts atomic.Int64
	finished := make(chan struct{})
	var result error
	go func() {
		defer close(finished)
		result = backoff.Do(context.Background(), func(context.Context) error {
			if attempts.Add(1) < 4 {
				return networks.NewRetryableError(busy, 0)
			}
			return nil
		})
	}()
	limiterDrainSleeps(t, clock, finished)
	<-finished

	if result != nil {
		t.Fatalf("Do = %v, want nil once the fourth attempt succeeds", result)
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("operation ran %d time(s), want 4", got)
	}
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
	if got := clock.waitsAsked(); !slices.Equal(got, want) {
		t.Fatalf("waits asked = %v, want %v", got, want)
	}
	if got, want := clock.Now().Sub(limiterAnchor), 350*time.Millisecond; got != want {
		t.Fatalf("elapsed %v, want %v", got, want)
	}
}

// TestRetryBackoffReportsEveryRetryToItsObserver covers the only way a
// caller can see inside the sequence. Do keeps one error and chooses the
// waits itself, so without the observer "attempt 2 of 5 failed with 503,
// retrying in 200ms" is not expressible by anybody - and an adapter that
// wanted the line would have to abandon Do and re-implement both ceilings,
// the classification and the sentinel by hand.
func TestRetryBackoffReportsEveryRetryToItsObserver(t *testing.T) {
	type observed struct {
		attempt int
		wait    time.Duration
		failure error
	}
	var (
		mu    sync.Mutex
		seen  []observed
		clock = newLimiterFakeClock()
	)
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		DelayCap:    time.Second,
		MaxAttempts: 3,
		MaxElapsed:  time.Hour,
	},
		networks.WithRetryBackoffClock(clock),
		networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)),
		networks.WithRetryBackoffOnRetry(func(attempt int, wait time.Duration, failure error) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, observed{attempt: attempt, wait: wait, failure: failure})
		}),
	)
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	unwell := errors.New("network is unwell")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = backoff.Do(context.Background(), func(context.Context) error {
			return networks.NewRetryableError(unwell, 0)
		})
	}()
	limiterDrainSleeps(t, clock, finished)
	<-finished

	mu.Lock()
	defer mu.Unlock()
	// Three attempts, two of which are followed by a wait: the attempt
	// that ends the sequence is not a retry and is not reported as one.
	want := []observed{
		{attempt: 1, wait: 50 * time.Millisecond},
		{attempt: 2, wait: 100 * time.Millisecond},
	}
	if len(seen) != len(want) {
		t.Fatalf("observer saw %d retries, want %d: %+v", len(seen), len(want), seen)
	}
	for i, got := range seen {
		if got.attempt != want[i].attempt || got.wait != want[i].wait {
			t.Fatalf("retry %d reported attempt %d waiting %v, want attempt %d waiting %v",
				i+1, got.attempt, got.wait, want[i].attempt, want[i].wait)
		}
		if !errors.Is(got.failure, unwell) {
			t.Fatalf("retry %d reported failure %v; an observer that cannot name the failure cannot write the log line it exists for", i+1, got.failure)
		}
	}
}

// TestRetryBackoffDoStopsAtTheAttemptCeiling proves the first of the two
// ceilings the contract asks for, and that the failure underneath travels
// out with the verdict.
func TestRetryBackoffDoStopsAtTheAttemptCeiling(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		DelayCap:    time.Second,
		MaxAttempts: 3,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	refused := errors.New("network refused the window")
	var attempts atomic.Int64
	finished := make(chan struct{})
	var result error
	go func() {
		defer close(finished)
		result = backoff.Do(context.Background(), func(context.Context) error {
			attempts.Add(1)
			return networks.NewRetryableError(refused, 0)
		})
	}()
	limiterDrainSleeps(t, clock, finished)
	<-finished

	if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
	}
	if !errors.Is(result, refused) {
		t.Fatalf("Do = %v; the last failure must travel with the verdict, or an operator cannot tell a refusing network from a spent budget", result)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("operation ran %d time(s), want 3", got)
	}
}

// TestRetryBackoffDoStopsAtTheElapsedCeiling proves the second ceiling,
// which the attempt count cannot: the budget here is spent by time while
// a hundred attempts are still nominally available. The failure
// underneath must travel with this verdict too - the two give-up paths
// make the same promise, and an operator reading either needs the same
// thing from it.
func TestRetryBackoffDoStopsAtTheElapsedCeiling(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   time.Second,
		DelayCap:    time.Second,
		MaxAttempts: 100,
		MaxElapsed:  1200 * time.Millisecond,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	unwell := errors.New("network is unwell")
	var attempts atomic.Int64
	finished := make(chan struct{})
	var result error
	go func() {
		defer close(finished)
		result = backoff.Do(context.Background(), func(context.Context) error {
			attempts.Add(1)
			return networks.NewRetryableError(unwell, 0)
		})
	}()
	limiterDrainSleeps(t, clock, finished)
	<-finished

	if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
	}
	if !errors.Is(result, unwell) {
		t.Fatalf("Do = %v; the last failure must travel with this verdict as it does with the attempt ceiling's, or an operator reading it has a spent budget and no cause", result)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("operation ran %d time(s), want 3: two 500ms waits fit inside the 1.2s budget and the third does not", got)
	}
	// The ceiling is a ceiling: the sequence stopped short of it rather
	// than overshooting it and reporting the overshoot afterwards.
	if got := clock.Now().Sub(limiterAnchor); got > 1200*time.Millisecond {
		t.Fatalf("elapsed %v, over the 1.2s budget", got)
	}
}

// TestRetryBackoffDoRefusesAWaitThatWouldSpendTheWholeBudget lands the
// sequence exactly on the ceiling, which the test above cannot: its waits
// never sum to the budget, so whether the comparison is inclusive is
// invisible in it. A wait that would leave nothing over is refused,
// because sleeping it spends the whole budget on waiting and none of it on
// asking - a ceiling honoured by wasting the thing it protects.
func TestRetryBackoffDoRefusesAWaitThatWouldSpendTheWholeBudget(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   time.Second,
		DelayCap:    time.Second,
		MaxAttempts: 100,
		// Exactly two of the 500ms waits below.
		MaxElapsed: time.Second,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0.5)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	unwell := errors.New("network is unwell")
	var attempts atomic.Int64
	finished := make(chan struct{})
	var result error
	go func() {
		defer close(finished)
		result = backoff.Do(context.Background(), func(context.Context) error {
			attempts.Add(1)
			return networks.NewRetryableError(unwell, 0)
		})
	}()
	limiterDrainSleeps(t, clock, finished)
	<-finished

	if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("operation ran %d time(s), want 2", got)
	}
	if got, want := clock.Now().Sub(limiterAnchor), 500*time.Millisecond; got != want {
		t.Fatalf("elapsed %v, want %v; the second wait would have carried the sequence to the budget exactly, leaving nothing to attempt with, so it must not have been slept", got, want)
	}
}

// TestRetryBackoffDoBoundsEachAttemptByTheElapsedBudget is the difference
// between a budget that bounds the waits and a budget that is a promise
// about when the caller gets an answer. A network that accepts the
// connection and never replies spends no waits at all, so a sequence that
// bounded only its own sleeping would sit inside one attempt for as long
// as the adapter's transport allowed - past the poll cycle's own schedule,
// with the next cycle stacking on top.
//
// It is the one test here that runs on real time, and it has to: the bound
// is a context deadline, and a context deadline is real whatever clock the
// backoff was handed. The operation's own patience is forty times the
// budget, so the assertion is about which of the two ended the attempt,
// not about how fast the machine is.
func TestRetryBackoffDoBoundsEachAttemptByTheElapsedBudget(t *testing.T) {
	const (
		budget   = 50 * time.Millisecond
		patience = 2 * time.Second
	)
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   time.Millisecond,
		DelayCap:    time.Millisecond,
		MaxAttempts: 10,
		MaxElapsed:  budget,
	}, networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	hung := errors.New("network accepted the connection and never answered")
	started := time.Now()
	result := backoff.Do(context.Background(), func(ctx context.Context) error {
		timer := time.NewTimer(patience)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		return networks.NewRetryableError(hung, 0)
	})
	elapsed := time.Since(started)

	if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
	}
	if !errors.Is(result, hung) {
		t.Fatalf("Do = %v; the last failure must travel with the verdict", result)
	}
	if elapsed >= patience/2 {
		t.Fatalf("Do returned after %v against a %v budget; a budget that bounds only the waits promises nothing about when the caller gets an answer, and an adapter whose network never replies waits on its transport instead", elapsed, budget)
	}
}

// TestRetryBackoffDoReturnsATerminalFailureUntouched keeps classification
// where it belongs. A rejected credential retried five times is five
// rejected credentials and a slower error message.
func TestRetryBackoffDoReturnsATerminalFailureUntouched(t *testing.T) {
	clock := newLimiterFakeClock()
	// The jitter is pinned to zero so this test cannot depend on the
	// clock being driven: a backoff that wrongly retried a terminal
	// failure must fail this test on the assertions below, not by
	// deadlocking on a wait nobody is advancing.
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{},
		networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	rejected := errors.New("network rejected the credentials")
	attempts := 0
	result := backoff.Do(context.Background(), func(context.Context) error {
		attempts++
		return rejected
	})
	if !errors.Is(result, rejected) {
		t.Fatalf("Do = %v, want the failure itself", result)
	}
	if errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v; a terminal failure is not a spent budget", result)
	}
	if attempts != 1 {
		t.Fatalf("operation ran %d time(s), want 1", attempts)
	}
	if asked := clock.waitsAsked(); len(asked) != 0 {
		t.Fatalf("a terminal failure was slept on for %v", asked)
	}
}

// TestNewTerminalErrorOverridesARetryableCause covers the veto. A
// transport helper marking every 5xx retryable is right at its own layer;
// the caller that reads the body and finds the account suspended knows
// better, and without a way to say so it must either spend the whole
// budget against a permanent condition or throw away the diagnosis the
// operator needs.
func TestNewTerminalErrorOverridesARetryableCause(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{MaxAttempts: 5},
		networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	unavailable := errors.New("network returned 503")
	suspended := networks.NewTerminalError(networks.NewRetryableError(unavailable, 0))
	attempts := 0
	result := backoff.Do(context.Background(), func(context.Context) error {
		attempts++
		return suspended
	})
	if attempts != 1 {
		t.Fatalf("operation ran %d time(s), want 1; a failure marked terminal is not retried however its cause was marked", attempts)
	}
	if errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v; a terminal verdict is not a spent budget", result)
	}
	if !errors.Is(result, unavailable) {
		t.Fatalf("Do = %v; the transport's diagnosis must survive the veto, or the only way to stop a retry is to discard what the operator needs", result)
	}
	if got := suspended.Error(); !strings.Contains(got, unavailable.Error()) {
		t.Fatalf("Error() = %q, want it to name the failure underneath the veto", got)
	}
	if networks.NewTerminalError(nil) != nil {
		t.Fatalf("NewTerminalError(nil) = %v, want nil; there is no failure to call final", networks.NewTerminalError(nil))
	}
}

// TestRetryBackoffDoReturnsPromptlyOnCancellation is Wait's promise again,
// one level up: a shutdown does not wait out a backoff either, and the
// failure that caused the wait still reaches the caller.
func TestRetryBackoffDoReturnsPromptlyOnCancellation(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   10 * time.Second,
		DelayCap:    10 * time.Second,
		MaxAttempts: 10,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0.25)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busy := errors.New("network is busy")
	done := make(chan error, 1)
	go func() {
		done <- backoff.Do(ctx, func(context.Context) error { return networks.NewRetryableError(busy, 0) })
	}()

	limiterSpinUntil(t, "the retry to park on its first wait", func() bool { return clock.sleeping() == 1 })
	cancel()

	result := <-done
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("Do after cancellation = %v, want context.Canceled", result)
	}
	if !errors.Is(result, busy) {
		t.Fatalf("Do = %v; the failure that caused the wait must survive the cancellation", result)
	}
	if got := clock.Now(); !got.Equal(limiterAnchor) {
		t.Fatalf("clock moved to %v; a cancelled retry must not wait out its %v backoff", got.Sub(limiterAnchor), 2500*time.Millisecond)
	}
}

// TestRetryBackoffDoRefusesAnAlreadyCancelledContext is the limiter's
// ordering rule one level up: a caller already told to stop does not get
// one more request issued on its behalf. The request would reach the
// network and count against the rate, for an answer nobody is waiting for.
func TestRetryBackoffDoRefusesAnAlreadyCancelledContext(t *testing.T) {
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{},
		networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	result := backoff.Do(ctx, func(context.Context) error {
		attempts++
		return nil
	})
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("Do on a cancelled context = %v, want context.Canceled", result)
	}
	if attempts != 0 {
		t.Fatalf("operation ran %d time(s) for a caller that had already been told to stop", attempts)
	}
}

// TestRetryBackoffDoReportsCancellationRatherThanASpentBudget covers the
// check between an attempt and its verdict. A sequence stopped by a
// shutdown reported as an exhausted budget is a message that sends an
// operator to widen a budget that was never the problem.
func TestRetryBackoffDoReportsCancellationRatherThanASpentBudget(t *testing.T) {
	clock := newLimiterFakeClock()
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   100 * time.Millisecond,
		MaxAttempts: 1,
		MaxElapsed:  time.Hour,
	}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	abandoned := errors.New("network call abandoned mid-flight")
	result := backoff.Do(ctx, func(context.Context) error {
		// The shutdown lands while the request is in flight, which is
		// when it lands in practice.
		cancel()
		return networks.NewRetryableError(abandoned, 0)
	})
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", result)
	}
	if errors.Is(result, networks.ErrRetryBudgetExhausted) {
		t.Fatalf("Do = %v; the attempt ceiling was reached, but a shutdown is what stopped the sequence, and reporting it as a spent budget sends an operator to widen a budget that was never the problem", result)
	}
	if !errors.Is(result, abandoned) {
		t.Fatalf("Do = %v; the failure must travel with the cancellation", result)
	}
}

// TestNewRetryBackoffResolvesAndRefusesPolicies covers the boundary
// between "unset", which takes a documented default, and "wrong", which
// is refused.
func TestNewRetryBackoffResolvesAndRefusesPolicies(t *testing.T) {
	t.Run("zero fields take the defaults", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{})
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		want := networks.RetryBackoffPolicy{
			BaseDelay:   networks.DefaultRetryBaseDelay,
			DelayCap:    networks.DefaultRetryDelayCap,
			MaxAttempts: networks.DefaultRetryAttempts,
			MaxElapsed:  networks.DefaultRetryElapsed,
		}
		if got := backoff.Policy(); got != want {
			t.Fatalf("Policy() = %+v, want %+v", got, want)
		}
	})

	t.Run("an unset cap never lands below a set base", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{BaseDelay: 60 * time.Second})
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		if got, want := backoff.Policy().DelayCap, 60*time.Second; got != want {
			t.Fatalf("DelayCap = %v, want %v", got, want)
		}
	})

	refused := map[string]networks.RetryBackoffPolicy{
		"negative base":     {BaseDelay: -time.Second},
		"negative cap":      {DelayCap: -time.Second},
		"negative attempts": {MaxAttempts: -1},
		"negative elapsed":  {MaxElapsed: -time.Second},
		"cap below base":    {BaseDelay: 5 * time.Second, DelayCap: time.Second},
	}
	for name, policy := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := networks.NewRetryBackoff(policy); !errors.Is(err, networks.ErrRetryBackoffPolicy) {
				t.Fatalf("NewRetryBackoff(%+v) error = %v, want ErrRetryBackoffPolicy", policy, err)
			}
		})
	}
}

// TestPacingOptionsIgnoreNilReplacements covers the three guards that keep
// an option from installing nothing. Each of them replaces a field the
// code then calls unconditionally, so a nil that got through would surface
// as a panic inside a poll cycle - or, for the jitter, as no panic at all
// and a frozen delay that retries every replica in lockstep.
func TestPacingOptionsIgnoreNilReplacements(t *testing.T) {
	t.Run("a nil limiter clock keeps the real one", func(t *testing.T) {
		limiter, err := networks.NewRateLimiter(1000, 1, networks.WithRateLimiterClock(nil))
		if err != nil {
			t.Fatalf("NewRateLimiter: %v", err)
		}
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("a nil backoff clock keeps the real one", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay:   time.Nanosecond,
			DelayCap:    time.Nanosecond,
			MaxAttempts: 2,
		}, networks.WithRetryBackoffClock(nil), networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		busy := errors.New("network is busy")
		result := backoff.Do(context.Background(), func(context.Context) error {
			return networks.NewRetryableError(busy, 0)
		})
		if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
			t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
		}
	})

	t.Run("a nil jitter source keeps a real one", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay: time.Second,
			DelayCap:  time.Second,
		}, networks.WithRetryBackoffRandom(nil))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		first, err := backoff.Delay(1, 0)
		if err != nil {
			t.Fatalf("Delay(1, 0): %v", err)
		}
		varied := false
		for range 100 {
			next, err := backoff.Delay(1, 0)
			if err != nil {
				t.Fatalf("Delay(1, 0): %v", err)
			}
			if next != first {
				varied = true
				break
			}
		}
		if !varied {
			t.Fatalf("a hundred draws all returned %v; a jitter frozen at one value is a fixed multiplier, which retries every replica in lockstep", first)
		}
	})

	t.Run("a nil retry observer observes nothing", func(t *testing.T) {
		clock := newLimiterFakeClock()
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay:   time.Nanosecond,
			DelayCap:    time.Nanosecond,
			MaxAttempts: 3,
		}, networks.WithRetryBackoffClock(clock), networks.WithRetryBackoffRandom(limiterFixedRandom(0)),
			networks.WithRetryBackoffOnRetry(nil))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		busy := errors.New("network is busy")
		result := backoff.Do(context.Background(), func(context.Context) error {
			return networks.NewRetryableError(busy, 0)
		})
		if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
			t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
		}
	})
}

// TestPacingRunsOnRealTimeWhenNoClockIsInjected exercises the clock that
// will actually run in production. Every other assertion in this file runs
// against limiterFakeClock, which is the only way to state an exact
// sequence - and the cost of that is a real timer path, with its own
// cancellation semantics and its own answer for a wait of no time, that
// nothing else here touches.
func TestPacingRunsOnRealTimeWhenNoClockIsInjected(t *testing.T) {
	t.Run("a real wait elapses and the request is granted", func(t *testing.T) {
		limiter, err := networks.NewRateLimiter(1000, 1)
		if err != nil {
			t.Fatalf("NewRateLimiter: %v", err)
		}
		for i := range 3 {
			if err := limiter.Wait(context.Background()); err != nil {
				t.Fatalf("request %d: Wait: %v", i+1, err)
			}
		}
	})

	t.Run("a real wait is cut short by cancellation", func(t *testing.T) {
		// One token every hundred seconds, so a limiter that ignored the
		// context would hold this test open for a hundred of them.
		limiter, err := networks.NewRateLimiter(0.01, 1)
		if err != nil {
			t.Fatalf("NewRateLimiter: %v", err)
		}
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("burst request: Wait: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		started := time.Now()
		if err := limiter.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("a cancelled caller sat out %v of a hundred-second rate limit", elapsed)
		}
	})

	t.Run("a real wait of no time at all returns without waiting", func(t *testing.T) {
		backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
			BaseDelay:   time.Nanosecond,
			DelayCap:    time.Nanosecond,
			MaxAttempts: 3,
		}, networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
		if err != nil {
			t.Fatalf("NewRetryBackoff: %v", err)
		}
		busy := errors.New("network is busy")
		result := backoff.Do(context.Background(), func(context.Context) error {
			return networks.NewRetryableError(busy, 0)
		})
		if !errors.Is(result, networks.ErrRetryBudgetExhausted) {
			t.Fatalf("Do = %v, want ErrRetryBudgetExhausted", result)
		}
	})
}

// TestNewRetryableErrorRefusesAFailureWithNoCause keeps the marking from
// being buildable empty. The plausible one-liner fills in the ask from the
// header and forgets the cause, because in the author's head the status
// was the diagnosis - and the artefact of a five-attempt give-up is then
// the word "retryable" and nothing else, which defeats the whole reason
// the last failure travels with every verdict.
func TestNewRetryableErrorRefusesAFailureWithNoCause(t *testing.T) {
	marked := networks.NewRetryableError(nil, 30*time.Second)
	if !errors.Is(marked, networks.ErrRetryableNoCause) {
		t.Fatalf("NewRetryableError(nil, 30s) = %v, want ErrRetryableNoCause", marked)
	}
	var target *networks.RetryableError
	if errors.As(marked, &target) {
		t.Fatalf("NewRetryableError(nil, 30s) is retryable; a whole budget would be spent against a network nobody can afterwards show was refusing anything")
	}

	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{MaxAttempts: 5},
		networks.WithRetryBackoffRandom(limiterFixedRandom(0)))
	if err != nil {
		t.Fatalf("NewRetryBackoff: %v", err)
	}
	attempts := 0
	result := backoff.Do(context.Background(), func(context.Context) error {
		attempts++
		return networks.NewRetryableError(nil, 0)
	})
	if attempts != 1 {
		t.Fatalf("operation ran %d time(s), want 1", attempts)
	}
	if !errors.Is(result, networks.ErrRetryableNoCause) {
		t.Fatalf("Do = %v, want ErrRetryableNoCause", result)
	}
}
