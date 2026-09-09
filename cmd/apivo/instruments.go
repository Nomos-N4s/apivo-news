package main

// Building a list of instruments as a list (ADR-0007).
//
// An instrument constructor returns an error, and it is a real one: a name
// this binary got wrong is a bug that should stop the process rather than
// produce a metric nobody can find. But ten instruments built one at a time
// is thirty lines of `if err != nil { return nil, err }` around ten lines of
// declaration, and the declarations are the part a reader has come for.
//
// So the error is collected. The first one wins and the rest are still built,
// because the point is to name the mistake, not to name it first.

import "github.com/Nomos-N4s/apivo-news/internal/platform/telemetry"

// instrumentBuilder builds instruments and remembers the first failure.
type instrumentBuilder struct {
	meter *telemetry.Meter
	err   error
}

// instruments starts a builder over the named instrumentation scope.
func instruments(provider *telemetry.Provider, scope string) *instrumentBuilder {
	return &instrumentBuilder{meter: provider.Meter(scope)}
}

// counter builds a counter, or notes why it could not.
//
// No unit: nothing this binary counts has one but itself. telemetry.Meter
// takes one because it is the general vocabulary; this builder is the local
// convenience, and a parameter every call passes "" to is one more thing to
// read past.
func (b *instrumentBuilder) counter(name, description string) telemetry.Counter {
	c, err := b.meter.Counter(name, description, "")
	b.note(err)
	return c
}

// gauge builds a gauge, unitless for the reason counter is. The ledger delta
// is in minor currency units, for which UCUM has no code and this repository
// uses none.
func (b *instrumentBuilder) gauge(name, description string) telemetry.Gauge {
	g, err := b.meter.Gauge(name, description, "")
	b.note(err)
	return g
}

// histogram builds a histogram, or notes why it could not.
func (b *instrumentBuilder) histogram(name, description, unit string) telemetry.Histogram {
	h, err := b.meter.Histogram(name, description, unit)
	b.note(err)
	return h
}

// note keeps the first failure.
func (b *instrumentBuilder) note(err error) {
	if err != nil && b.err == nil {
		b.err = err
	}
}
