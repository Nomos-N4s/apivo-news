package main

// What the money invariants look like in a scrape (ADR-0007).

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// TestTheLedgerVerdictReachesTheScrape, per currency and with the count of
// currencies covered beside it - because a pass that summed nothing is
// vacuously clean, and an alert that cannot tell that from a verified zero
// goes quiet exactly when the ledger stops being visible.
func TestTheLedgerVerdictReachesTheScrape(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	money.LedgerSummed(context.Background(), wallet.LedgerSum{
		Currencies: 2,
		Net:        map[string]int64{"EUR": 0, "GBP": 250},
	})

	body := scrape(t, p)
	for _, want := range []string{
		"apivo_cashback_ledger_net",
		"apivo_cashback_ledger_currencies_checked",
		`currency="EUR"`,
		`currency="GBP"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not carry %q:\n%s", want, body)
		}
	}
	// The satisfied interface, asserted rather than assumed: the wiring in
	// main.go is the only other thing that would have caught a drift here.
	var _ wallet.LedgerObserver = money
}

// TestACorrectedCurrencyStopsAlerting.
//
// This is why LedgerSum carries every currency rather than only the broken
// ones. The instrument is a gauge and a gauge nobody sets stays where it was:
// without a reported zero on the pass after the fix, an alert on "the ledger
// does not net to zero" would keep firing forever, and the person on call
// would learn to ignore it.
func TestACorrectedCurrencyStopsAlerting(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	money.LedgerSummed(context.Background(), wallet.LedgerSum{
		Currencies: 1, Net: map[string]int64{"EUR": 500},
	})
	if body := scrape(t, p); !strings.Contains(body, "apivo_cashback_ledger_net") {
		t.Fatalf("the imbalance was never recorded:\n%s", body)
	}
	// The incident is resolved and the next pass sums a balanced ledger.
	money.LedgerSummed(context.Background(), wallet.LedgerSum{
		Currencies: 1, Net: map[string]int64{"EUR": 0},
	})

	body := scrape(t, p)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "apivo_cashback_ledger_net{") {
			continue
		}
		if !strings.HasSuffix(line, " 0") {
			t.Errorf("EUR still reads as out of balance after a clean pass: %q", line)
		}
	}
}

// TestAParkedDeliveryIsCountedByTypeAndNotById.
//
// The event id and its subject are unbounded, and an unbounded label is how a
// metrics store dies - not gradually, but at the moment somebody replays a
// backlog. The identifiers belong in the log line beside the call, which is
// where an operator looking for one row will be.
func TestAParkedDeliveryIsCountedByTypeAndNotById(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	id := uuid.New()
	money.deadLettered(context.Background(), events.DeadLetter{
		Subscriber: "cashback-account-closures",
		Event:      events.Event{EventID: id, Type: "identity.account.deleted"},
		Attempts:   3,
		LastError:  "the handler refused",
	})

	body := scrape(t, p)
	if !strings.Contains(body, "apivo_events_dead_letters") {
		t.Fatalf("the parked delivery was not counted:\n%s", body)
	}
	if !strings.Contains(body, `subscriber="cashback-account-closures"`) ||
		!strings.Contains(body, `type="identity.account.deleted"`) {
		t.Errorf("the scrape does not carry the bounded labels:\n%s", body)
	}
	if strings.Contains(body, id.String()) {
		t.Errorf("the event id became a metric label, which is unbounded cardinality:\n%s", body)
	}
}

// TestTheMoneyInstrumentsWorkWithTelemetryOff, which is how every environment
// runs today.
func TestTheMoneyInstrumentsWorkWithTelemetryOff(t *testing.T) {
	t.Parallel()
	money, err := newMoneyInstruments(nil)
	if err != nil {
		t.Fatalf("building against a nil provider: %v", err)
	}
	money.LedgerSummed(context.Background(), wallet.LedgerSum{
		Currencies: 1, Net: map[string]int64{"EUR": 0},
	})
	money.deadLettered(context.Background(), events.DeadLetter{
		Subscriber: "s", Event: events.Event{Type: "t"},
	})
}

// TestTheLifecycleFunnelReachesTheScrape. The Outcome struct was computed and
// thrown away at the job boundary, and it is the earning funnel: opened
// pending, held by a rule, or queued because the report matched no click.
func TestTheLifecycleFunnelReachesTheScrape(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	money.LifecycleRan(context.Background(), earnings.Outcome{
		Credited: 3, Held: 1, Queued: 2, Confirmed: 4, Reversed: 1, Failed: 1,
	})

	body := scrape(t, p)
	if !strings.Contains(body, "apivo_cashback_credits") {
		t.Fatalf("the lifecycle outcome is not in the scrape:\n%s", body)
	}
	for _, want := range []string{"credited", "held", "queued", "confirmed", "reversed", "failed"} {
		if !strings.Contains(body, `outcome="`+want+`"`) {
			t.Errorf("the scrape does not carry outcome=%q:\n%s", want, body)
		}
	}
	// Awaiting was zero on this pass and a counter is a rate: adding nothing
	// and adding zero are the same fact, and a series nobody has ever
	// incremented has nothing to say.
	if strings.Contains(body, `outcome="awaiting"`) {
		t.Errorf("a zero was recorded as if something had happened:\n%s", body)
	}

	var _ earnings.LifecycleObserver = money
}

// TestOnlyNewlyRecordedDifferencesAreCounted.
//
// Found is every difference the pass DERIVED, and a re-import of the same
// statement derives all of them again while writing none. Counting Found would
// turn a retried import into a fresh discrepancy every time - and a retried
// import is exactly what an operator does when the first one failed.
func TestOnlyNewlyRecordedDifferencesAreCounted(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	// A pass that derived three and wrote none: the same statement, imported
	// a second time.
	money.differencesFound(context.Background(), ops.Detection{
		Found: []ops.Difference{
			{Kind: ops.ReportedNotPaid}, {Kind: ops.PaidNotReported}, {Kind: ops.AmountMismatch},
		},
		Recorded: 0,
	})
	if body := scrape(t, p); strings.Contains(body, "apivo_cashback_reconciliation_differences") {
		t.Fatalf("a re-import that wrote nothing was counted as a discrepancy:\n%s", body)
	}

	// And a pass that did write something is counted.
	money.differencesFound(context.Background(), ops.Detection{
		Found:    []ops.Difference{{Kind: ops.ReportedNotPaid}, {Kind: ops.AmountMismatch}},
		Recorded: 2,
	})
	body := scrape(t, p)
	if !strings.Contains(body, "apivo_cashback_reconciliation_differences") {
		t.Fatalf("a recorded difference was not counted:\n%s", body)
	}
	if !strings.Contains(body, "} 2") {
		t.Errorf("the count is not the 2 that were recorded:\n%s", body)
	}
}

// TestADetectionThatFailedCountsNothing. The decorator is where that is
// decided, and a number that moved on a pass which did not complete would say
// a discrepancy was found where none was read.
func TestADetectionThatFailedCountsNothing(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	counted := countedReconciliation{ReconciliationStore: failingDetector{}, money: money}

	if _, err := counted.DetectDifferences(context.Background(), uuid.New()); err == nil {
		t.Fatal("the failing detector reported success")
	}
	if body := scrape(t, p); strings.Contains(body, "apivo_cashback_reconciliation_differences") {
		t.Errorf("a failed detection was counted:\n%s", body)
	}
}

// failingDetector is a reconciliation store whose detection never completes.
// Only DetectDifferences is reachable through the decorator under test; the
// embedded nil interface makes any other call a deliberate panic rather than
// a silent zero.
//
// It returns a POPULATED detection alongside its error, which *PGStore does
// not - PGStore answers a zero Detection on every failure path. The fake is
// deliberately less careful, because the decorator must not be relying on its
// dependency's tidiness: a store that wrote two rows and then failed to commit
// has recorded nothing, whatever its return value says.
type failingDetector struct {
	ops.ReconciliationStore
}

func (failingDetector) DetectDifferences(context.Context, uuid.UUID) (ops.Detection, error) {
	return ops.Detection{
		Found:    []ops.Difference{{Kind: ops.ReportedNotPaid}, {Kind: ops.AmountMismatch}},
		Recorded: 2,
	}, errors.New("the detection could not run")
}

// TestNilInstrumentsAreSafeAtEverySeam. The wiring tests pass none, so a nil
// receiver reaching any of the four reports is not hypothetical.
func TestNilInstrumentsAreSafeAtEverySeam(t *testing.T) {
	t.Parallel()
	var money *moneyInstruments
	ctx := context.Background()
	money.LedgerSummed(ctx, wallet.LedgerSum{Currencies: 1, Net: map[string]int64{"EUR": 0}})
	money.deadLettered(ctx, events.DeadLetter{Subscriber: "s", Event: events.Event{Type: "t"}})
	money.LifecycleRan(ctx, earnings.Outcome{Credited: 1})
	money.differencesFound(ctx, ops.Detection{Recorded: 1})

	// And through the decorator, which is how a wiring test reaches one.
	counted := countedReconciliation{ReconciliationStore: failingDetector{}, money: money}
	if _, err := counted.DetectDifferences(ctx, uuid.New()); err == nil {
		t.Error("the failing detector reported success")
	}
}

// TestTheCanaryVerdictIsOneHot.
//
// Exactly one state reads 1 and the other three read 0, written on every
// pass. A single gauge carrying a state code would need a legend nobody has
// at three in the morning; and writing only the current state would leave the
// previous one at 1 forever, so a network that went from suspect to retired
// would keep firing its alarm until the process restarted.
func TestTheCanaryVerdictIsOneHot(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	// Suspect: clicked through, nothing matched, past the threshold.
	money.AttributionJudged(context.Background(), networks.AttributionVerdict{
		Network:      networks.NetworkID("linkwise"),
		FirstClickAt: time.Now().Add(-time.Hour),
		Unattributed: 5,
	})

	body := scrape(t, p)
	if got := canaryStates(t, body); !reflect.DeepEqual(got, map[string]string{
		networks.AttributionIdle:     "0",
		networks.AttributionRetired:  "0",
		networks.AttributionSuspect:  "1",
		networks.AttributionWatching: "0",
	}) {
		t.Errorf("the canary states read %v, want only suspect set:\n%s", got, body)
	}
	if !strings.Contains(body, "apivo_cashback_attribution_unattributed") {
		t.Errorf("the unattributed count is not in the scrape:\n%s", body)
	}

	var _ networks.AttributionObserver = money
}

// TestARetiredCanaryStopsAlerting. The state that mattered yesterday must not
// still read 1 today.
func TestARetiredCanaryStopsAlerting(t *testing.T) {
	t.Parallel()
	p := observedProvider(t)
	money, err := newMoneyInstruments(p)
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	network := networks.NetworkID("linkwise")

	money.AttributionJudged(context.Background(), networks.AttributionVerdict{
		Network: network, FirstClickAt: time.Now().Add(-time.Hour), Unattributed: 5,
	})
	// The first credit lands and the canary retires.
	money.AttributionJudged(context.Background(), networks.AttributionVerdict{
		Network: network, FirstClickAt: time.Now().Add(-time.Hour), Attributed: 1, Unattributed: 5,
	})

	got := canaryStates(t, scrape(t, p))
	if got[networks.AttributionSuspect] != "0" {
		t.Errorf("the canary still reads suspect after a credit landed: %v", got)
	}
	if got[networks.AttributionRetired] != "1" {
		t.Errorf("the canary does not read retired after a credit landed: %v", got)
	}
}

// canaryStates reads the state gauge out of a Prometheus exposition.
func canaryStates(t *testing.T, body string) map[string]string {
	t.Helper()
	states := make(map[string]string)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "apivo_cashback_attribution_canary{") {
			continue
		}
		value := line[strings.LastIndex(line, " ")+1:]
		for _, known := range []string{
			networks.AttributionIdle, networks.AttributionRetired,
			networks.AttributionSuspect, networks.AttributionWatching,
		} {
			if strings.Contains(line, `state="`+known+`"`) {
				states[known] = value
			}
		}
	}
	return states
}
