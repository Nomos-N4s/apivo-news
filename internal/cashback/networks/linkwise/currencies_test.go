package linkwise_test

// Which currency a transaction is in, joined from the programme it belongs
// to (C-6).
//
// This is the file that would have caught the bug it was written for. The
// transaction report carries no currency field, so an adapter has to get one
// from somewhere; declaring one per account is right for 329 of the 334
// programmes the recorded account is joined to and silently wrong for the
// five that report in PLN and USD. Wrong here is not an error message - it is
// a member paid roughly four times what they earned.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// aTestClock is a clock a test moves by hand, so the currency index can be
// aged past its refresh interval without waiting an hour.
type aTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *aTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep does not sleep. Every rate in this file is set high enough that
// nothing waits on pacing, so returning immediately changes no outcome and
// keeps the suite fast.
func (c *aTestClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (c *aTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// oneTransactionIn builds a report body carrying a single transaction against
// the named programme.
func oneTransactionIn(programmeID string) string {
	return `[{"id":9001,"program":{"id":` + programmeID + `},"amount":"19.51","commission":"2.93",` +
		`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Validated","date":"2024-06-08T00:00:00+03:00"}}]`
}

// TestATransactionTakesItsProgrammesCurrency is the finding, as an assertion.
//
// Programme 13847 reports in PLN and 12975 in USD; the rest of the recorded
// account is EUR. A per-account declaration gets these two wrong while
// failing nothing.
func TestATransactionTakesItsProgrammesCurrency(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		programme string
		want      money.Currency
	}{
		{programme: "5", want: "EUR"},
		{programme: "13847", want: "PLN"},
		{programme: "12975", want: "USD"},
	} {
		t.Run(tt.programme, func(t *testing.T) {
			t.Parallel()
			client := servingTransactions(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(oneTransactionIn(tt.programme)))
			})
			seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
			if err != nil {
				t.Fatalf("FetchTransactions(): %v", err)
			}
			got, failed := collect(t, seq)
			if failed != nil || len(got) != 1 {
				t.Fatalf("the iteration yielded %d transactions and ended with %v", len(got), failed)
			}
			if got[0].SaleAmount.Currency != tt.want {
				t.Errorf("the sale is in %q, want %q: programme %s reports in it",
					got[0].SaleAmount.Currency, tt.want, tt.programme)
			}
			if got[0].Commission.Currency != tt.want {
				t.Errorf("the commission is in %q, want %q", got[0].Commission.Currency, tt.want)
			}
			// One currency for the pair, because the evidence row stores one
			// column for both figures.
			if got[0].SaleAmount.Currency != got[0].Commission.Currency {
				t.Errorf("the sale is in %q and the commission in %q", got[0].SaleAmount.Currency, got[0].Commission.Currency)
			}
		})
	}
}

// TestTheCurrencyReadAsksForEveryProgramme, not only the joined ones.
//
// The catalogue asks joined=yes because it wants routes this account may
// promote. This read wants the currency of any programme a transaction can
// NAME - including one the account has since left, which a trailing sweep
// re-reading a ninety-day-old window will meet. status=all is here for the
// same reason: a paused programme's old transactions still have to be
// denominated.
func TestTheCurrencyReadAsksForEveryProgramme(t *testing.T) {
	t.Parallel()

	var programmeQueries []string
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			programmeQueries = append(programmeQueries, r.URL.RawQuery)
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}}]`))
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("5")))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	if _, failed := collect(t, seq); failed != nil {
		t.Fatalf("the iteration ended with %v", failed)
	}

	if len(programmeQueries) != 1 {
		t.Fatalf("the programme list was read %d times for one window", len(programmeQueries))
	}
	for _, want := range []string{"joined=all", "status=all", "format=json"} {
		if !strings.Contains(programmeQueries[0], want) {
			t.Errorf("the programme read asked %q, want it to carry %q", programmeQueries[0], want)
		}
	}
	if strings.Contains(programmeQueries[0], "joined=yes") {
		t.Error("the currency read asked only for joined programmes; a transaction can name one this account has since left")
	}
}

// TestTheProgrammeListIsReadOnceAndThenCached. It is a single response of
// about four megabytes taking several seconds, so reading it per transaction
// would cost more than the window it is denominating.
func TestTheProgrammeListIsReadOnceAndThenCached(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			reads.Add(1)
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}}]`))
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("5")))
	})

	for range 3 {
		seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		if _, failed := collect(t, seq); failed != nil {
			t.Fatalf("the iteration ended with %v", failed)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("the programme list was read %d times across three windows, want once", got)
	}
}

// TestTheProgrammeListIsReadAgainWhenItIsStale. The refresh is the backstop
// that lets a currency correction reach a running deployment without a
// restart.
func TestTheProgrammeListIsReadAgainWhenItIsStale(t *testing.T) {
	t.Parallel()

	clock := &aTestClock{now: time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)}
	var reads atomic.Int32
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			reads.Add(1)
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}}]`))
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("5")))
	}, linkwise.WithClock(clock), linkwise.WithCurrencyRefresh(time.Hour))

	read := func() {
		t.Helper()
		seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		if _, failed := collect(t, seq); failed != nil {
			t.Fatalf("the iteration ended with %v", failed)
		}
	}

	read()
	clock.advance(59 * time.Minute)
	read()
	if got := reads.Load(); got != 1 {
		t.Errorf("the list was read %d times inside the refresh interval, want once", got)
	}
	clock.advance(2 * time.Minute)
	read()
	if got := reads.Load(); got != 2 {
		t.Errorf("the list was read %d times after the interval passed, want twice", got)
	}
}

// TestAMissAgainstAFreshListIsNotReReadImmediately.
//
// A miss has one obvious cause - a programme joined since the last read - and
// one immediate re-read is worth it for that. But re-reading a list fetched
// in the same call asks the same question at four megabytes a time, so that
// one case is excluded: the miss is reported instead, naming the programme.
func TestAMissAgainstAFreshListIsNotReReadImmediately(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			reads.Add(1)
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}}]`))
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("999")))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)

	if !errors.Is(failed, linkwise.ErrUnknownProgrammeCurrency) {
		t.Fatalf("the iteration ended with %v, want ErrUnknownProgrammeCurrency", failed)
	}
	if len(got) != 0 {
		t.Errorf("%d transactions were yielded in a currency nobody established", len(got))
	}
	if !strings.Contains(failed.Error(), "999") {
		t.Errorf("the failure does not name the programme somebody has to look at: %v", failed)
	}
	if r := reads.Load(); r != 1 {
		t.Errorf("the list was read %d times, want the one read this call already made", r)
	}
}

// TestAProgrammeJoinedSinceTheListWasReadIsFound is the case the re-read
// exists for, in the sequence it actually happens in: the list is read, the
// account joins a retailer, and a transaction against it arrives before the
// refresh interval is up. Without the re-read that window fails for an hour -
// a self-inflicted outage caused by a new retailer.
func TestAProgrammeJoinedSinceTheListWasReadIsFound(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32
	var naming atomic.Value
	naming.Store("5")
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			if reads.Add(1) == 1 {
				_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}},{"id":999,"currency":{"code":"PLN"}}]`))
			return
		}
		id, _ := naming.Load().(string)
		_, _ = w.Write([]byte(oneTransactionIn(id)))
	}, linkwise.WithCurrencyRefresh(time.Hour))

	// The first window populates the index from a list that does not carry
	// programme 999 yet.
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	if _, failed := collect(t, seq); failed != nil {
		t.Fatalf("the first window ended with %v", failed)
	}

	// Now the account joins programme 999 and a transaction against it
	// arrives, well inside the refresh interval.
	naming.Store("999")
	seq, err = client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil || len(got) != 1 {
		t.Fatalf("the iteration yielded %d transactions and ended with %v", len(got), failed)
	}
	if got[0].SaleAmount.Currency != "PLN" {
		t.Errorf("the sale is in %q, want PLN from the programme that appeared on the re-read", got[0].SaleAmount.Currency)
	}
	if r := reads.Load(); r != 2 {
		t.Errorf("the list was read %d times, want the first read and exactly one re-read on the miss", r)
	}
}

// TestAnUnreadableProgrammeListFailsTheWindow, and does NOT fall back.
//
// This is the whole discipline of the file in one case. An adapter that met
// an unreadable programme list and reached for a default would be the silent
// mis-denomination the join exists to remove, arrived at by a different road.
// A window that cannot be read is visible; a window read in the wrong
// currency is not.
func TestAnUnreadableProgrammeListFailsTheWindow(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("5")))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collectMerchantsFreeTransactions(t, seq)
	if !errors.Is(failed, networks.ErrNetworkRefused) {
		t.Fatalf("the iteration ended with %v, want the programme read's own refusal", failed)
	}
	if len(got) != 0 {
		t.Errorf("%d transactions were yielded in a currency nobody established", len(got))
	}
}

// collectMerchantsFreeTransactions is [collect] under a name that says which
// sequence it drains, for the one case above that reads oddly otherwise.
func collectMerchantsFreeTransactions(t *testing.T, seq func(func(networks.Reported, error) bool)) ([]networks.Reported, error) {
	t.Helper()
	return collect(t, seq)
}

// TestAProgrammeWithAnUnreadableCurrencyIsSkippedRatherThanFatal. One
// programme with a broken currency code makes ITS OWN transactions
// unreadable, naming itself; there is no reason for it to stop every other
// programme's window from being polled.
func TestAProgrammeWithAnUnreadableCurrencyIsSkippedRatherThanFatal(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			_, _ = w.Write([]byte(`[{"id":5,"currency":{"code":"EUR"}},{"id":6,"currency":{"code":"euro"}}]`))
			return
		}
		_, _ = w.Write([]byte(oneTransactionIn("5")))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil || len(got) != 1 {
		t.Fatalf("a neighbour's broken currency stopped a readable window: %d transactions, %v", len(got), failed)
	}
	if got[0].SaleAmount.Currency != "EUR" {
		t.Errorf("the sale is in %q, want EUR", got[0].SaleAmount.Currency)
	}
}

// TestTheProductionClockHonoursCancellation.
//
// The client's own clock is only reached when no clock was injected, so no
// other test in this package exercises it - and the one thing it must get
// right is the one thing a hand-rolled timer usually gets wrong. A Sleep that
// ignored the context would hold a shutting-down poller open for the whole of
// a rate-limit wait.
func TestTheProductionClockHonoursCancellation(t *testing.T) {
	t.Parallel()

	client, err := linkwise.New(anAccount(t), linkwise.WithCredential(theUsername, thePassword))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	clock := client.Clock()

	// A non-positive wait returns without waiting at all.
	if err := clock.Sleep(t.Context(), 0); err != nil {
		t.Errorf("Sleep(0) = %v, want it to return immediately", err)
	}
	// A real wait ends on the context rather than on the timer.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := clock.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v, want context.Canceled", err)
	}
	// And a wait that finishes reports nothing.
	if err := clock.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("Sleep(1ms) = %v, want nil", err)
	}
}
