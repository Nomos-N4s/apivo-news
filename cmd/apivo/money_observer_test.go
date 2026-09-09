package main

// What the money invariants look like in a scrape (ADR-0007).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

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
