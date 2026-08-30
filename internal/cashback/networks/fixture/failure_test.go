// The tests for failure.go: that the knob produces the port's own three
// classifications and nothing of its own, and that a finite count runs out
// exactly when it says it will.

package fixture

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// TestFailureClassifiesToThePortsSentinels is contract rule 9 at this
// adapter. A caller decides whether to retry the window, wait, or stop and
// raise the account, and it decides on the sentinel - so an adapter that
// invented an error of its own here would satisfy the knob and break the
// contract.
func TestFailureClassifiesToThePortsSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind Failure
		want error
	}{
		{kind: FailureNone, want: nil},
		{kind: FailureUnavailable, want: networks.ErrNetworkUnavailable},
		{kind: FailureRateLimited, want: networks.ErrNetworkRateLimited},
		{kind: FailureRefused, want: networks.ErrNetworkRefused},
	}

	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			t.Parallel()
			got := tc.kind.classify()
			if tc.want == nil {
				if got != nil {
					t.Fatalf("%s.classify() = %v, want nil", tc.kind, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("%s.classify() = %v, want one wrapping %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestFailureClassifyRefusesAKindItDoesNotKnow refuses a default for the same
// reason contract rule 2 refuses one for a status word: a caller that asked
// for a failure this package does not have is asking for a retry decision
// nobody defined, and quietly answering "unavailable" would make a test about
// the terminal path pass while exercising the retryable one.
func TestFailureClassifyRefusesAKindItDoesNotKnow(t *testing.T) {
	t.Parallel()

	err := Failure("gone_weird").classify()
	if err == nil {
		t.Fatal("Failure(\"gone_weird\").classify() = nil; a kind nobody defined must not answer as one that is")
	}
	if errors.Is(err, networks.ErrNetworkRefused) || errors.Is(err, networks.ErrNetworkRateLimited) {
		t.Errorf("an unknown kind classified as a specific failure: %v", err)
	}
}

func TestInjectorIsSilentUntilItIsSet(t *testing.T) {
	t.Parallel()

	var failures injector
	for range 3 {
		if err := failures.take(); err != nil {
			t.Fatalf("take() on an unconfigured injector = %v, want nil", err)
		}
	}
}

// TestInjectorConsumesAFiniteCount is what a backoff loop is driven by: fail
// twice, then answer. A counter that only came down on success would never
// come down at all, and the retry would never be seen to succeed.
func TestInjectorConsumesAFiniteCount(t *testing.T) {
	t.Parallel()

	var failures injector
	failures.set(FailureUnavailable, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := failures.take(); !errors.Is(err, networks.ErrNetworkUnavailable) {
			t.Fatalf("attempt %d: take() = %v, want one wrapping ErrNetworkUnavailable", attempt, err)
		}
	}
	if err := failures.take(); err != nil {
		t.Fatalf("attempt 3: take() = %v, want nil; a count of 2 must be spent after two calls", err)
	}
	if kind, left := failures.peek(); kind != FailureNone || left != 0 {
		t.Errorf("a spent injection reads as (%s, %d), want (%s, 0)", kind, left, FailureNone)
	}
}

// TestInjectorAlwaysNeverRunsOut is what a terminal failure actually looks
// like: a revoked credential does not clear itself after three attempts, and
// a poller that kept retrying would loop forever with a frozen cursor.
func TestInjectorAlwaysNeverRunsOut(t *testing.T) {
	t.Parallel()

	var failures injector
	failures.set(FailureRefused, FailureAlways)
	for attempt := 1; attempt <= 6; attempt++ {
		if err := failures.take(); !errors.Is(err, networks.ErrNetworkRefused) {
			t.Fatalf("attempt %d: take() = %v, want one wrapping ErrNetworkRefused", attempt, err)
		}
	}
}

func TestInjectorSetClearsAStandingFailure(t *testing.T) {
	t.Parallel()

	var failures injector
	failures.set(FailureRefused, FailureAlways)
	failures.set(FailureNone, FailureAlways)
	if err := failures.take(); err != nil {
		t.Fatalf("take() after clearing = %v, want nil", err)
	}
}

// TestInjectorHandsOutExactlyItsCountUnderConcurrency is the race detector's
// test and the counter's: a limiter hammering the adapter from several
// goroutines must be refused exactly as many times as the count said, not
// once per goroutine and not once in total.
func TestInjectorHandsOutExactlyItsCountUnderConcurrency(t *testing.T) {
	t.Parallel()

	const (
		callers = 16
		budget  = 5
	)
	var failures injector
	failures.set(FailureRateLimited, budget)

	var refused atomic.Int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			if err := failures.take(); err != nil {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := refused.Load(); got != budget {
		t.Errorf("%d of %d callers were refused, want exactly %d", got, callers, budget)
	}
}
