package main

// The two answers the database cannot give (T228).
//
// The store reads rows. This decides, per network, whether this binary can
// poll it and whether anybody set its credential - the only two facts on
// GET /ops/networks that come from the build and the environment rather than
// from a table. Both are exactly the sort of thing that reads correct until
// somebody adds a driver, so they are asserted rather than reviewed.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// storedNetworks answers with rows as the store would: both booleans false,
// because the store does not know either.
type storedNetworks struct {
	rows []ops.ConnectedNetwork
	err  error
}

func (s storedNetworks) ConnectedNetworks(context.Context) ([]ops.ConnectedNetwork, error) {
	return s.rows, s.err
}

// oneAccountAt builds a stored row for a network with a single account.
func oneAccountAt(id string) ops.ConnectedNetwork {
	return ops.ConnectedNetwork{
		ID:       id,
		Accounts: []ops.ConnectedAccount{{ID: uuid.New(), ExternalPublisherID: "P-1"}},
	}
}

// TestOnlyAShippedDriverReadsAsShipped. FR-092's defect is a row seeded for
// a driver the server cannot poll, and it is invisible until something says
// so. shippedNetworks is the one list, so this asks it rather than a copy.
func TestOnlyAShippedDriverReadsAsShipped(t *testing.T) {
	t.Parallel()
	inspector := networkInspector{
		store: storedNetworks{rows: []ops.ConnectedNetwork{
			oneAccountAt(config.NetworkDriverFixture),
			oneAccountAt(config.NetworkDriverAwin),
		}},
	}

	networks, err := inspector.ConnectedNetworks(context.Background())
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if len(networks) != 2 {
		t.Fatalf("networks = %d, want 2", len(networks))
	}
	if !networks[0].DriverShipped {
		t.Errorf("%q reads as unshipped; it is in shippedNetworks", networks[0].ID)
	}
	// awin is deliberately absent from the registry - *awin.Client does not
	// implement the port - and a deployment that seeded a row for it must be
	// able to see that this binary cannot poll it.
	if networks[1].DriverShipped {
		t.Errorf("%q reads as shipped; the registry does not have it, and the server refuses to start against such a row", networks[1].ID)
	}
}

// TestTheCredentialIsPresentOnlyWhenTheBlockIsComplete. The same check that
// decides whether cashback mounts at all, so the endpoint and that decision
// cannot disagree about which network is usable.
func TestTheCredentialIsPresentOnlyWhenTheBlockIsComplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		network config.NetworkConfig
		want    bool
	}{
		{
			name: "linkwise with both halves of its Basic credential",
			network: config.NetworkConfig{
				Driver:    config.NetworkDriverLinkwise,
				AccountID: "CD20",
				APIKey:    config.NewSecret("user"),
				APISecret: config.NewSecret("pass"),
			},
			want: true,
		},
		{
			name: "linkwise with only half of it",
			network: config.NetworkConfig{
				Driver:    config.NetworkDriverLinkwise,
				AccountID: "CD20",
				APIKey:    config.NewSecret("user"),
			},
			// A username and no password reports every key present unless
			// NeedsCredentialPair is honoured, and the deployment is then
			// refused on its first poll - which reads as the publisher
			// account being rejected rather than as an unset variable.
			want: false,
		},
		{
			name: "the fixture, which needs no credential but still needs an account id",
			network: config.NetworkConfig{
				Driver:    config.NetworkDriverFixture,
				AccountID: "fixture-publisher",
			},
			want: true,
		},
		{
			name:    "the fixture with no account id",
			network: config.NetworkConfig{Driver: config.NetworkDriverFixture},
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inspector := networkInspector{
				cfg:   config.CashbackConfig{Networks: []config.NetworkConfig{c.network}},
				store: storedNetworks{rows: []ops.ConnectedNetwork{oneAccountAt(c.network.Driver)}},
			}
			networks, err := inspector.ConnectedNetworks(context.Background())
			if err != nil {
				t.Fatalf("inspecting: %v", err)
			}
			if got := networks[0].Accounts[0].CredentialPresent; got != c.want {
				t.Errorf("credential_present = %v, want %v", got, c.want)
			}
		})
	}
}

// TestASeededNetworkNobodyConfigured. A row exists, NETWORKS does not name
// it, so no key is even defined for it - reporting the credential absent is
// the only true answer.
func TestASeededNetworkNobodyConfigured(t *testing.T) {
	t.Parallel()
	inspector := networkInspector{
		cfg:   config.CashbackConfig{},
		store: storedNetworks{rows: []ops.ConnectedNetwork{oneAccountAt(config.NetworkDriverLinkwise)}},
	}

	networks, err := inspector.ConnectedNetworks(context.Background())
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if networks[0].Accounts[0].CredentialPresent {
		t.Error("credential_present is true for a network NETWORKS does not name; nothing defines a key for it, so nothing could have set one")
	}
	if !networks[0].DriverShipped {
		t.Error("driver_shipped is false; whether the binary ships an adapter has nothing to do with whether this deployment configured it")
	}
}

// TestAFailedReadIsNotAnEmptyDeployment. The decorator must not turn a
// broken read into "connected to nothing", which is the one answer an
// operator diagnosing silence would most readily believe.
func TestAFailedReadIsNotAnEmptyDeployment(t *testing.T) {
	t.Parallel()
	inspector := networkInspector{store: storedNetworks{err: errors.New("the database is gone")}}
	if _, err := inspector.ConnectedNetworks(context.Background()); err == nil {
		t.Fatal("a failed read came back as a successful empty list")
	}
}
