// A measurement vocabulary of our own (ADR-0007).
//
// ADR-0007 says nothing above this package names OpenTelemetry, and until now
// that cost nothing: a caller passed a context and read a *Provider. A metric
// is where it starts to cost something, because a caller has to say what it is
// measuring, and the obvious way to let it is to hand out a metric.Int64Counter
// and let the vendor's type travel.
//
// So the vocabulary is ours instead: Counter, Gauge, Histogram and Attr, four
// types with no import of anything outside the standard library in their
// signatures. A consuming package declares the one-method interface it needs -
// the way internal/platform/http declares Instrumentation - and one of these
// satisfies it. The backend stays a deployment decision.
//
// ATTRIBUTE VALUES ARE STRINGS, deliberately and only. An attribute whose
// value can be anything is a metric whose cardinality can be anything, and a
// metrics store dies of cardinality long before it dies of volume. A string is
// not a guarantee - "member id" is a string - but it makes the dangerous case
// something a reader can see in the call rather than infer from a type.

package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

// Attr is one label on a measurement.
type Attr struct {
	Key   string
	Value string
}

// Label is a shorthand for the common case.
func Label(key, value string) Attr { return Attr{Key: key, Value: value} }

// Counter is a measurement that only goes up: things that happened.
type Counter interface {
	Add(ctx context.Context, n int64, attrs ...Attr)
}

// Gauge is a measurement that is whatever it is when it is taken: a backlog, a
// balance, a verdict. Recorded when the value is known rather than polled, so
// nothing here reads a database on a scrape.
type Gauge interface {
	Record(ctx context.Context, v int64, attrs ...Attr)
}

// Histogram records a distribution. Durations, principally, in seconds -
// OpenTelemetry's own unit for them, so a backend's default buckets apply.
type Histogram interface {
	Record(ctx context.Context, v float64, attrs ...Attr)
}

// Meter builds the instruments for one instrumentation scope.
//
// A scope is the import path of the package doing the measuring, which is what
// lets a backend say where a number came from. Obtain one from
// [Provider.Meter]; the zero value is not usable.
type Meter struct {
	inner metric.Meter
}

// Meter answers the instrument builder for scope, which should be the
// measuring package's import path.
//
// It is never nil and never fails: when telemetry is off the instruments are
// OpenTelemetry's own no-ops, so a caller builds and records exactly as it
// would otherwise and no code path exists that only runs in production.
func (p *Provider) Meter(scope string) *Meter {
	if p == nil || p.meter == nil {
		return &Meter{inner: metricnoop.NewMeterProvider().Meter(scope)}
	}
	return &Meter{inner: p.meter.Meter(scope)}
}

// Counter builds a counter. unit is a UCUM code, or "" where the thing counted
// has no unit but itself - which is the usual case, and is why it is not "1".
func (m *Meter) Counter(name, description, unit string) (Counter, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	c, err := m.inner.Int64Counter(name,
		metric.WithDescription(description), metric.WithUnit(unit))
	if err != nil {
		return nil, err
	}
	return counter{c}, nil
}

// Gauge builds a gauge.
func (m *Meter) Gauge(name, description, unit string) (Gauge, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	g, err := m.inner.Int64Gauge(name,
		metric.WithDescription(description), metric.WithUnit(unit))
	if err != nil {
		return nil, err
	}
	return gauge{g}, nil
}

// Histogram builds a histogram. Seconds, for a duration.
func (m *Meter) Histogram(name, description, unit string) (Histogram, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	h, err := m.inner.Float64Histogram(name,
		metric.WithDescription(description), metric.WithUnit(unit))
	if err != nil {
		return nil, err
	}
	return histogram{h}, nil
}

// maxNameLen is OpenTelemetry's limit on an instrument name.
const maxNameLen = 255

// validName applies OpenTelemetry's instrument-name rule HERE, before the SDK
// sees it, so that a bad name is refused identically whether or not this
// deployment configured a collector.
//
// The no-op meter accepts anything - it has nothing to register a name
// against. Without this, an instrument named by a copy-paste would build
// cleanly in every test in this repository, all of which run with telemetry
// off, and fail for the first time on the first deployment that has it on.
// That is the worst available place to learn it.
func validName(name string) error {
	if name == "" {
		return errors.New("telemetry: an instrument needs a name")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("telemetry: the instrument name %q is longer than %d characters", name, maxNameLen)
	}
	if !isLetter(rune(name[0])) {
		return fmt.Errorf("telemetry: the instrument name %q must begin with a letter", name)
	}
	for _, r := range name[1:] {
		if isLetter(r) || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' || r == '/' {
			continue
		}
		return fmt.Errorf("telemetry: the instrument name %q may not contain %q", name, r)
	}
	return nil
}

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

type counter struct{ inner metric.Int64Counter }

func (c counter) Add(ctx context.Context, n int64, attrs ...Attr) {
	c.inner.Add(ctx, n, metric.WithAttributes(keyValues(attrs)...))
}

type gauge struct{ inner metric.Int64Gauge }

func (g gauge) Record(ctx context.Context, v int64, attrs ...Attr) {
	g.inner.Record(ctx, v, metric.WithAttributes(keyValues(attrs)...))
}

type histogram struct{ inner metric.Float64Histogram }

func (h histogram) Record(ctx context.Context, v float64, attrs ...Attr) {
	h.inner.Record(ctx, v, metric.WithAttributes(keyValues(attrs)...))
}

// keyValues converts our labels to the vendor's, which is the whole of the
// coupling and is confined to this file.
func keyValues(attrs []Attr) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, len(attrs))
	for i, a := range attrs {
		out[i] = attribute.String(a.Key, a.Value)
	}
	return out
}
