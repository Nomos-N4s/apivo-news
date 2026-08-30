package clickout_test

// Choosing the adapter a redirect is built by. The band decides which
// network a click is issued through, so this lookup is the last place a
// click can be assembled from one network's template and recognised by
// neither.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// stubAdapter is the narrow thing the registry asks for: something that
// names its network and builds a URL. Nothing here polls, which is the point
// of Redirector being narrower than the whole adapter port.
type stubAdapter struct {
	id  networks.NetworkID
	url string
	err error

	built networks.DeeplinkTarget
}

func (s *stubAdapter) ID() networks.NetworkID { return s.id }

func (s *stubAdapter) BuildDeeplink(_ context.Context, target networks.DeeplinkTarget, _ networks.IssuedClickRef) (string, error) {
	s.built = target
	if s.err != nil {
		return "", s.err
	}
	return s.url, nil
}

func TestARedirectIsBuiltByTheAdapterForItsOwnNetwork(t *testing.T) {
	t.Parallel()

	awin := &stubAdapter{id: "awin", url: "https://awin.example.test/go"}
	other := &stubAdapter{id: "tradedoubler", url: "https://td.example.test/go"}
	deeplinks, err := clickout.NewDeeplinks(awin, other)
	if err != nil {
		t.Fatalf("NewDeeplinks(): %v", err)
	}

	target := networks.DeeplinkTarget{
		OfferID: uuid.New(), NetworkID: "tradedoubler",
		ClickRefParam: "epi", Template: "https://td.example.test/go",
	}
	got, err := deeplinks.Build(t.Context(), target, aRef(t))
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if got != other.url {
		t.Errorf("Build() = %q, want the tradedoubler adapter's %q", got, other.url)
	}
	if awin.built != (networks.DeeplinkTarget{}) {
		t.Errorf("the awin adapter was asked to build %+v, which is another network's band", awin.built)
	}
}

// TestAnOfferOnANetworkThisBinaryDoesNotHaveIsRefusedDeterministically keeps
// the on-call pointed at the right thing. No adapter is a binary or a
// catalogue to fix, not a network having a bad day, and the second sentinel
// is what says so.
func TestAnOfferOnANetworkThisBinaryDoesNotHaveIsRefusedDeterministically(t *testing.T) {
	t.Parallel()

	deeplinks, err := clickout.NewDeeplinks(&stubAdapter{id: "awin", url: "https://awin.example.test/go"})
	if err != nil {
		t.Fatalf("NewDeeplinks(): %v", err)
	}

	built, err := deeplinks.Build(t.Context(), networks.DeeplinkTarget{
		OfferID: uuid.New(), NetworkID: "a-network-nobody-wired",
		ClickRefParam: "ref", Template: "https://x.test/go",
	}, aRef(t))

	if !errors.Is(err, clickout.ErrNoNetworkAdapter) {
		t.Fatalf("Build() error = %v, want one wrapping %v", err, clickout.ErrNoNetworkAdapter)
	}
	// Both sentinels the port promises: "do not redirect this member", and
	// "this will fail the same way forever".
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Errorf("Build() error = %v, want it to wrap ErrDeeplinkNotFormed", err)
	}
	if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
		t.Errorf("Build() error = %v, want it to wrap ErrDeeplinkInputsRefused; without it an operator cannot tell a route to fix from a network having a bad day", err)
	}
	if built != "" {
		t.Errorf("Build() refused and still returned %q; a half-built URL still redirects", built)
	}
}

func TestARegistryThatCouldServeTheWrongAdapterIsRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		adapters []clickout.Redirector
	}{
		{
			// Last-one-wins here would mean one of the two silently serves
			// every click for that network, and which one depends on
			// argument order.
			name:     "two adapters claim one network",
			adapters: []clickout.Redirector{&stubAdapter{id: "awin"}, &stubAdapter{id: "awin"}},
		},
		{
			// An adapter that cannot name itself cannot have its clicks
			// reconciled either.
			name:     "an adapter names no network",
			adapters: []clickout.Redirector{&stubAdapter{id: ""}},
		},
		{name: "a nil adapter", adapters: []clickout.Redirector{nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := clickout.NewDeeplinks(tc.adapters...); err == nil {
				t.Fatal("NewDeeplinks() built a registry that cannot be trusted to pick an adapter")
			}
		})
	}
}
