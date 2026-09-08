package telemetry

// Invariant 1 on the wire out (ADR-0007).
//
// These cases are the reason this package exists. An endpoint test can prove
// what a handler renders; only this can prove what leaves the process when
// nobody wrote the attribute - which is the case that matters, because
// auto-instrumentation attaches URLs, SQL and headers without being asked.
//
// The suite is deliberately written against the EXPORTER rather than against
// scrub(): a test of the pure function would pass while a wiring change sent
// spans around it.

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// exported runs one span carrying attrs through the scrubbing exporter and
// answers what the collector would have received.
func exported(t *testing.T, attrs ...attribute.KeyValue) map[string]string {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(scrubbingExporter{inner: sink}))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	_, span := provider.Tracer("test").Start(context.Background(), "op")
	span.SetAttributes(attrs...)
	span.End()

	stubs := sink.GetSpans()
	if len(stubs) != 1 {
		t.Fatalf("exported %d spans, want 1", len(stubs))
	}
	got := map[string]string{}
	for _, attr := range stubs[0].Attributes {
		got[string(attr.Key)] = attr.Value.String()
	}
	return got
}

// TestASecretNamedAttributeIsRedacted. The same predicate the log handler
// uses, called rather than copied - so a marker added to the logging package
// covers spans on the same commit.
func TestASecretNamedAttributeIsRedacted(t *testing.T) {
	t.Parallel()
	// One per marker family, spelled the several ways a caller might.
	secrets := []string{
		"api_key", "apiKey", "API-KEY", "network.api.secret",
		"authorization", "db.password", "connection_string",
		"database_url", "dsn", "private_key", "session_key",
		"bearer_token", "credential_ref_value", "access_key",
	}
	for _, key := range secrets {
		got := exported(t, attribute.String(key, "the-actual-value"))
		if got[key] != Redacted {
			t.Errorf("%q exported as %q, want %q", key, got[key], Redacted)
		}
	}
}

// TestTheDangerousConventionsAreSuppressedWhateverTheyHold. These keys do not
// name a secret; their VALUE is unsafe by construction, and no inspection of
// it could be trusted to notice.
func TestTheDangerousConventionsAreSuppressedWhateverTheyHold(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"db.statement":                      "insert into cashback.payout_destination (details) values ('DE89370400440532013000')",
		"db.query.text":                     "select * from account where email = 'member@example.test'",
		"url.full":                          "https://go.linkwi.se/x?subid1=abc&token=live_secret",
		"url.query":                         "token=live_secret",
		"http.url":                          "https://api.example.test/v1?key=live_secret",
		"http.target":                       "/api/v1/cashback/wallet?token=live_secret",
		"http.request.header.authorization": "Bearer eyJhbGciOi",
		"http.request.header.cookie":        "sb-access-token=eyJhbGciOi",
		"http.response.header.set-cookie":   "sb-refresh-token=eyJhbGciOi",
		"http.request.header.x-api-key":     "live_secret",
	}
	for key, value := range cases {
		got := exported(t, attribute.String(key, value))
		if got[key] != Redacted {
			t.Errorf("%q exported as %q, want %q", key, got[key], Redacted)
		}
	}
}

// TestNoSecretSurvivesAnywhereInTheExportedSpan. The assertions above check
// the key they were given. This checks the whole exported payload for the
// values themselves, which is what catches a redaction that replaced the
// wrong element of the slice.
func TestNoSecretSurvivesAnywhereInTheExportedSpan(t *testing.T) {
	t.Parallel()
	const iban = "DE89370400440532013000"
	const token = "eyJhbGciOiJIUzI1NiJ9.secret"

	got := exported(t,
		attribute.String("http.route", "/api/v1/cashback/wallet"),
		attribute.String("db.statement", "insert into vault values ('"+iban+"')"),
		attribute.String("authorization", "Bearer "+token),
		attribute.Int("http.status_code", 200),
	)

	var whole strings.Builder
	for key, value := range got {
		whole.WriteString(key)
		whole.WriteString("=")
		whole.WriteString(value)
		whole.WriteString(" ")
	}
	for _, forbidden := range []string{iban, token, "Bearer"} {
		if strings.Contains(whole.String(), forbidden) {
			t.Errorf("the exported span carries %q: %s", forbidden, whole.String())
		}
	}
}

// TestTheSafeAttributesAreUntouched. A scrubber that redacted everything
// would satisfy every test above and be useless. This is the other half.
func TestTheSafeAttributesAreUntouched(t *testing.T) {
	t.Parallel()
	got := exported(t,
		attribute.String("http.route", "/api/v1/cashback/ops/withdrawals"),
		attribute.String("http.request.method", "GET"),
		attribute.Int("http.response.status_code", 200),
		attribute.String("network.id", "linkwise"),
		attribute.String("url.path", "/api/v1/cashback/wallet"),
	)
	want := map[string]string{
		"http.route":                "/api/v1/cashback/ops/withdrawals",
		"http.request.method":       "GET",
		"http.response.status_code": "200",
		"network.id":                "linkwise",
		"url.path":                  "/api/v1/cashback/wallet",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%q exported as %q, want %q — a scrubber that redacts everything is not a scrubber", key, got[key], value)
		}
	}
}

// TestScrubDoesNotCopyWhenNothingIsDangerous. Not a micro-optimisation for
// its own sake: this runs on every span of every request, and the common case
// is a span with nothing to redact. Asserted because the lazy copy is easy to
// lose in a later edit and nothing else would notice.
func TestScrubDoesNotCopyWhenNothingIsDangerous(t *testing.T) {
	t.Parallel()
	attrs := []attribute.KeyValue{
		attribute.String("http.route", "/x"),
		attribute.Int("http.response.status_code", 200),
	}
	if got := scrub(attrs); &got[0] != &attrs[0] {
		t.Error("scrub copied a slice it had no reason to touch")
	}
}
