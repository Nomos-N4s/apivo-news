// The tests for deeplink.go: that every refusal names both which kind of
// refusal it is and that the member must not be redirected. It holds the
// route fixture and the network id, which the port's own tests build on.

package networks_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// portTestNetworkID is the network every case below is reported by, fixed so
// a failing test prints the same identifier every run.
const portTestNetworkID = networks.NetworkID("awin")

// portTestOfferID is the offer every deeplink case is built against.
var portTestOfferID = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// portTestTarget is a live route on the network under test, carrying the two
// facts a redirect is built from: the template and the network's own
// click-reference parameter (FR-021).
func portTestTarget() networks.DeeplinkTarget {
	return networks.DeeplinkTarget{
		OfferID:       portTestOfferID,
		NetworkID:     portTestNetworkID,
		ClickRefParam: "clickref",
		Template:      "https://www.awin1.com/cread.php?awinmid=4471&ued=https%3A%2F%2Fgartenhaus.example",
	}
}

func TestValidateDeeplinkInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		id     networks.NetworkID
		target networks.DeeplinkTarget
		ref    networks.IssuedClickRef
		// wantErr is the sentinel beneath the umbrella that names WHICH kind
		// of refusal this is; nil means the inputs describe a redirect.
		// Every refusal is also checked against the umbrella below.
		wantErr error
		wantIn  []string
	}{
		{
			name:   "a live route with a minted click reference",
			id:     portTestNetworkID,
			target: portTestTarget(),
			ref:    portTestIssuedRef,
		},
		{
			name: "an offer published on another network",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.NetworkID = "tradedoubler"
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{`"tradedoubler"`, `"awin"`},
		},
		{
			name: "an offer whose network differs only in case",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.NetworkID = "AWIN"
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{`"AWIN"`},
		},
		{
			name: "an offer with no deeplink template",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "  "
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{"no deeplink template"},
		},
		{
			name: "a template an operator's UPDATE left padded",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = " " + o.Template
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{"padded with space"},
		},
		{
			name: "a relative template, which is no redirect target at all",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "/cread.php?awinmid=4471"
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{"absolute http or https"},
		},
		{
			name: "a template whose scheme a browser must never be handed",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "javascript:alert(document.cookie)"
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{"absolute http or https"},
		},
		{
			name: "an offer naming no click-reference parameter (FR-021)",
			id:   portTestNetworkID,
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.ClickRefParam = "  "
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrDeeplinkInputsRefused,
			wantIn:  []string{"click-reference parameter"},
		},
		{
			name:    "no click reference at all, so the redirect is being built out of order (FR-020)",
			id:      portTestNetworkID,
			target:  portTestTarget(),
			ref:     networks.IssuedClickRef{},
			wantErr: networks.ErrInvalidIssuedClickRef,
			wantIn:  []string{"no click reference"},
		},
		{
			name: "an adapter with no network id of its own",
			id:   "",
			target: func() networks.DeeplinkTarget {
				// The target names the same empty network, so the
				// wrong-network guard cannot fire first and mask the one
				// this case exists to pin.
				o := portTestTarget()
				o.NetworkID = ""
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrInvalidNetworkID,
			wantIn:  []string{"names no network"},
		},
		{
			name: "an adapter whose id is merely mistyped",
			id:   "Awin",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.NetworkID = "Awin"
				return o
			}(),
			ref:     portTestIssuedRef,
			wantErr: networks.ErrInvalidNetworkID,
			wantIn:  []string{`"Awin"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := networks.ValidateDeeplinkInputs(tc.id, tc.target, tc.ref)
			portTestAssert(t, "ValidateDeeplinkInputs()", err, tc.wantErr, tc.wantIn)
			if tc.wantErr != nil && !errors.Is(err, networks.ErrDeeplinkNotFormed) {
				t.Errorf("ValidateDeeplinkInputs() = %v, want it to wrap %v too: contract rule 5 promises one umbrella",
					err, networks.ErrDeeplinkNotFormed)
			}
		})
	}
}

// TestDeeplinkRefusalCarriesBothVerdicts pins the two things a caller needs
// from a refused redirect, and pins that they are different questions. Rule 5
// promises one umbrella - do not redirect this member - and every refusal
// wraps it. Beside it, ErrDeeplinkInputsRefused says the refusal is
// deterministic: our own routing bug or a route somebody has to fix, never
// the network being unwell. Without the second, the click-out handler answers
// 502 (contracts/http-api.md) and the on-call is paged towards a network that
// is working perfectly.
func TestDeeplinkRefusalCarriesBothVerdicts(t *testing.T) {
	t.Parallel()

	broken := []networks.DeeplinkTarget{
		func() networks.DeeplinkTarget { o := portTestTarget(); o.NetworkID = "tradedoubler"; return o }(),
		func() networks.DeeplinkTarget { o := portTestTarget(); o.Template = ""; return o }(),
		func() networks.DeeplinkTarget { o := portTestTarget(); o.Template = "gartenhaus.example"; return o }(),
		func() networks.DeeplinkTarget { o := portTestTarget(); o.ClickRefParam = ""; return o }(),
	}
	for i, target := range broken {
		err := networks.ValidateDeeplinkInputs(portTestNetworkID, target, portTestIssuedRef)
		if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
			t.Errorf("target %d: ValidateDeeplinkInputs() = %v, want an error wrapping %v",
				i+1, err, networks.ErrDeeplinkNotFormed)
		}
		if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
			t.Errorf("target %d: ValidateDeeplinkInputs() = %v, want an error wrapping %v: a caller cannot otherwise tell our bug from the network's",
				i+1, err, networks.ErrDeeplinkInputsRefused)
		}
		if errors.Is(err, networks.ErrNetworkUnavailable) || errors.Is(err, networks.ErrNetworkRefused) {
			t.Errorf("target %d: ValidateDeeplinkInputs() = %v, which blames the network for our own inputs", i+1, err)
		}
	}

	err := networks.ValidateDeeplinkInputs(portTestNetworkID, portTestTarget(), networks.IssuedClickRef{})
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) || !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Errorf("an unminted reference = %v, want it to wrap both %v and %v",
			err, networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused)
	}
}

// TestAppendClickRef covers the assembly half of contract rule 5 directly,
// rather than only through an adapter. Both rules it keeps are ones a
// redirect breaks silently - the member is sent to the retailer either way -
// so they are pinned here, once, for every adapter that calls it.
func TestAppendClickRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target networks.DeeplinkTarget
		want   string
		// wantErr is the sentinel a refusal must carry beneath the umbrella;
		// nil means a URL is expected instead.
		wantErr error
	}{
		{
			name:   "a template that already carries a query",
			target: portTestTarget(),
			want: "https://www.awin1.com/cread.php?awinmid=4471&ued=https%3A%2F%2Fgartenhaus.example&clickref=" +
				portTestRefValue,
		},
		{
			name: "a template with no query at all",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "https://www.awin1.com/awclick.php"
				return o
			}(),
			want: "https://www.awin1.com/awclick.php?clickref=" + portTestRefValue,
		},
		{
			name: "a template whose query would not survive re-encoding",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				// Reversed pairs and an unescaped comma: url.Values.Encode
				// would sort these and escape the comma, which is a change
				// to a value the operator typed.
				o.Template = "https://www.awin1.com/cread.php?ued=https%3A%2F%2Fa.example&p=x,y&awinmid=4471"
				return o
			}(),
			want: "https://www.awin1.com/cread.php?ued=https%3A%2F%2Fa.example&p=x,y&awinmid=4471&clickref=" +
				portTestRefValue,
		},
		{
			name: "a click-reference parameter that has to be escaped",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.ClickRefParam = "click ref"
				return o
			}(),
			want: "https://www.awin1.com/cread.php?awinmid=4471&ued=https%3A%2F%2Fgartenhaus.example&click+ref=" +
				portTestRefValue,
		},
		{
			name: "a template that already sets the click-reference parameter",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "https://www.awin1.com/cread.php?awinmid=4471&clickref=whatever-the-operator-pasted"
				return o
			}(),
			wantErr: networks.ErrDeeplinkInputsRefused,
		},
		{
			name: "a template that already sets it to nothing",
			target: func() networks.DeeplinkTarget {
				o := portTestTarget()
				o.Template = "https://www.awin1.com/cread.php?awinmid=4471&clickref="
				return o
			}(),
			wantErr: networks.ErrDeeplinkInputsRefused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := networks.AppendClickRef(tt.target, portTestIssuedRef)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("AppendClickRef() error = %v, want one wrapping %v", err, tt.wantErr)
				}
				if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
					t.Errorf("AppendClickRef() error = %v, which does not tell the caller not to redirect", err)
				}
				if got != "" {
					t.Errorf("AppendClickRef() returned %q beside a refusal; a half-built URL still redirects", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("AppendClickRef() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("AppendClickRef() = %q, want %q", got, tt.want)
			}
		})
	}
}
