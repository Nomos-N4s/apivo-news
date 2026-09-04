package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// TestEveryShippedDriverIsBothServableAndSeedable is FR-092's defect, asserted
// rather than described.
//
// Before the registry there were two lists: networkAdapter shipped the
// fixture, documentedNetwork shipped the fixture AND awin. So
// `connect-network --driver awin` wrote a cashback.network row and the server
// then refused to start against it - a deployment seeded for a network the
// binary cannot poll. Nothing compared the two lists, so nothing noticed.
//
// This is the test that would have. It reaches both answers through the two
// exported paths an operator actually takes, so a future second list is a
// failure here rather than a deployment nobody can start.
func TestEveryShippedDriverIsBothServableAndSeedable(t *testing.T) {
	t.Parallel()

	if len(shippedNetworks) == 0 {
		t.Fatal("no driver is registered, so this test would pass without asserting anything")
	}

	for driver := range shippedNetworks {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()

			documented, err := documentedNetwork(driver)
			if err != nil {
				t.Fatalf("documentedNetwork(%q): %v", driver, err)
			}
			if documented.ID == "" {
				t.Errorf("documentedNetwork(%q) named no network, so connect-network would seed a row with no id", driver)
			}
			if documented.ID.String() != driver {
				t.Errorf("documentedNetwork(%q) describes %q: the row connect-network seeds would name a different network from the one the adapter polls",
					driver, documented.ID)
			}

			account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID(driver), "publisher-1")
			if err != nil {
				t.Fatalf("building a publisher account for %q: %v", driver, err)
			}
			adapter, err := networkAdapter(driver, account)
			if err != nil {
				t.Fatalf("networkAdapter(%q): a driver connect-network can seed must also be one the server can build: %v", driver, err)
			}
			if adapter == nil {
				t.Fatal("networkAdapter returned no adapter and no error")
			}

			// ValidateNetwork is what the poller runs on every tick. An
			// adapter that fails it is one every sweep would refuse.
			if err := networks.ValidateNetwork(adapter); err != nil {
				t.Errorf("the registered adapter does not satisfy the port: %v", err)
			}
			if adapter.ID().String() != driver {
				t.Errorf("registered under %q but reports %q; the registry key and the adapter would disagree about which network this is",
					driver, adapter.ID())
			}
		})
	}
}

// TestADriverTheBinaryDoesNotShipIsRefusedByBothPaths. The other half: absence
// has to be absent from both answers too, or the two lists have merely been
// re-created with one of them empty.
func TestADriverTheBinaryDoesNotShipIsRefusedByBothPaths(t *testing.T) {
	t.Parallel()

	// awin is the case this matters for and the reason the registry exists:
	// documentedNetwork used to know it while networkAdapter did not.
	// *awin.Client has no FetchTransactions and no Limits, so it cannot be
	// registered until it implements the port (T236-T241, deferred).
	for _, driver := range []string{config.NetworkDriverAwin, "linkwise", "tradetracker", "not_a_network"} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()

			if _, ok := shippedNetworks[driver]; ok {
				t.Skipf("%q is registered now, so it is no longer an absent driver", driver)
			}

			if _, err := documentedNetwork(driver); err == nil {
				t.Errorf("documentedNetwork(%q) succeeded: connect-network would seed a row for a network the server cannot poll", driver)
			}

			account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID(driver), "publisher-1")
			if err != nil {
				t.Fatalf("building a publisher account for %q: %v", driver, err)
			}
			if _, err := networkAdapter(driver, account); err == nil {
				t.Errorf("networkAdapter(%q) succeeded for a driver this binary does not ship", driver)
			}
		})
	}
}

// TestAnEmptyDriverSaysNothingWasNamed. An operator who configured nothing and
// one who configured something wrong have made different mistakes, and the
// message is the only thing that tells them apart.
func TestAnEmptyDriverSaysNothingWasNamed(t *testing.T) {
	t.Parallel()

	_, err := documentedNetwork("")
	if !errors.Is(err, errNoDriverNamed) {
		t.Errorf("documentedNetwork(\"\") = %v, want it to wrap errNoDriverNamed", err)
	}
}

// TestAnUnknownDriverNamesWhatTheBinaryHas. An error that refuses without
// saying what would have worked sends the reader to the source.
func TestAnUnknownDriverNamesWhatTheBinaryHas(t *testing.T) {
	t.Parallel()

	_, err := documentedNetwork("not_a_network")
	if err == nil {
		t.Fatal("an unknown driver was accepted")
	}
	if !strings.Contains(err.Error(), config.NetworkDriverFixture) {
		t.Errorf("the refusal does not name a driver the binary ships: %v", err)
	}
}

// TestShippedDriversReadsTheSameEveryTime. Map iteration is not ordered, and
// an error message that reorders itself between runs is one nobody can search
// for in a log.
func TestShippedDriversReadsTheSameEveryTime(t *testing.T) {
	t.Parallel()

	first := shippedDrivers()
	for range 20 {
		if got := shippedDrivers(); got != first {
			t.Fatalf("shippedDrivers is not stable: %q then %q", first, got)
		}
	}
	if first == "" {
		t.Error("shippedDrivers named nothing at all")
	}
}
