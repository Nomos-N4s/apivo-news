// The tests for deeplink.go: contract rule 5 in both directions - a reference
// that survives the round trip through the network's own parameter, and every
// refusal that is better than a URL which redirects and loses the money.

package fixture

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// fixtureTestIssuedRef mints the reference a redirect would have been issued
// with, which cannot fail for the recorded value and is checked separately in
// fixture_test.go.
func fixtureTestIssuedRef(t *testing.T) networks.IssuedClickRef {
	t.Helper()
	ref, err := networks.NewIssuedClickRef(RecordedClickRef)
	if err != nil {
		t.Fatalf("NewIssuedClickRef(%q): %v", RecordedClickRef, err)
	}
	return ref
}

// TestBuildDeeplinkRoundTripsTheIssuedClickRef is contract rule 5: the
// reference Apivo minted comes back out of the network's own parameter
// unchanged. A redirect that dropped or mangled it still reaches the
// retailer, so the member buys and is never credited - the failure that costs
// them real money while looking exactly like success.
func TestBuildDeeplinkRoundTripsTheIssuedClickRef(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t)
	target := adapter.DeeplinkTarget(uuid.New())
	ref := fixtureTestIssuedRef(t)

	got, err := adapter.BuildDeeplink(t.Context(), target, ref)
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("BuildDeeplink() returned %q, which is not a URL: %v", got, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		t.Errorf("BuildDeeplink() returned %q, which is not an absolute URL fit for a Location header", got)
	}
	if back := parsed.Query().Get(target.ClickRefParam); back != ref.Ref() {
		t.Fatalf("the %q parameter came back as %q, want %q", target.ClickRefParam, back, ref.Ref())
	}
	if keep := parsed.Query().Get("merchant"); keep != "42" {
		t.Errorf("the template's own merchant parameter came back as %q, want %q", keep, "42")
	}
}

// TestBuildDeeplinkPreservesTheTemplateVerbatim refuses the re-encoding
// url.Values.Encode would do. It reorders parameters and normalises their
// escaping, which is a change to a value an operator was told this system
// would pass through - made to data some networks match on byte for byte.
func TestBuildDeeplinkPreservesTheTemplateVerbatim(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t)
	target := adapter.DeeplinkTarget(uuid.New())
	target.Template = "https://track.fixture.invalid/c/9f3?z=1&a=b%2Fc&m=42"

	got, err := adapter.BuildDeeplink(t.Context(), target, fixtureTestIssuedRef(t))
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}
	if !strings.Contains(got, "?z=1&a=b%2Fc&m=42&") {
		t.Errorf("BuildDeeplink() returned %q; the template's own query was rewritten rather than appended to", got)
	}
}

// TestBuildDeeplinkRefusesWhatCannotDescribeARedirect is contract rule 5's
// other half. Each of these produces a URL that works and loses the money, or
// one that should never have been issued at all, and every refusal wraps both
// sentinels: the first is what a caller deciding "do not redirect this
// member" tests, the second says the refusal is deterministic and no network
// is unwell - so the on-call is not paged towards one that is working.
func TestBuildDeeplinkRefusesWhatCannotDescribeARedirect(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t)
	offerID := uuid.New()
	tests := []struct {
		name   string
		mutate func(*networks.DeeplinkTarget)
		ref    func(*testing.T) networks.IssuedClickRef
	}{
		{
			name:   "published on another network",
			mutate: func(target *networks.DeeplinkTarget) { target.NetworkID = networks.NetworkID("awin") },
		},
		{
			name:   "no click-reference parameter",
			mutate: func(target *networks.DeeplinkTarget) { target.ClickRefParam = "  " },
		},
		{
			name:   "a template that is not absolute",
			mutate: func(target *networks.DeeplinkTarget) { target.Template = "/c/9f3?merchant=42" },
		},
		{
			name:   "a template with a scheme a browser must never be handed",
			mutate: func(target *networks.DeeplinkTarget) { target.Template = "javascript:alert(1)" },
		},
		{
			name:   "no template at all",
			mutate: func(target *networks.DeeplinkTarget) { target.Template = "" },
		},
		{
			name:   "a reference that was never minted",
			mutate: func(*networks.DeeplinkTarget) {},
			ref:    func(*testing.T) networks.IssuedClickRef { return networks.IssuedClickRef{} },
		},
		{
			name: "a template that already sets the click-reference parameter",
			mutate: func(target *networks.DeeplinkTarget) {
				target.Template = "https://track.fixture.invalid/c/9f3?clickref=someone-elses"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := adapter.DeeplinkTarget(offerID)
			tc.mutate(&target)
			ref := fixtureTestIssuedRef(t)
			if tc.ref != nil {
				ref = tc.ref(t)
			}
			got, err := adapter.BuildDeeplink(t.Context(), target, ref)
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink() error = %v, want one wrapping ErrDeeplinkNotFormed", err)
			}
			if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
				t.Errorf("BuildDeeplink() error = %v, want one also wrapping ErrDeeplinkInputsRefused: this refusal is deterministic, and a caller must not page an on-call towards a healthy network for it", err)
			}
			if got != "" {
				t.Errorf("BuildDeeplink() returned %q beside its refusal; a partially-formed URL still redirects", got)
			}
		})
	}
}

// TestBuildDeeplinkReportsAnInjectedNetworkFailure holds the other half of
// the two-sentinel split. A network that could not be reached is refused with
// contract rule 9's classification rather than with ErrDeeplinkInputsRefused,
// because that difference decides whether the offer is one to stop
// publishing or the network is merely unwell.
func TestBuildDeeplinkReportsAnInjectedNetworkFailure(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithFailure(FailureRefused, FailureAlways))
	got, err := adapter.BuildDeeplink(t.Context(), adapter.DeeplinkTarget(uuid.New()), fixtureTestIssuedRef(t))
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Fatalf("BuildDeeplink() error = %v, want one wrapping ErrDeeplinkNotFormed", err)
	}
	if !errors.Is(err, networks.ErrNetworkRefused) {
		t.Errorf("BuildDeeplink() error = %v, want one wrapping ErrNetworkRefused", err)
	}
	if errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Errorf("BuildDeeplink() error = %v also claims the inputs were refused; the inputs were fine and the network was not", err)
	}
	if got != "" {
		t.Errorf("BuildDeeplink() returned %q beside its refusal", got)
	}
}

// TestBuildDeeplinkRefusesACancelledContext keeps a redirect from being
// handed to a caller whose request has already gone away, and reports it as
// neither our routing bug nor the network's fault.
func TestBuildDeeplinkRefusesACancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	adapter := fixtureTestAdapter(t)
	_, err := adapter.BuildDeeplink(ctx, adapter.DeeplinkTarget(uuid.New()), fixtureTestIssuedRef(t))
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildDeeplink() error = %v, want one wrapping both ErrDeeplinkNotFormed and context.Canceled", err)
	}
}

// TestBuildDeeplinkChecksTheInputsBeforeTheNetwork pins the order the method
// does its work in. A deterministic refusal must not be reported as a network
// failure just because a failure happened to be injected, or an offer nobody
// can ever redirect for would be retried forever as a network having a bad
// day.
func TestBuildDeeplinkChecksTheInputsBeforeTheNetwork(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithFailure(FailureUnavailable, FailureAlways))
	target := adapter.DeeplinkTarget(uuid.New())
	target.Template = "/c/9f3"

	_, err := adapter.BuildDeeplink(t.Context(), target, fixtureTestIssuedRef(t))
	if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Fatalf("BuildDeeplink() error = %v, want one wrapping ErrDeeplinkInputsRefused", err)
	}
	if errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Errorf("BuildDeeplink() blamed the network for a template it could never have used: %v", err)
	}
	if kind, _ := adapter.failures.peek(); kind != FailureUnavailable {
		t.Errorf("the injected failure was consumed by a call that never reached the network; it now reads as %s", kind)
	}
}
