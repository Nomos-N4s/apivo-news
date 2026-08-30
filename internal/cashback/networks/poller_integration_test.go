// The poller against the two things a network does to it (T061, FR-031):
// stop answering part way through a window, and refuse to be asked so often.
//
// Both are asserted through a real poll into a real Postgres, because what
// is under test is not the pieces - the backoff has its own tests, and so
// does the cursor - but the sentence FR-031 actually makes: "never loses or
// double-counts a window across restarts". That is a claim about the pieces
// together, and it can only be read off the evidence table afterwards.
//
// A NOTE ON WHAT THIS DEMONSTRATES RATHER THAN GUARDS. The port puts pacing
// on the adapter - "the adapter holds itself to it ... pacing its requests
// to RequestsPerSecond" - and no adapter in this repository paces today,
// because the only one serves from testdata and makes no requests at all. So
// the paced adapter below is built here, in the shape a real one will take:
// its page read is wrapped in the module's own RetryBackoff. What that
// proves is that the composition works and that a rate-limited window is not
// lost by it. It does not prove any shipped adapter does this, because none
// yet has anything to pace.

package networks_test

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// pacingTestAdapter reads its pages through a RetryBackoff, which is what
// contract rule 3 asks of an adapter that talks to a rate-limited network.
//
// It wraps the fake adapter rather than replacing it, so everything the
// poller relies on - the declared limits, the window refusal, the recorded
// windows - is the behaviour the other poller tests drive.
type pacingTestAdapter struct {
	*pollerTestAdapter
	backoff *networks.RetryBackoff
	// refusals is how many attempts the network turns away before it
	// answers, counted down as they are spent.
	refusals int
	// attempts is every attempt made, answered or refused.
	attempts int
	// waits is what the backoff slept before each retry, in order.
	waits []time.Duration
}

// FetchTransactions asks for the window through the backoff, exactly as an
// adapter with a real network behind it would ask for a page.
func (a *pacingTestAdapter) FetchTransactions(ctx context.Context, window networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	if err := a.Limits().ValidateWindow(window); err != nil {
		return nil, err
	}
	var page iter.Seq2[networks.Reported, error]
	err := a.backoff.Do(ctx, func(ctx context.Context) error {
		a.attempts++
		if a.refusals > 0 {
			a.refusals--
			// Marked retryable explicitly, because that is the contract:
			// the backoff retries exactly what an adapter has said is
			// worth retrying, and infers nothing from the sentinel. A
			// network's refusal is only a blip if the adapter that
			// understands that network says so.
			return networks.NewRetryableError(networks.ErrNetworkRateLimited, 0)
		}
		var err error
		page, err = a.pollerTestAdapter.FetchTransactions(ctx, window)
		return err
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// pacedAdapter builds one over a network that refuses the given number of
// attempts before answering, on a clock the test drives.
func pacedAdapter(t *testing.T, account networks.PublisherAccount, refusals int, clock *conformPacingClock, reports ...networks.Reported) *pacingTestAdapter {
	t.Helper()
	adapter := &pacingTestAdapter{
		pollerTestAdapter: pollerTestNetwork(account, pollerTestReports(reports...)),
		refusals:          refusals,
	}
	backoff, err := networks.NewRetryBackoff(networks.RetryBackoffPolicy{
		BaseDelay:   time.Second,
		MaxAttempts: 4,
		MaxElapsed:  time.Minute,
	},
		networks.WithRetryBackoffClock(clock),
		// The jitter is pinned at its ceiling so the sequence is the
		// policy's own rather than a range. Left to chance it is FULL
		// jitter - each wait a random fraction of a doubling ceiling - so
		// a later wait may legitimately be shorter than an earlier one,
		// which is the decorrelation the jitter exists for and not
		// something a test may assert against.
		networks.WithRetryBackoffRandom(func() float64 { return 1 }),
		networks.WithRetryBackoffOnRetry(func(_ int, wait time.Duration, _ error) {
			adapter.waits = append(adapter.waits, wait)
		}),
	)
	if err != nil {
		t.Fatalf("NewRetryBackoff(): %v", err)
	}
	adapter.backoff = backoff
	return adapter
}

// TestARateLimitedWindowIsRetriedAndLandsExactlyOnce is the composition the
// first real adapter will be built out of: the network turns two attempts
// away, the backoff waits, the third answers, and the window lands once.
//
// "Once" is the assertion that matters. A retry that re-read a window
// already half persisted would double-count it, and nothing in a poll's
// return value would say so - the count would look like a busy morning.
func TestARateLimitedWindowIsRetriedAndLandsExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := pollerSchemaAccount(ctx, t, tx)
	first, second := pollerSchemaPair(t)

	clock := &conformPacingClock{now: time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)}
	adapter := pacedAdapter(t, account, 2, clock, first, second)

	poll, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("PollForward(): %v", err)
	}
	if poll.Outcome.FirstReports != 2 {
		t.Errorf("the poll reported %s, want both reports stored once", poll.Outcome)
	}
	if rows := storedFor(ctx, t, tx, account); rows != 2 {
		t.Errorf("%d evidence row(s) after two refusals and a retry, want 2", rows)
	}
	if adapter.attempts != 3 {
		t.Errorf("the network was asked %d time(s), want 3 - two refused and one answered", adapter.attempts)
	}
	// The waits actually happened, and grew. The exact durations are the
	// backoff's own business - it jitters, and its sequence has its own
	// tests - so what is asserted here is the property a poll depends on:
	// there was a wait before each retry, and the second was longer than
	// the first, which is what stops a refused network being hammered.
	if len(adapter.waits) != 2 {
		t.Fatalf("the backoff waited %d time(s), want 2: %v", len(adapter.waits), adapter.waits)
	}
	if adapter.waits[1] <= adapter.waits[0] {
		t.Errorf("the waits were %v then %v, want the second longer", adapter.waits[0], adapter.waits[1])
	}
	// Each within its own doubling ceiling. Asserted as bounds rather than
	// as exact durations: the ceiling is computed in floating point, so an
	// equality here would be a test of arithmetic rounding rather than of
	// the policy.
	if adapter.waits[0] > time.Second || adapter.waits[1] > 2*time.Second {
		t.Errorf("the waits were %v then %v, want each inside its ceiling of 1s and 2s", adapter.waits[0], adapter.waits[1])
	}
	if slept := adapter.waits[0] + adapter.waits[1]; clock.total() != slept {
		t.Errorf("the clock advanced %v, want the %v the backoff chose; a wait that is chosen and not slept is no wait at all",
			clock.total(), slept)
	}
	// One window, asked for once per ANSWERED attempt: the refusals never
	// reached the fake beneath, so nothing was half-read and re-read.
	if len(adapter.windows) != 1 {
		t.Errorf("the reports were read for %d window(s), want 1: %v", len(adapter.windows), adapter.windows)
	}
	if got := cursorsOf(ctx, t, tx, account).CursorAt.Time; !got.Equal(poll.Window.To) {
		t.Errorf("the cursor stands at %s, want the window's end %s", got, poll.Window.To)
	}
}

// TestASpentRetryBudgetLosesNoWindow is the other end of the same story. The
// network never answers, the sequence gives up, and the poll fails - which
// must leave the account exactly as it was, so the next tick reads the same
// period rather than the one after it.
func TestASpentRetryBudgetLosesNoWindow(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := pollerSchemaAccount(ctx, t, tx)
	first, second := pollerSchemaPair(t)

	clock := &conformPacingClock{now: time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)}
	// More refusals than the policy has attempts.
	adapter := pacedAdapter(t, account, 9, clock, first, second)
	poller := pollerSchemaPoller(t, tx)

	_, err := poller.PollForward(ctx, adapter)
	if err == nil {
		t.Fatal("a network that never answered produced a successful poll")
	}
	if !errors.Is(err, networks.ErrNetworkRateLimited) {
		t.Errorf("the failure is %v, want it to carry the network's own reason", err)
	}
	if rows := storedFor(ctx, t, tx, account); rows != 0 {
		t.Errorf("%d evidence row(s) after a window that was never read, want 0", rows)
	}
	if cursorsOf(ctx, t, tx, account).CursorAt.Valid {
		t.Fatal("the cursor moved over a window the network never answered for")
	}

	// The network recovers. The same period is read, and lands once.
	adapter.refusals = 0
	poll, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("the poll after the network recovered: %v", err)
	}
	if poll.Outcome.FirstReports != 2 {
		t.Errorf("the recovery reported %s, want both reports stored once", poll.Outcome)
	}
}

// TestARestartReReadsAtMostOneWindowAndSkipsNone is FR-031 stated the way an
// operator would ask it - "what did we lose?" - and the answer has to be
// nothing, twice over: no evidence from the half-read window, and no period
// stepped over.
func TestARestartReReadsAtMostOneWindowAndSkipsNone(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := pollerSchemaAccount(ctx, t, tx)
	first, second := pollerSchemaPair(t)

	// The second window dies after yielding one report - the shape of a
	// process killed, or a network that stopped answering, mid-window.
	//
	// That report is a transaction NEITHER window has reported before, and
	// deliberately so. Yielding one already stored would make "nothing
	// survived" true for the wrong reason: the dedup path would have
	// swallowed it whether the transaction rolled back or not, and the test
	// could not tell the two apart.
	partial := pollerTestReport(t, "FIX-1003", networks.StatusPending, "pending",
		pollerSchemaStart.Add(144*time.Hour), 250)
	adapter := pollerTestNetwork(account, func(call int, _ networks.QueryWindow) ([]networks.Reported, error) {
		if call == 1 {
			return []networks.Reported{partial}, networks.ErrNetworkUnavailable
		}
		return []networks.Reported{first, second}, nil
	})
	poller := pollerSchemaPoller(t, tx)

	firstPoll, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("the first poll: %v", err)
	}
	storedAfterFirst := storedFor(ctx, t, tx, account)

	if _, err := poller.PollForward(ctx, adapter); err == nil {
		t.Fatal("the poll that lost the network reported success")
	}
	if got := storedFor(ctx, t, tx, account); got != storedAfterFirst {
		t.Errorf("%d evidence row(s) survived the half-read window, want the %d from before it", got, storedAfterFirst)
	}
	// Named, so the assertion above cannot pass because the report happened
	// to be one the dedup path would have swallowed anyway.
	var partials int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.network_transaction where network_id = $1 and external_id = $2`,
		string(account.Network()), partial.ExternalID).Scan(&partials); err != nil {
		t.Fatalf("counting the half-read window's report: %v", err)
	}
	if partials != 0 {
		t.Errorf("%s was stored by a window that never finished reading; the transaction did not roll back", partial.ExternalID)
	}

	recovered, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("the poll after the failure: %v", err)
	}
	if len(adapter.windows) != 3 {
		t.Fatalf("the network was asked for %d window(s), want 3", len(adapter.windows))
	}
	if !adapter.windows[2].From.Equal(adapter.windows[1].From) || !adapter.windows[2].To.Equal(adapter.windows[1].To) {
		t.Errorf("after the failure the poller read %s, want the window it abandoned, %s",
			adapter.windows[2], adapter.windows[1])
	}
	if !adapter.windows[1].From.Equal(firstPoll.Window.To) {
		t.Errorf("the abandoned window started at %s, want where the first poll stopped, %s",
			adapter.windows[1].From, firstPoll.Window.To)
	}
	// And the re-read costs nothing: those reports were stored by the first
	// poll, so the recovery double-counts none of them.
	if recovered.Outcome.FirstReports != 0 || recovered.Outcome.Unchanged != 2 {
		t.Errorf("the recovery reported %s, want both reports recognised as already stored", recovered.Outcome)
	}
}
