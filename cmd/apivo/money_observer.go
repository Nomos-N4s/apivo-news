package main

// The money invariants, as numbers something can alert on (ADR-0007).
//
// Every one of these was already being computed. The C-1 check summed the
// ledger and logged it; the subscriber registry parked a delivery and logged
// it. Each DETECTS, and until this file none of them TOLD ANYBODY - which is
// the gap ADR-0007's second question names, and the reason a log line that
// scrolled away is how an incident becomes a surprise.
//
// One meter, built once at start-up, because an instrument is registered
// against a name and building two under one name is the way a metric silently
// stops adding up.

import (
	"context"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"
)

// moneyScope names the instrumentation scope for the cashback invariants.
const moneyScope = "github.com/Nomos-N4s/apivo-news/internal/cashback"

// moneyInstruments is what the invariants report through.
type moneyInstruments struct {
	// ledgerNet is the signed minor-unit delta per currency. Zero is the
	// only correct value and the one C-1 exists to assert.
	ledgerNet telemetry.Gauge
	// ledgerCurrencies is how many currencies the last pass actually summed.
	// It exists because zero and "all clean" are different facts: a check
	// that summed nothing is vacuously clean, and an alert that cannot tell
	// the two apart is an alert that goes quiet exactly when the ledger
	// stops being visible.
	ledgerCurrencies telemetry.Gauge
	// deadLetters counts deliveries parked for an operator. A parked
	// delivery is money or consent nobody acted on, and it blocks its lane
	// until somebody requeues it.
	deadLetters telemetry.Counter
}

// newMoneyInstruments builds them, or fails on a name this binary got wrong.
func newMoneyInstruments(provider *telemetry.Provider) (*moneyInstruments, error) {
	meter := provider.Meter(moneyScope)
	ledgerNet, err := meter.Gauge("apivo.cashback.ledger.net",
		"How far the ledger is from netting to zero in one currency (C-1). Anything but zero is an incident.", "")
	if err != nil {
		return nil, err
	}
	ledgerCurrencies, err := meter.Gauge("apivo.cashback.ledger.currencies_checked",
		"How many currencies the last C-1 pass summed. Zero means no ledger was visible to sum, which is not the same as clean.", "")
	if err != nil {
		return nil, err
	}
	deadLetters, err := meter.Counter("apivo.events.dead_letters",
		"Event deliveries parked for an operator, by subscriber and event type.", "")
	if err != nil {
		return nil, err
	}
	return &moneyInstruments{
		ledgerNet:        ledgerNet,
		ledgerCurrencies: ledgerCurrencies,
		deadLetters:      deadLetters,
	}, nil
}

// LedgerSummed records what one C-1 pass found. It satisfies
// wallet.LedgerObserver.
func (m *moneyInstruments) LedgerSummed(ctx context.Context, sum wallet.LedgerSum) {
	m.ledgerCurrencies.Record(ctx, int64(sum.Currencies))
	for currency, net := range sum.Net {
		m.ledgerNet.Record(ctx, net, telemetry.Label("currency", currency))
	}
}

// deadLettered records one parked delivery.
//
// The label is the event TYPE and the subscriber, never the event id or its
// subject: those are unbounded, and an unbounded label is how a metrics store
// dies. Which row to look at is in the log line beside this call, which is the
// right place for an identifier nobody aggregates over.
func (m *moneyInstruments) deadLettered(ctx context.Context, parked events.DeadLetter) {
	m.deadLetters.Add(ctx, 1,
		telemetry.Label("subscriber", parked.Subscriber),
		telemetry.Label("type", parked.Event.Type))
}
