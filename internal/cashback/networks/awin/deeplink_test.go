// The tests for deeplink.go: that a redirect is assembled from the route the
// operator configured, that the click reference lands in the parameter the
// route names rather than one written here, and that a template Awin's own
// Link Builder hands out half-finished is refused rather than redirected.

package awin_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/awin"
)

// theOfferID is the offer every route below is published against, fixed so a
// failing test prints the same identifier every run.
var theOfferID = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// theClickRef is a reference of the shape the click table accepts: 22
// base64url characters, which is the first length carrying the 128 bits
// FR-020's unguessability is specified in.
const theClickRef = "3zK9pQ2vX7mB4nL8sR1tYw"

// offline builds a client that never contacts anything. BuildDeeplink is the
// one port method that reaches no network, so its tests need no server - and
// a test that stood one up would not notice an implementation that called it.
func offline(t *testing.T) *awin.Client {
	t.Helper()
	client, err := awin.New(anAccount(t), awin.WithToken(theToken))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client
}

// aRoute is a live Awin route: a cread.php template with the advertiser and
// the encoded destination already on it, and clickref as the parameter Awin
// reads the reference from.
func aRoute() networks.DeeplinkTarget {
	return networks.DeeplinkTarget{
		OfferID:       theOfferID,
		NetworkID:     awin.ID,
		ClickRefParam: "clickref",
		Template:      "https://www.awin1.com/cread.php?awinmid=4471&awinaffid=123456&ued=https%3A%2F%2Fgartenhaus.example%2Fsale",
	}
}

// aRef is the reference Apivo minted for the click being redirected.
func aRef(t *testing.T) networks.IssuedClickRef {
	t.Helper()
	ref, err := networks.NewIssuedClickRef(theClickRef)
	if err != nil {
		t.Fatalf("NewIssuedClickRef(): %v", err)
	}
	return ref
}

func TestTheReferenceLandsInTheParameterTheRouteNames(t *testing.T) {
	t.Parallel()

	// Not clickref. Awin's own parameter is clickref, and a test that used
	// it could not tell an adapter reading the route from one with the name
	// written into it - which is the bug FR-021 exists to prevent, and which
	// loses attribution on every click without failing.
	route := aRoute()
	route.ClickRefParam = "pref1"

	got, err := offline(t).BuildDeeplink(t.Context(), route, aRef(t))
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("BuildDeeplink() returned %q, which is not a URL: %v", got, err)
	}
	if value := parsed.Query().Get("pref1"); value != theClickRef {
		t.Errorf("BuildDeeplink() put %q in pref1, want %q", value, theClickRef)
	}
	if parsed.Query().Has("clickref") {
		t.Errorf("BuildDeeplink() = %q, which uses Awin's own parameter name instead of the route's", got)
	}
}

func TestTheRestOfTheRouteIsCarriedThrough(t *testing.T) {
	t.Parallel()

	got, err := offline(t).BuildDeeplink(t.Context(), aRoute(), aRef(t))
	if err != nil {
		t.Fatalf("BuildDeeplink(): %v", err)
	}

	want := aRoute().Template + "&clickref=" + theClickRef
	if got != want {
		t.Errorf("BuildDeeplink() = %q, want %q", got, want)
	}
}

// TestAnUnreplacedPlaceholderIsRefused is the case Awin's documentation puts
// in front of every publisher: their Link Builder emits general tracking
// links carrying a literal !!!id!!! that "partners will have to manually
// replace with their own unique Publisher ID". Pasted into an offer, such a
// link redirects perfectly and credits nobody.
func TestAnUnreplacedPlaceholderIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		// wantNamed is the placeholder the refusal must quote, so an operator
		// is sent to a string rather than to a URL to read character by
		// character.
		wantNamed string
	}{
		{
			name:      "a general tracking link straight out of the Link Builder",
			template:  "https://www.awin1.com/cread.php?awinmid=4471&awinaffid=!!!id!!!&ued=https%3A%2F%2Fa.example",
			wantNamed: "!!!id!!!",
		},
		{
			name:      "a placeholder anywhere else on the route",
			template:  "https://www.awin1.com/cread.php?awinmid=4471&campaign=!!!promotype!!!",
			wantNamed: "!!!promotype!!!",
		},
		{
			name:      "a placeholder somebody half-deleted",
			template:  "https://www.awin1.com/cread.php?awinmid=4471&awinaffid=!!!id",
			wantNamed: "!!!id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := aRoute()
			route.Template = tt.template

			got, err := offline(t).BuildDeeplink(t.Context(), route, aRef(t))
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink() error = %v, which does not tell the caller not to redirect", err)
			}
			if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
				t.Errorf("BuildDeeplink() error = %v, which would page the on-call towards Awin for a route we can see is wrong", err)
			}
			if got != "" {
				t.Errorf("BuildDeeplink() returned %q beside a refusal; a half-built URL still redirects", got)
			}
			if !strings.Contains(err.Error(), tt.wantNamed) {
				t.Errorf("BuildDeeplink() error = %v, which does not name %q", err, tt.wantNamed)
			}
			if !strings.Contains(err.Error(), theOfferID.String()) {
				t.Errorf("BuildDeeplink() error = %v, which does not name the offer to go and fix", err)
			}
		})
	}
}

// TestARouteWithAPlaceholderIsNotSalvaged pins the order: the refusal does
// not depend on the template being otherwise assemblable, and no URL - not
// even one inside an error message - exists by the time it is made.
func TestARouteWithAPlaceholderIsNotSalvaged(t *testing.T) {
	t.Parallel()

	route := aRoute()
	route.Template = "https://www.awin1.com/cread.php?awinaffid=!!!id!!!"

	got, err := offline(t).BuildDeeplink(t.Context(), route, aRef(t))
	if err == nil {
		t.Fatalf("BuildDeeplink() = %q, want a refusal", got)
	}
	if strings.Contains(err.Error(), theClickRef) {
		t.Errorf("BuildDeeplink() error = %v, which carries a reference into a refusal", err)
	}
}

func TestTheCheckableRefusalsComeFromThePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		route func() networks.DeeplinkTarget
	}{
		{
			name: "an offer published on another network",
			route: func() networks.DeeplinkTarget {
				r := aRoute()
				r.NetworkID = "tradedoubler"
				return r
			},
		},
		{
			name: "a route naming no click-reference parameter",
			route: func() networks.DeeplinkTarget {
				r := aRoute()
				r.ClickRefParam = ""
				return r
			},
		},
		{
			name: "a template that is not an absolute URL",
			route: func() networks.DeeplinkTarget {
				r := aRoute()
				r.Template = "/cread.php?awinmid=4471"
				return r
			},
		},
		{
			name: "a template that is not http or https",
			route: func() networks.DeeplinkTarget {
				r := aRoute()
				r.Template = "javascript:alert(1)"
				return r
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := offline(t).BuildDeeplink(t.Context(), tt.route(), aRef(t))
			if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Fatalf("BuildDeeplink() error = %v, which does not tell the caller not to redirect", err)
			}
			if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
				t.Errorf("BuildDeeplink() error = %v, which does not say the refusal is ours to fix", err)
			}
			if got != "" {
				t.Errorf("BuildDeeplink() returned %q beside a refusal", got)
			}
		})
	}
}

// TestAnAbandonedRequestIsNotRedirected covers the third kind of refusal:
// neither our routing bug nor Awin's fault, so it carries the umbrella and
// the cause and not the deterministic sentinel.
func TestAnAbandonedRequestIsNotRedirected(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := offline(t).BuildDeeplink(ctx, aRoute(), aRef(t))
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Fatalf("BuildDeeplink() error = %v, which does not tell the caller not to redirect", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("BuildDeeplink() error = %v, which does not say the request was abandoned", err)
	}
	if errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Errorf("BuildDeeplink() error = %v, which blames a route for a cancelled request", err)
	}
	if got != "" {
		t.Errorf("BuildDeeplink() returned %q beside a refusal", got)
	}
}

func TestTheClientNamesTheNetworkItSpeaksTo(t *testing.T) {
	t.Parallel()

	if got := offline(t).ID(); got != awin.ID {
		t.Errorf("ID() = %q, want %q", got, awin.ID)
	}
}
