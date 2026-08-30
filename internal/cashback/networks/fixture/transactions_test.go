// The tests for transactions.go: the recorded lifecycle played end to end,
// and the four ways iteration can end - each of which a caller's durable
// cursor depends on being told apart.

package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fixtureTestWindow covers every recorded transaction time and is well inside
// the reference network's 31-day maximum, so a test about anything else is
// never failing over its window.
func fixtureTestWindow() networks.QueryWindow {
	return networks.QueryWindow{
		From: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
	}
}

// fixtureTestCollect drains a sequence, returning what it yielded before any
// error. The values yielded BEFORE a failure are half of what these tests are
// about: an adapter that hands back reports and then fails mid-window is the
// situation a cursor gets wrong.
func fixtureTestCollect[V any](seq iter.Seq2[V, error]) ([]V, error) {
	var values []V
	for value, err := range seq {
		if err != nil {
			return values, err
		}
		values = append(values, value)
	}
	return values, nil
}

// fixtureTestRead runs one whole transaction window, failing the test on an
// immediate error, which is never what the caller is asking about.
func fixtureTestRead(t *testing.T, adapter *Network) ([]networks.Reported, error) {
	t.Helper()
	seq, err := adapter.FetchTransactions(t.Context(), fixtureTestWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	return fixtureTestCollect(seq)
}

// fixtureTestAmount builds an expected figure, which can only fail on a
// mistyped currency in the test itself.
func fixtureTestAmount(t *testing.T, minor int64, currency string) money.Amount {
	t.Helper()
	amount, err := money.New(minor, money.Currency(currency))
	if err != nil {
		t.Fatalf("money.New(%d, %q): %v", minor, currency, err)
	}
	return amount
}

// TestFetchTransactionsPlaysTheRecordedLifecycle is the whole point of this
// package: one transaction observed across four polls as click, pending,
// approved, reversed, with a second transaction sharing the window that is
// never attributed, is re-reported once completely unchanged, and then
// changes status and both amounts at once.
//
// Re-issuing the same window is what moves it along, which is contract rule
// 4: the same question, answered with the network's account of that period as
// it stands now. It is the only mechanism by which a pending transaction is
// ever seen to become confirmed, and an adapter that memoised a window would
// freeze every member's money at pending with no error anywhere.
func TestFetchTransactionsPlaysTheRecordedLifecycle(t *testing.T) {
	t.Parallel()

	type expected struct {
		externalID string
		status     networks.Status
		statusRaw  string
		clickRef   string
		attributed bool
		sale       money.Amount
		commission money.Amount
	}
	eur4999 := fixtureTestAmount(t, 4999, "EUR")
	eur499 := fixtureTestAmount(t, 499, "EUR")
	gbp12000 := fixtureTestAmount(t, 12000, "GBP")
	gbp900 := fixtureTestAmount(t, 900, "GBP")

	lifecycle := [][]expected{
		StageClick: {
			{externalID: "FIX-1001", status: networks.StatusPending, statusRaw: "pending", sale: eur4999, commission: eur499},
		},
		StagePending: {
			{externalID: "FIX-1001", status: networks.StatusPending, statusRaw: "pending", clickRef: RecordedClickRef, attributed: true, sale: eur4999, commission: eur499},
			{externalID: "FIX-1002", status: networks.StatusPending, statusRaw: "pending", sale: gbp12000, commission: gbp900},
		},
		StageApproved: {
			{externalID: "FIX-1001", status: networks.StatusConfirmed, statusRaw: "approved", clickRef: RecordedClickRef, attributed: true, sale: eur4999, commission: eur499},
			{externalID: "FIX-1002", status: networks.StatusPending, statusRaw: "pending", sale: gbp12000, commission: gbp900},
		},
		StageReversed: {
			{externalID: "FIX-1001", status: networks.StatusReversed, statusRaw: "void", clickRef: RecordedClickRef, attributed: true, sale: eur4999, commission: eur499},
			{externalID: "FIX-1002", status: networks.StatusDeclined, statusRaw: "declined", sale: fixtureTestAmount(t, 15000, "GBP"), commission: fixtureTestAmount(t, 1125, "GBP")},
		},
	}

	adapter := fixtureTestAdapter(t)
	for stage, want := range lifecycle {
		if got := adapter.Stage(); got != Stage(stage) {
			t.Fatalf("before the %s read the clock is at %s", Stage(stage), got)
		}
		got, err := fixtureTestRead(t, adapter)
		if err != nil {
			t.Fatalf("%s: %v", Stage(stage), err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: read %d transactions, want %d", Stage(stage), len(got), len(want))
		}
		for i, report := range got {
			expect := want[i]
			ref, attributed := report.ClickRef.Ref()
			switch {
			case report.ExternalID != expect.externalID:
				t.Errorf("%s report %d: id %q, want %q", Stage(stage), i, report.ExternalID, expect.externalID)
			case report.Status != expect.status || report.StatusRaw != expect.statusRaw:
				t.Errorf("%s report %s: %s normalised from %q, want %s from %q",
					Stage(stage), report.ExternalID, report.Status, report.StatusRaw, expect.status, expect.statusRaw)
			case attributed != expect.attributed || ref != expect.clickRef:
				t.Errorf("%s report %s: click reference %s, want present=%t %q",
					Stage(stage), report.ExternalID, report.ClickRef, expect.attributed, expect.clickRef)
			case !report.SaleAmount.Equal(expect.sale) || !report.Commission.Equal(expect.commission):
				t.Errorf("%s report %s: sale %s commission %s, want %s and %s",
					Stage(stage), report.ExternalID, report.SaleAmount, report.Commission, expect.sale, expect.commission)
			}
		}
	}
}

// TestFetchTransactionsStaysAtTheEndOfTheRecording holds the last
// observation. A network goes on reporting a reversal rather than forgetting
// it, so a poller that keeps running finds the same final answer instead of
// falling off the end of the recording.
func TestFetchTransactionsStaysAtTheEndOfTheRecording(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StageReversed))
	first, err := fixtureTestRead(t, adapter)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := fixtureTestRead(t, adapter)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(first) != len(second) || adapter.Stage() != StageReversed {
		t.Fatalf("re-reading the last observation returned %d then %d reports, clock at %s",
			len(first), len(second), adapter.Stage())
	}
}

// TestFetchTransactionsDoesNotAdvanceWhenTheCallerBreaks is contract rule 4's
// resumability. A poller that stopped because a write failed must be able to
// run that window again from the beginning and miss nothing - which it can
// only do if the fixture has not moved on behind its back.
func TestFetchTransactionsDoesNotAdvanceWhenTheCallerBreaks(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StagePending))
	seq, err := adapter.FetchTransactions(t.Context(), fixtureTestWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	seen := 0
	for report, err := range seq {
		if err != nil {
			t.Fatalf("yielded %v", err)
		}
		seen++
		_ = report
		break
	}
	if seen != 1 {
		t.Fatalf("the abandoned read saw %d reports, want 1", seen)
	}
	if got := adapter.Stage(); got != StagePending {
		t.Fatalf("breaking out of the range moved the clock to %s; a half-read window must be re-askable", got)
	}

	again, err := fixtureTestRead(t, adapter)
	if err != nil {
		t.Fatalf("re-reading the window: %v", err)
	}
	if len(again) != 2 || again[0].ExternalID != "FIX-1001" {
		t.Errorf("the re-read returned %d reports starting at %q, want the whole of the %s observation",
			len(again), again[0].ExternalID, StagePending)
	}
}

// TestFetchTransactionsRefusesTheWindowsItCannotBeAsked is contract rule 3's
// checkable half, and it is refused rather than clamped: a caller that asked
// for 90 days and silently received 31 believes it has read a period it has
// not, and advances its cursor past the 59 days it never saw.
func TestFetchTransactionsRefusesTheWindowsItCannotBeAsked(t *testing.T) {
	t.Parallel()

	anchor := fixtureTestWindow().From
	tests := []struct {
		name    string
		window  networks.QueryWindow
		wantErr error
	}{
		{
			name:    "wider than the network allows",
			window:  networks.QueryWindow{From: anchor, To: anchor.Add(32 * 24 * time.Hour)},
			wantErr: networks.ErrWindowTooWide,
		},
		{
			name:    "no upper bound, which is what a forgotten argument carries",
			window:  networks.QueryWindow{From: anchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
		{
			name:    "ending before it starts",
			window:  networks.QueryWindow{From: anchor.Add(time.Hour), To: anchor},
			wantErr: networks.ErrInvalidQueryWindow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adapter := fixtureTestAdapter(t)
			seq, err := adapter.FetchTransactions(t.Context(), tc.window)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("FetchTransactions() error = %v, want one wrapping %v", err, tc.wantErr)
			}
			if seq != nil {
				t.Error("a refused window came back with a sequence; a caller that ranged it would read an answer nobody gave")
			}
			if got := adapter.Stage(); got != StageClick {
				t.Errorf("a refused window moved the clock to %s", got)
			}
		})
	}
}

// TestFetchTransactionsFiltersToTheWindowItWasAsked holds the half-open
// membership rule adjacent windows partition a backfill by, using the port's
// own definition so that every adapter filters identically.
func TestFetchTransactionsFiltersToTheWindowItWasAsked(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StageReversed))
	narrow := networks.QueryWindow{
		From: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC),
	}
	seq, err := adapter.FetchTransactions(t.Context(), narrow)
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, err := fixtureTestCollect(seq)
	if err != nil {
		t.Fatalf("reading the narrow window: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "FIX-1001" {
		t.Fatalf("the narrow window returned %d reports, want only FIX-1001", len(got))
	}
	for _, report := range got {
		if !narrow.Contains(report.TransactedAt) {
			t.Errorf("report %s is dated %s, outside the window %s", report.ExternalID, report.TransactedAt, narrow)
		}
	}
}

// TestFetchTransactionsCarriesTheVerbatimPayload is contract rule 1 at the
// port's edge: the payload is storable JSON, it is the recording's own bytes,
// and it carries facts the normalised columns do not - which is the whole
// reason a normalisation bug can be fixed later without the network's help.
func TestFetchTransactionsCarriesTheVerbatimPayload(t *testing.T) {
	t.Parallel()

	got, err := fixtureTestRead(t, fixtureTestAdapter(t, WithStage(StageApproved)))
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the observation yielded nothing to check")
	}
	for _, report := range got {
		if !json.Valid(report.RawPayload) {
			t.Errorf("report %s carries a payload that is not JSON: %s", report.ExternalID, report.RawPayload)
		}
		if !strings.Contains(string(report.RawPayload), report.ExternalID) {
			t.Errorf("report %s carries a payload that does not mention it: %s", report.ExternalID, report.RawPayload)
		}
		if !strings.Contains(string(report.RawPayload), "advertiser_name") {
			t.Errorf("report %s carries a payload holding only the normalised columns, which could re-derive nothing", report.ExternalID)
		}
	}
}

// TestFetchTransactionsHandsOutItsOwnPayloadBytes refuses the shared backing
// array. The recording is decoded once for the whole process, so a report
// carrying the shared bytes would let one caller edit what another is later
// handed as evidence - and evidence that can be rewritten after the fact is
// what this entire ingestion path exists to prevent.
func TestFetchTransactionsHandsOutItsOwnPayloadBytes(t *testing.T) {
	t.Parallel()

	first, err := fixtureTestRead(t, fixtureTestAdapter(t))
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("the first observation yielded nothing")
	}
	before := string(first[0].RawPayload)
	for i := range first[0].RawPayload {
		first[0].RawPayload[i] = 'X'
	}

	second, err := fixtureTestRead(t, fixtureTestAdapter(t))
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := string(second[0].RawPayload); got != before {
		t.Errorf("editing one caller's payload changed the next caller's evidence:\n got %s\nwant %s", got, before)
	}
}

// TestFetchTransactionsYieldsAbandonedWhenItStopsEarly is contract rule 8,
// and it is the one thing an adapter must not get wrong. A poller interrupted
// at report 400 of 900 that saw a loop end cleanly would record the window as
// read and never ask for the other 500 again - every member owed cashback on
// them silently never credited, with nothing logged.
func TestFetchTransactionsYieldsAbandonedWhenItStopsEarly(t *testing.T) {
	t.Parallel()

	t.Run("cancelled before the first report", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		adapter := fixtureTestAdapter(t, WithStage(StagePending))
		seq, err := adapter.FetchTransactions(ctx, fixtureTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		got, err := fixtureTestCollect(seq)
		if !errors.Is(err, networks.ErrIterationAbandoned) || !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration ended with %v, want one wrapping both ErrIterationAbandoned and context.Canceled", err)
		}
		if len(got) != 0 {
			t.Errorf("a cancelled read yielded %d reports", len(got))
		}
		if stage := adapter.Stage(); stage != StagePending {
			t.Errorf("an abandoned read moved the clock to %s", stage)
		}
	})

	t.Run("cancelled part way through", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		adapter := fixtureTestAdapter(t, WithStage(StageReversed))
		seq, err := adapter.FetchTransactions(ctx, fixtureTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		seen, abandoned := 0, error(nil)
		for _, err := range seq {
			if err != nil {
				abandoned = err
				break
			}
			seen++
			cancel()
		}
		if seen == 0 {
			t.Fatal("nothing was yielded before the cancellation, so this proves nothing about stopping part way")
		}
		if !errors.Is(abandoned, networks.ErrIterationAbandoned) {
			t.Fatalf("iteration ended with %v, want one wrapping ErrIterationAbandoned", abandoned)
		}
	})

	// The case only the check BETWEEN pages can catch, and the reason that
	// check is not redundant with the one inside a page.
	//
	// Iterating an empty page runs no loop body, so an adapter that noticed
	// cancellation only while walking a page's transactions would step over
	// the last one, fall out of its loop with nothing left to read, and end
	// the iteration cleanly - telling a poller the window was whole when the
	// window had been abandoned. Recorded observation 4 ends on an empty
	// page, as a real network's last page usually does, precisely so this can
	// be proved rather than reasoned about.
	//
	// The cancellation lands after the LAST real report, which is what makes
	// the empty page the only page left; the count is read off a clean pass
	// rather than written down, so this stays true if the recording grows.
	t.Run("cancelled before an empty last page", func(t *testing.T) {
		t.Parallel()

		// A clean pass first. StageReversed is the end of the lifecycle and
		// the clock stops there, so the second read below answers from the
		// same observation.
		counting := fixtureTestAdapter(t, WithStage(StageReversed))
		seq, err := counting.FetchTransactions(t.Context(), fixtureTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		whole, err := fixtureTestCollect(seq)
		if err != nil {
			t.Fatalf("the uncancelled read failed: %v", err)
		}
		if len(whole) == 0 {
			t.Fatal("the final observation reported nothing, so there is no last report to cancel after")
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		adapter := fixtureTestAdapter(t, WithStage(StageReversed))
		seq, err = adapter.FetchTransactions(ctx, fixtureTestWindow())
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		seen, abandoned := 0, error(nil)
		for _, err := range seq {
			if err != nil {
				abandoned = err
				break
			}
			seen++
			// Deliberately not breaking: the caller is still willing to
			// read, and it is the adapter that has to notice the context.
			if seen == len(whole) {
				cancel()
			}
		}
		if seen != len(whole) {
			t.Fatalf("saw %d report(s) before the cancellation, want all %d - the empty page was not what stopped this", seen, len(whole))
		}
		if !errors.Is(abandoned, networks.ErrIterationAbandoned) || !errors.Is(abandoned, context.Canceled) {
			t.Fatalf("iteration ended with %v, want one wrapping both ErrIterationAbandoned and context.Canceled", abandoned)
		}
	})
}

// TestFetchTransactionsReportsAWordNobodyMapped is contract rule 2 proved
// against a real adapter rather than asserted about a mock: the reports that
// mapped cleanly arrive, and then the word nobody mapped stops the window
// instead of being guessed at.
func TestFetchTransactionsReportsAWordNobodyMapped(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StageApproved), WithUnmappableStatus())
	got, err := fixtureTestRead(t, adapter)
	if !errors.Is(err, networks.ErrUnmappableStatus) {
		t.Fatalf("iteration ended with %v, want one wrapping ErrUnmappableStatus", err)
	}
	if len(got) == 0 {
		t.Error("the unmappable page came first, so nothing proves the reports before it were still delivered")
	}
	for _, report := range got {
		if !report.Status.Valid() {
			t.Errorf("report %s was yielded carrying status %q", report.ExternalID, report.Status)
		}
	}
	if stage := adapter.Stage(); stage != StageApproved {
		t.Errorf("a window that ended in a refusal moved the clock to %s", stage)
	}
}

// TestFetchTransactionsYieldsAnInjectedFailureMidWindow is contract rule 9.
// The failure is yielded rather than returned immediately, so a caller cannot
// tell an eager adapter from a lazy one by where it surfaced; it strikes
// after the first page, so a caller that classified the error correctly and
// still advanced its cursor over the half-window it had been handed is caught
// here rather than in production.
func TestFetchTransactionsYieldsAnInjectedFailureMidWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind Failure
		want error
	}{
		{kind: FailureUnavailable, want: networks.ErrNetworkUnavailable},
		{kind: FailureRateLimited, want: networks.ErrNetworkRateLimited},
		{kind: FailureRefused, want: networks.ErrNetworkRefused},
	}

	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			t.Parallel()
			adapter := fixtureTestAdapter(t, WithStage(StageReversed), WithFailure(tc.kind, FailureAlways))
			seq, err := adapter.FetchTransactions(t.Context(), fixtureTestWindow())
			if err != nil {
				t.Fatalf("FetchTransactions() refused the window immediately with %v; rule 9 says a network failure is yielded", err)
			}
			got, err := fixtureTestCollect(seq)
			if !errors.Is(err, tc.want) {
				t.Fatalf("iteration ended with %v, want one wrapping %v", err, tc.want)
			}
			if len(got) == 0 {
				t.Error("the failure struck before any report; a caller is then never handed the half-read window it has to refuse to record")
			}
			if stage := adapter.Stage(); stage != StageReversed {
				t.Errorf("a failed read moved the clock to %s", stage)
			}
		})
	}
}

// TestFetchTransactionsRetriesThroughAFiniteFailure is what the poller's
// backoff is for: fail twice, then answer, with the same window each time
// because rule 4 offers no resumption point inside one.
func TestFetchTransactionsRetriesThroughAFiniteFailure(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StagePending), WithFailure(FailureUnavailable, 2))
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := fixtureTestRead(t, adapter); !errors.Is(err, networks.ErrNetworkUnavailable) {
			t.Fatalf("attempt %d ended with %v, want one wrapping ErrNetworkUnavailable", attempt, err)
		}
	}
	got, err := fixtureTestRead(t, adapter)
	if err != nil {
		t.Fatalf("the third attempt ended with %v, want the window", err)
	}
	if len(got) != 2 || adapter.Stage() != StageApproved {
		t.Errorf("the retry read %d reports and left the clock at %s, want 2 and %s", len(got), adapter.Stage(), StageApproved)
	}
}

// TestFetchTransactionsCrossesAPageBoundary holds the awkwardness that makes
// the rest of this file worth anything: the two transactions of one window
// arrive from different recorded response bodies, so an adapter that stopped
// at the end of the first page would be caught here.
func TestFetchTransactionsCrossesAPageBoundary(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithStage(StagePending))
	pages := adapter.recorded.transactionPages(StagePending, false)
	if len(pages) < 2 {
		t.Fatalf("the %s observation records %d pages, so nothing here crosses a boundary", StagePending, len(pages))
	}
	got, err := fixtureTestRead(t, adapter)
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d reports across %d pages, want 2", len(got), len(pages))
	}
	if got[0].ExternalID != "FIX-1001" || got[1].ExternalID != "FIX-1002" {
		t.Errorf("reports arrived as %q then %q, want them in the recorded order", got[0].ExternalID, got[1].ExternalID)
	}
}

// TestFetchTransactionsYieldsOnlyValuesThatValidate is contract rule 7 as a
// caller can check it: every value handed over passes its own Validate, so a
// mis-mapped currency or a blank click reference is caught at the adapter
// rather than at an INSERT halfway through a window.
func TestFetchTransactionsYieldsOnlyValuesThatValidate(t *testing.T) {
	t.Parallel()

	checked := 0
	adapter := fixtureTestAdapter(t)
	for stage := StageClick; stage <= StageReversed; stage++ {
		got, err := fixtureTestRead(t, adapter)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		for _, report := range got {
			checked++
			if err := report.Validate(); err != nil {
				t.Errorf("%s yielded report %s, which does not validate: %v", stage, report.ExternalID, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no report was validated, so this rule judged nothing and passed vacuously")
	}
}
