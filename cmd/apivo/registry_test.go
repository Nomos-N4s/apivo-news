package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
// aConfigurationFor is the environment block a driver would be given, with a
// placeholder wherever it needs a credential.
//
// A driver that needs one is not a driver this test can skip: the point of
// the suite is that every SHIPPED driver is both seedable and servable, and
// one that cannot be built without a password is still servable - it just
// needs the password its own MissingKeys already requires. The values are
// obviously not real, and they never reach a network: nothing here polls.
func aConfigurationFor(driver string) config.NetworkConfig {
	cfg := config.NetworkConfig{Driver: driver, AccountID: "publisher-1"}
	if cfg.NeedsCredentials() {
		cfg.APIKey = config.NewSecret("not-a-real-username")
	}
	if cfg.NeedsCredentialPair() {
		cfg.APISecret = config.NewSecret("not-a-real-password")
	}
	return cfg
}

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
			adapter, err := networkAdapter(aConfigurationFor(driver), account)
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
			if _, err := networkAdapter(aConfigurationFor(driver), account); err == nil {
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

// TestTheConfiguredNetworkTakesTheFirstEntry. It used to scan for the first
// USABLE one, so that a deployment whose leading network lacked a credential
// polled the one behind it. CashbackConfig.Mountable makes that unreachable
// - cashback is not built at all while any configured network is unusable -
// and scanning would now only hide the day the two stopped agreeing.
func TestTheConfiguredNetworkTakesTheFirstEntry(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cfg   config.CashbackConfig
		want  string
		wired bool
	}{
		"nothing configured": {
			cfg: config.CashbackConfig{Enabled: true},
		},
		"one network": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			}},
			want: config.NetworkDriverFixture, wired: true,
		},
		"two networks: the first, in the order NETWORKS named them": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: "linkwise", AccountID: "publisher-42",
					APIKey: config.NewSecret("k"), APISecret: config.NewSecret("s")},
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			}},
			want: "linkwise", wired: true,
		},
		// The one that changed. This configuration never reaches serve -
		// Mountable is false over it - but connect-network is not behind
		// that gate and must be handed the network the operator NAMED, so
		// it can report every key that network is missing.
		"an unusable first network is still the first network": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: "linkwise"},
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			}},
			want: "linkwise", wired: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, wired := theConfiguredNetwork(tt.cfg)
			if wired != tt.wired {
				t.Fatalf("theConfiguredNetwork() wired = %v, want %v", wired, tt.wired)
			}
			if got.Driver != tt.want {
				t.Fatalf("theConfiguredNetwork() = %q, want %q", got.Driver, tt.want)
			}
		})
	}
}

// TestReportNetworkConfiguration holds the startup report to what FR-091 and
// the founder's decision of 2026-09-04 each require of it: the cause named
// per network, and the consequence said once, separately.
func TestReportNetworkConfiguration(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cfg  config.CashbackConfig
		says []string
		mute []string
	}{
		"complete": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			}},
			mute: []string{"cannot poll", "CASHBACK IS NOT MOUNTED", "only the first"},
		},
		"one unusable network": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: "linkwise"},
			}},
			// Without the quotes the renderer puts around the driver: the
			// text handler escapes them, and asserting on the escaping
			// would pin the handler rather than the message.
			says: []string{
				// The SECRET is named too: Linkwise authenticates with HTTP
				// Basic, so its credential is two values and both are keys
				// an operator has to set.
				"cannot poll: NETWORK_LINKWISE_ACCOUNT_ID, NETWORK_LINKWISE_API_KEY, NETWORK_LINKWISE_API_SECRET are unset",
				"CASHBACK IS NOT MOUNTED",
			},
		},
		// The consequence is said ONCE however many networks caused it: an
		// operator reading two "cashback is off" lines would look for two
		// problems.
		"two unusable networks name both and stop once": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: "linkwise"},
				{Driver: "tradetracker"},
			}},
			says: []string{
				"cannot poll: NETWORK_LINKWISE_ACCOUNT_ID",
				"cannot poll: NETWORK_TRADETRACKER_ACCOUNT_ID",
				"drivers=\"linkwise, tradetracker\"",
			},
		},
		// Usable and not wired is a different report and must not be
		// confused with the one above: nothing is broken, this build simply
		// polls one network.
		"a second usable network is reported as not wired": {
			cfg: config.CashbackConfig{Enabled: true, Networks: []config.NetworkConfig{
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
				{Driver: "linkwise", AccountID: "publisher-42",
					APIKey: config.NewSecret("k"), APISecret: config.NewSecret("s")},
			}},
			says: []string{"wires only the first", "not_wired=1", "wired=" + config.NetworkDriverFixture},
			mute: []string{"CASHBACK IS NOT MOUNTED"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			reportNetworkConfiguration(context.Background(),
				slog.New(slog.NewTextHandler(&buf, nil)), tt.cfg)
			logged := buf.String()
			for _, want := range tt.says {
				if !strings.Contains(logged, want) {
					t.Errorf("the report does not carry %q; output: %q", want, logged)
				}
			}
			for _, unwanted := range tt.mute {
				if strings.Contains(logged, unwanted) {
					t.Errorf("the report carries %q and should not; output: %q", unwanted, logged)
				}
			}
			if got := strings.Count(logged, "CASHBACK IS NOT MOUNTED"); got > 1 {
				t.Errorf("the consequence is reported %d times, want at most once; output: %q", got, logged)
			}
		})
	}
}

// TestCredentialRefNamesThisNetworksKey. The row records a KEY INTO
// CONFIGURATION and never a credential (ADR-0003). It was the literal
// "NETWORK_API_KEY", which was true while one network existed and became a
// lie the moment the keys grew a driver in them: every account row would
// have named one key and at most one of them could have been right.
func TestCredentialRefNamesThisNetworksKey(t *testing.T) {
	t.Parallel()

	if got := credentialRef(config.NetworkConfig{Driver: "linkwise"}); got != "NETWORK_LINKWISE_API_KEY" {
		t.Fatalf("credentialRef() = %q, want the network's own key", got)
	}
}
