// Keeping secrets out of spans (ADR-0007, Invariant 1).
//
// internal/platform/logging redacts attributes whose key names a secret. It
// redacts LOGS, and until this package existed a log line was the only way a
// value left this process in a form somebody else stored.
//
// A span attribute is a second way, and it is worse in one respect: nobody
// writes it. Auto-instrumentation attaches request URLs, SQL text and headers
// on its own, and in this system those carry member account ids, IBAN-shaped
// strings on their way to the vault (ADR-0006), and bearer tokens. ADR-0003
// keeps a network credential out of the database and out of the repository;
// letting one out through a trace instead would be the same leak by a
// different door.
//
// So every span is scrubbed on the way to the exporter. Two rules:
//
//  1. An attribute whose KEY names a secret is redacted, using
//     [logging.IsSecretKey] - the same predicate the log handler uses, called
//     rather than copied, so the two cannot drift apart.
//  2. An attribute on the suppressed list is redacted whatever its key looks
//     like, because its VALUE is dangerous by construction: SQL text, a URL
//     with a query string, a captured header.
//
// It runs at the exporter rather than at the span processor because a span
// reaching OnEnd is already read-only. Wrapping the exporter is the last
// point at which anything can still be changed, which is also the right
// place for a defence that must not be bypassable by instrumentation added
// later.

package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
)

// Redacted is written in place of a scrubbed value. It is the logging
// package's marker, so a reader who has seen one has seen both.
const Redacted = logging.Redacted

// suppressedKeys name attributes whose value cannot be made safe by
// inspecting it, so the key alone is enough to refuse.
//
// These are OpenTelemetry's own semantic conventions, and both the current
// and the superseded spellings are listed: an instrumentation library
// pinned to an older convention would otherwise slip past a list written
// only for the new one.
var suppressedKeys = map[string]struct{}{
	// SQL. This deployment's statements carry a member's payout details on
	// their way to the vault reference, and bind parameters are rendered
	// into the text by some drivers.
	"db.statement":  {},
	"db.query.text": {},
	// URLs with their query string attached. A click-out's redirect carries
	// a click reference, an auth callback carries a code, and neither is
	// something to keep for days in a trace store.
	"url.full":    {},
	"url.query":   {},
	"http.url":    {},
	"http.target": {},
}

// suppressedPrefixes name whole families, where the dangerous part is the
// suffix an instrumentation library chooses at run time.
var suppressedPrefixes = []string{
	// Captured headers. Authorization is the obvious one; Cookie and any
	// X-Api-Key a future edge sets are the ones that would be forgotten.
	"http.request.header.",
	"http.response.header.",
}

// scrub returns attributes safe to export, redacting rather than dropping.
//
// Redacting rather than dropping is deliberate: a reader has to be able to
// tell "this was withheld" from "this was never set", which is the same
// reason [logging.Redacted] is not the empty string.
func scrub(attrs []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for i, attr := range attrs {
		if !dangerous(string(attr.Key)) {
			continue
		}
		// Copy lazily: the overwhelming majority of spans carry nothing
		// that needs touching, and a per-span allocation for those would be
		// a cost paid on every request for a case that does not arise.
		if out == nil {
			out = make([]attribute.KeyValue, len(attrs))
			copy(out, attrs)
		}
		out[i] = attribute.String(string(attr.Key), Redacted)
	}
	if out == nil {
		return attrs
	}
	return out
}

// dangerous reports whether an attribute key must have its value withheld.
func dangerous(key string) bool {
	if _, ok := suppressedKeys[key]; ok {
		return true
	}
	for _, prefix := range suppressedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return logging.IsSecretKey(key)
}

// scrubbingExporter wraps an exporter and scrubs every span through it.
type scrubbingExporter struct {
	inner sdktrace.SpanExporter
}

// ExportSpans scrubs, then delegates.
func (e scrubbingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	scrubbed := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		scrubbed[i] = scrubbedSpan{ReadOnlySpan: span, attrs: scrub(span.Attributes())}
	}
	return e.inner.ExportSpans(ctx, scrubbed)
}

// Shutdown delegates.
func (e scrubbingExporter) Shutdown(ctx context.Context) error { return e.inner.Shutdown(ctx) }

// scrubbedSpan is one span with its attributes replaced.
//
// It embeds the interface rather than a concrete type, so every method this
// package does not override keeps working when the SDK adds one - and the
// SDK does add them. Overriding by embedding is what keeps this wrapper from
// becoming a maintenance burden on every upgrade.
type scrubbedSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

// Attributes answers the scrubbed set.
func (s scrubbedSpan) Attributes() []attribute.KeyValue { return s.attrs }
