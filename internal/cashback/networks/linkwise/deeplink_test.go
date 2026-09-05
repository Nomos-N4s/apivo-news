package linkwise_test

// Assembling the redirect a member is sent through (T246, contract rule 5).
//
// Every case here is about a URL that WORKS and loses the money. A refusal
// that is too strict costs one offer, loudly, with the offer named; a
// redirect that is too permissive costs every member who used it, silently,
// and is discovered a month later when the transactions never arrive.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
)

// theRef is a click reference of the shape IssuedClickRef mints: at least 22
// URL-safe characters.
const theRef = "0mB7hQ2xKp4vT9sLcNfR1w"

// aTarget is a well-formed route on this network, which each case then
// breaks in exactly one way.
func aTarget() networks.DeeplinkTarget {
	return networks.DeeplinkTarget{
		OfferID:       uuid.MustParse("6f1d2c3b-4a59-4e87-9c10-2b7e5d8a3f64"),
		NetworkID:     linkwise.ID,
		ClickRefParam: "subid1",
		Template:      "https://go.linkwi.se/z/5-12/CD20/?lnkurl=https%3A%2F%2Fshop.example%2Fx",
	}
}

func anIssuedRef(t *testing.T) networks.IssuedClickRef {
	t.Helper()
	ref, err := networks.NewIssuedClickRef(theRef)
	if err != nil {
		t.Fatalf("NewIssuedClickRef(): %v", err)
	}
	return ref
}

// aDeeplinkClient builds a client with no transport: BuildDeeplink never
// contacts Linkwise, which is the whole reason it is assembled locally.
func aDeeplinkClient(t *testing.T) *linkwise.Client {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), linkwise.ID, "CD20")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	client, err := linkwise.New(account, linkwise.WithCredential(theUsername, thePassword))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client
}

// TestTheReferenceSurvivesTheRoundTrip. IssuedClickRef is at least 22
// URL-safe characters and it has to arrive at the network byte for byte: a
// reference that was escaped, truncated or re-cased matches no click, and an
// unattributed transaction is a member who bought and was never credited.
func TestTheReferenceSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	got, err := aDeeplinkClient(t).BuildDeeplink(t.Context(), aTarget(), anIssuedRef(t))
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("the deeplink is not a URL: %v", err)
	}
	if carried := parsed.Query().Get("subid1"); carried != theRef {
		t.Errorf("subid1 = %q, want %q byte for byte", carried, theRef)
	}
	// And the destination the operator wrote is still there, unchanged.
	if got, want := parsed.Query().Get("lnkurl"), "https://shop.example/x"; got != want {
		t.Errorf("lnkurl = %q, want %q; the template's own parameters must survive", got, want)
	}
	if parsed.Scheme != "https" || parsed.Host != "go.linkwi.se" {
		t.Errorf("the deeplink points at %s://%s, not where the template did", parsed.Scheme, parsed.Host)
	}
}

// TestAnySlotIsAccepted. Five slots is a budget rather than a field name, and
// which one this deployment uses is the network row's business - the column
// exists so an operator can change it without a release.
func TestAnySlotIsAccepted(t *testing.T) {
	t.Parallel()

	client := aDeeplinkClient(t)
	for _, slot := range []string{"subid1", "subid2", "subid3", "subid4", "subid5"} {
		t.Run(slot, func(t *testing.T) {
			t.Parallel()
			target := aTarget()
			target.ClickRefParam = slot
			got, err := client.BuildDeeplink(t.Context(), target, anIssuedRef(t))
			if err != nil {
				t.Fatalf("BuildDeeplink(%s): %v", slot, err)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("the deeplink is not a URL: %v", err)
			}
			if carried := parsed.Query().Get(slot); carried != theRef {
				t.Errorf("%s = %q, want the reference", slot, carried)
			}
		})
	}
}

// TestASlotLinkwiseDoesNotReadIsRefused is the refusal that earns its keep.
//
// click_ref_param is an editable column and the value it most plausibly
// acquires by mistake is another network's. Set Awin's "clickref" here and
// every redirect still works, every member still reaches the retailer, and
// Linkwise reports every transaction with all five sub-ids null. Nothing
// fails.
func TestASlotLinkwiseDoesNotReadIsRefused(t *testing.T) {
	t.Parallel()

	client := aDeeplinkClient(t)
	for _, tt := range []struct{ name, param string }{
		{name: "Awin's", param: "clickref"},
		{name: "a sixth slot Linkwise's own script refuses", param: "subid6"},
		{name: "a zeroth slot", param: "subid0"},
		{name: "the prefix alone", param: "subid"},
		{name: "a negative slot", param: "subid-1"},
		{name: "padded", param: "subid01"},
		{name: "cased", param: "SubID1"},
		{name: "a plausible invention", param: "sub_id1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := aTarget()
			target.ClickRefParam = tt.param
			got, err := client.BuildDeeplink(t.Context(), target, anIssuedRef(t))
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink(%q) = %q, %v; want ErrDeeplinkNotFormed", tt.param, got, err)
			}
			if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
				t.Errorf("the refusal does not say it is deterministic, so the on-call is paged towards a network that is working: %v", err)
			}
			if got != "" {
				t.Errorf("a refused redirect still returned the URL %q", got)
			}
			// The set is closed and small, so the refusal names all of it:
			// the operator reading this has to type the right value next.
			for _, slot := range []string{"subid1", "subid5"} {
				if !strings.Contains(err.Error(), slot) {
					t.Errorf("the refusal does not name %q among the slots that work: %v", slot, err)
				}
			}
		})
	}
}

// TestAnUnescapedDestinationIsRefused.
//
// A Linkwise click URL nests the member's destination inside its own query.
// An unescaped one has already leaked its query string into the outer URL, so
// the click reference would be appended after a fragment of the retailer's
// URL - and which of the two Linkwise then reads is not this adapter's to
// decide.
func TestAnUnescapedDestinationIsRefused(t *testing.T) {
	t.Parallel()

	client := aDeeplinkClient(t)
	for _, tt := range []struct{ name, template string }{
		{
			name:     "a destination carrying its own query",
			template: "https://go.linkwi.se/z/5-12/CD20/?lnkurl=https://shop.example/x?colour=red",
		},
		{
			name:     "and one whose parameters then leaked out",
			template: "https://go.linkwi.se/z/5-12/CD20/?lnkurl=https://shop.example/x?a=1&b=2",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := aTarget()
			target.Template = tt.template
			got, err := client.BuildDeeplink(t.Context(), target, anIssuedRef(t))
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink() = %q, %v; want ErrDeeplinkNotFormed", got, err)
			}
			if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
				t.Errorf("the refusal does not say it is deterministic: %v", err)
			}
			if !strings.Contains(err.Error(), "6f1d2c3b") {
				t.Errorf("the refusal does not name the offer somebody has to go and fix: %v", err)
			}
		})
	}
}

// TestAnEscapedDestinationIsAccepted is the other side of that refusal: a
// correctly escaped destination carries %3F rather than a question mark, and
// must not be caught.
func TestAnEscapedDestinationIsAccepted(t *testing.T) {
	t.Parallel()

	target := aTarget()
	target.Template = "https://go.linkwi.se/z/5-12/CD20/?lnkurl=" +
		url.QueryEscape("https://shop.example/x?colour=red&size=40")
	got, err := aDeeplinkClient(t).BuildDeeplink(t.Context(), target, anIssuedRef(t))
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("the deeplink is not a URL: %v", err)
	}
	if want := "https://shop.example/x?colour=red&size=40"; parsed.Query().Get("lnkurl") != want {
		t.Errorf("lnkurl = %q, want %q", parsed.Query().Get("lnkurl"), want)
	}
	if parsed.Query().Get("subid1") != theRef {
		t.Errorf("subid1 = %q, want the reference", parsed.Query().Get("subid1"))
	}
}

// TestThePortsOwnRefusalsStillApply. ValidateDeeplinkInputs runs first, before
// a single character of URL exists, so there is no state in which a
// partially-formed URL could be returned by accident.
func TestThePortsOwnRefusalsStillApply(t *testing.T) {
	t.Parallel()

	client := aDeeplinkClient(t)
	for _, tt := range []struct {
		name   string
		mutate func(*networks.DeeplinkTarget)
	}{
		{
			name:   "a target published on another network",
			mutate: func(target *networks.DeeplinkTarget) { target.NetworkID = "awin" },
		},
		{
			name:   "a template that is not absolute",
			mutate: func(target *networks.DeeplinkTarget) { target.Template = "/z/5-12/CD20/" },
		},
		{
			name:   "a template with a scheme a browser would execute",
			mutate: func(target *networks.DeeplinkTarget) { target.Template = "javascript:alert(1)" },
		},
		{
			name:   "no click-reference parameter at all",
			mutate: func(target *networks.DeeplinkTarget) { target.ClickRefParam = "" },
		},
		{
			name: "a template that already sets the slot",
			mutate: func(target *networks.DeeplinkTarget) {
				target.Template = "https://go.linkwi.se/z/5-12/CD20/?subid1=somebody-elses"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := aTarget()
			tt.mutate(&target)
			got, err := client.BuildDeeplink(t.Context(), target, anIssuedRef(t))
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink() = %q, %v; want ErrDeeplinkNotFormed", got, err)
			}
			if got != "" {
				t.Errorf("a refused redirect still returned the URL %q", got)
			}
		})
	}
}

// TestAnAbandonedRequestIsNotHandedAURL. A caller whose request has already
// been cancelled should not be given a redirect it is about to send nobody
// to - and the refusal must not read as our routing being broken.
func TestAnAbandonedRequestIsNotHandedAURL(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := aDeeplinkClient(t).BuildDeeplink(ctx, aTarget(), anIssuedRef(t))
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Fatalf("BuildDeeplink() = %q, %v; want ErrDeeplinkNotFormed", got, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not say what stopped it: %v", err)
	}
	if errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Errorf("a cancelled request was reported as a broken route, which sends somebody to fix an offer that is fine: %v", err)
	}
}

// TestBuildingADeeplinkContactsNothing. The redirect is assembled from the
// offer's own template, so attribution never depends on Linkwise being
// reachable at the moment of the click - which on this network would put a
// second of latency in front of the one action a member takes.
func TestBuildingADeeplinkContactsNothing(t *testing.T) {
	t.Parallel()

	// The client below has the real base URL and a credential; if
	// BuildDeeplink reached for the network it would fail rather than answer.
	account, err := networks.NewPublisherAccount(uuid.New(), linkwise.ID, "CD20")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	client, err := linkwise.New(account,
		linkwise.WithCredential(theUsername, thePassword),
		// A rate of one request a minute: anything that took a token would
		// either block or fail, and this returns immediately.
		linkwise.WithRateLimitPerMinute(1))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if _, err := client.BuildDeeplink(t.Context(), aTarget(), anIssuedRef(t)); err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}
}
