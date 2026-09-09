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

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"
)

// moneyScope names the instrumentation scope for the cashback invariants.
const moneyScope = "github.com/Nomos-N4s/apivo-news/internal/cashback"

// moneyInstruments is what the invariants report through.
//
// Every method tolerates a nil receiver, so a caller that was handed no
// instruments - a wiring test, principally - reports into nothing rather than
// growing a guard of its own at each of the four seams.
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
	// credits counts what each lifecycle run did, by the outcome each item
	// reached. This is the earning funnel: opened pending, held by a rule,
	// or queued because the report matched no click.
	credits telemetry.Counter
	// differences counts reconciliation differences as they are recorded -
	// what the network says it paid against what this system says it
	// earned.
	//
	// UNLABELLED, and that is a limitation rather than a choice. Detection
	// reports how many rows were new as a count, not as a list, so which
	// KINDS were new cannot be told from outside the ops package: labelling
	// this by the kinds in Found would count a re-import of the same
	// statement as fresh discrepancies every time. The kind breakdown is a
	// question for the table, and belongs with the backlog gauges.
	differences telemetry.Counter
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
	credits, err := meter.Counter("apivo.cashback.credits",
		"What each earnings lifecycle pass did, by the outcome each item reached.", "")
	if err != nil {
		return nil, err
	}
	differences, err := meter.Counter("apivo.cashback.reconciliation.differences",
		"Reconciliation differences newly recorded by a detection pass.", "")
	if err != nil {
		return nil, err
	}
	return &moneyInstruments{
		ledgerNet:        ledgerNet,
		ledgerCurrencies: ledgerCurrencies,
		deadLetters:      deadLetters,
		credits:          credits,
		differences:      differences,
	}, nil
}

// LifecycleRan records what one earnings pass did. It satisfies
// earnings.LifecycleObserver.
//
// Every field, including the zeroes, and one instrument with an outcome label
// rather than seven instruments: these are the same measurement taken seven
// ways, and a dashboard asking "where do credits stop" wants them on one
// axis. Failed is here too - an item the pass could not act on is read again
// next run, and a Failed that never falls is a queue that is not draining.
func (m *moneyInstruments) LifecycleRan(ctx context.Context, out earnings.Outcome) {
	if m == nil {
		return
	}
	for outcome, n := range map[string]int{
		"credited":  out.Credited,
		"held":      out.Held,
		"queued":    out.Queued,
		"confirmed": out.Confirmed,
		"awaiting":  out.Awaiting,
		"reversed":  out.Reversed,
		"failed":    out.Failed,
	} {
		if n == 0 {
			// A counter is a rate, not a level: adding nothing and adding
			// zero are the same fact, and the series exists as soon as the
			// first non-zero pass records it.
			continue
		}
		m.credits.Add(ctx, int64(n), telemetry.Label("outcome", outcome))
	}
}

// differencesFound records what one detection pass recorded.
//
// Recorded, never len(Found): Found includes every difference this pass
// derived, and a re-import of the same statement derives all of them again
// while writing none. Counting Found would turn a retried import into a fresh
// discrepancy each time - which is the number somebody would then alert on.
func (m *moneyInstruments) differencesFound(ctx context.Context, detection ops.Detection) {
	if m == nil {
		return
	}
	if detection.Recorded == 0 {
		return
	}
	m.differences.Add(ctx, int64(detection.Recorded))
}

// countedReconciliation wraps the operator module's reconciliation store so a
// detection is counted where it happens.
//
// A decorator rather than a twelfth parameter on ops.NewHandler, and rather
// than a seam inside the ops package: the module already exposes the
// behaviour as an interface, the composition root is where an adapter belongs,
// and networkInspector next door does exactly this.
type countedReconciliation struct {
	ops.ReconciliationStore
	money *moneyInstruments
}

// DetectDifferences delegates, then counts what was recorded.
//
// A failed detection counts nothing. The handler logs it and answers 500, and
// a number that moved on a pass which did not complete would say a discrepancy
// was found where none was read.
func (c countedReconciliation) DetectDifferences(ctx context.Context, run uuid.UUID) (ops.Detection, error) {
	detection, err := c.ReconciliationStore.DetectDifferences(ctx, run)
	if err != nil {
		return detection, err
	}
	c.money.differencesFound(ctx, detection)
	return detection, nil
}

// LedgerSummed records what one C-1 pass found. It satisfies
// wallet.LedgerObserver.
func (m *moneyInstruments) LedgerSummed(ctx context.Context, sum wallet.LedgerSum) {
	if m == nil {
		return
	}
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
	if m == nil {
		return
	}
	m.deadLetters.Add(ctx, 1,
		telemetry.Label("subscriber", parked.Subscriber),
		telemetry.Label("type", parked.Event.Type))
}
