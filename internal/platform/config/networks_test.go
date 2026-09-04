package config_test

// NETWORKS and the per-driver blocks (T215-T217), tested through FromEnv
// rather than against parseNetworks directly: what an operator sets is an
// environment, and a list that parsed correctly into a struct nothing else
// agreed with would pass a unit test and fail a deployment.

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// networksEnv is the baseline with NETWORKS replaced, so each case states
// only what it is about.
func networksEnv(overrides map[string]string) map[string]string {
	return withEnv(enabledCashbackEnv(), overrides)
}

// TestParseNetworks covers the list itself: what a well-formed one produces,
// and every shape that is refused.
func TestParseNetworks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want []config.NetworkConfig
		// wantErr, when non-empty, is a fragment the refusal must name.
		wantErr string
	}{
		{
			name: "one network",
			env:  networksEnv(nil),
			want: configuredNetworks(),
		},
		{
			// ORDER IS THE OPERATOR'S, not the map's. This is the assertion
			// that makes theConfiguredNetwork's "the first entry" mean
			// anything: a list that came back in a different order each run
			// would wire a different network each restart.
			name: "two networks keep the order they were named in",
			env: networksEnv(map[string]string{
				"NETWORKS":                             "reference_network," + config.NetworkDriverFixture,
				"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID": "publisher-42",
				"NETWORK_REFERENCE_NETWORK_API_KEY":    "network-key",
			}),
			want: []config.NetworkConfig{
				{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
				},
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			},
		},
		{
			name: "whitespace around each entry is trimmed",
			env: networksEnv(map[string]string{
				"NETWORKS":                             "  reference_network ,\t" + config.NetworkDriverFixture + " ",
				"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID": "publisher-42",
				"NETWORK_REFERENCE_NETWORK_API_KEY":    "network-key",
			}),
			want: []config.NetworkConfig{
				{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
				},
				{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			},
		},
		{
			// A block whose driver is not named reaches nothing. This is the
			// stance the whole design rests on: presence does not imply
			// intent, or a typo could conjure a network.
			name: "a block nothing names is not a network",
			env: networksEnv(map[string]string{
				"NETWORK_LINKWISE_ACCOUNT_ID": "publisher-99",
				"NETWORK_LINKWISE_API_KEY":    "linkwise-key",
			}),
			want: configuredNetworks(),
		},
		{
			name:    "a doubled comma is refused",
			env:     networksEnv(map[string]string{"NETWORKS": "awin,,linkwise"}),
			wantErr: "empty entry",
		},
		{
			name:    "a trailing comma is refused",
			env:     networksEnv(map[string]string{"NETWORKS": config.NetworkDriverFixture + ","}),
			wantErr: "empty entry",
		},
		{
			name:    "a repeated driver is refused",
			env:     networksEnv(map[string]string{"NETWORKS": "awin,awin"}),
			wantErr: "twice",
		},
		{
			// It names the network account table, because that IS the
			// arrangement an operator asking for this wants.
			name:    "the refusal for a repeat says where two accounts belong",
			env:     networksEnv(map[string]string{"NETWORKS": "awin,awin"}),
			wantErr: "cashback.network_account",
		},
		{
			name: "an empty list polls nothing and is not an error",
			env: networksEnv(map[string]string{
				"NETWORKS":                   "",
				"NETWORK_FIXTURE_ACCOUNT_ID": "",
			}),
			want: nil,
		},
		{
			name: "a list of only whitespace is the same as an empty one",
			env: networksEnv(map[string]string{
				"NETWORKS":                   "   ",
				"NETWORK_FIXTURE_ACCOUNT_ID": "",
			}),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.FromEnv(envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("FromEnv() = %+v, want an error naming %q", got.Cashback.Networks, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not name %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv() error: %v", err)
			}
			if !reflect.DeepEqual(got.Cashback.Networks, tt.want) {
				t.Fatalf("Networks = %+v, want %+v", got.Cashback.Networks, tt.want)
			}
		})
	}
}

// TestLegacyNetworkKeysAreRefused is the upgrade gate. Aliasing them onto the
// first network would let a deployment that meant to run two networks run
// one with every diagnostic green, which is the failure this refusal buys
// one edit at upgrade time to avoid.
func TestLegacyNetworkKeysAreRefused(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"NETWORK_DRIVER",
		"NETWORK_ACCOUNT_ID",
		"NETWORK_API_KEY",
		"NETWORK_API_SECRET",
		"NETWORK_SOURCE_LANGUAGE",
	} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, err := config.FromEnv(envFrom(networksEnv(map[string]string{key: "anything"})))
			if err == nil {
				t.Fatalf("%s was accepted", key)
			}
			for _, want := range []string{key, config.NetworksKey, "NETWORK_<DRIVER>_*"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal for %s does not name %q: %v", key, want, err)
				}
			}
		})
	}
}

// TestLegacyNetworkKeysAreRefusedTogether: an operator upgrading has all five
// set, and being told about them one restart at a time is five restarts.
func TestLegacyNetworkKeysAreRefusedTogether(t *testing.T) {
	t.Parallel()

	_, err := config.FromEnv(envFrom(networksEnv(map[string]string{
		"NETWORK_DRIVER":     "awin",
		"NETWORK_ACCOUNT_ID": "publisher-42",
		"NETWORK_API_KEY":    "a-key",
	})))
	if err == nil {
		t.Fatal("the legacy keys were accepted")
	}
	for _, want := range []string{"NETWORK_DRIVER", "NETWORK_ACCOUNT_ID", "NETWORK_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestLegacyNetworkKeysAreRefusedWithCashbackOff. They are read before
// CASHBACK_ENABLED is consulted, deliberately: an operator who upgrades with
// the product off and turns it on a week later would otherwise meet the
// refusal at the worst possible moment.
func TestLegacyNetworkKeysAreRefusedWithCashbackOff(t *testing.T) {
	t.Parallel()

	_, err := config.FromEnv(envFrom(map[string]string{
		"DATABASE_URL":   "postgres://x",
		"NETWORK_DRIVER": "awin",
	}))
	if err == nil {
		t.Fatal("a legacy key was accepted with cashback off")
	}
}

// TestAnEmptyLegacyKeyIsNotSet keeps the refusal from firing on an env file
// that carries the key commented out to zero - which is what an upgraded
// template looks like mid-edit.
func TestAnEmptyLegacyKeyIsNotSet(t *testing.T) {
	t.Parallel()

	if _, err := config.FromEnv(envFrom(networksEnv(map[string]string{
		// withEnv deletes on "", so set the whitespace case directly.
		"NETWORK_DRIVER": "   ",
	}))); err != nil {
		t.Fatalf("a blank legacy key was refused: %v", err)
	}
}

func TestNetworkConfigKeys(t *testing.T) {
	t.Parallel()

	accountID, apiKey, apiSecret, sourceLanguage := config.NetworkConfig{Driver: "reference_network"}.Keys()
	for got, want := range map[string]string{
		accountID:      "NETWORK_REFERENCE_NETWORK_ACCOUNT_ID",
		apiKey:         "NETWORK_REFERENCE_NETWORK_API_KEY",
		apiSecret:      "NETWORK_REFERENCE_NETWORK_API_SECRET",
		sourceLanguage: "NETWORK_REFERENCE_NETWORK_SOURCE_LANGUAGE",
	} {
		if got != want {
			t.Errorf("Keys() gave %q, want %q", got, want)
		}
	}
}

func TestNetworkConfigMissingKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.NetworkConfig
		want []string
	}{
		{
			name: "a live network with nothing set",
			cfg:  config.NetworkConfig{Driver: "reference_network"},
			want: []string{"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID", "NETWORK_REFERENCE_NETWORK_API_KEY"},
		},
		{
			name: "a live network with no credential",
			cfg:  config.NetworkConfig{Driver: "reference_network", AccountID: "publisher-42"},
			want: []string{"NETWORK_REFERENCE_NETWORK_API_KEY"},
		},
		{
			// The fixture needs no CREDENTIAL and still needs an ACCOUNT:
			// the cursors live on a network_account row, and an adapter
			// that needs no credential still polls on behalf of somebody.
			name: "the fixture still needs an account",
			cfg:  config.NetworkConfig{Driver: config.NetworkDriverFixture},
			want: []string{"NETWORK_FIXTURE_ACCOUNT_ID"},
		},
		{
			name: "the fixture with an account is complete",
			cfg:  config.NetworkConfig{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			want: nil,
		},
		{
			// An API SECRET is never required: plenty of networks issue one
			// value, and demanding a pair would refuse them.
			name: "a live network needs no secret",
			cfg: config.NetworkConfig{
				Driver:    "reference_network",
				AccountID: "publisher-42",
				APIKey:    config.NewSecret("network-key"),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.cfg.MissingKeys()
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("MissingKeys() = %v, want %v", got, tt.want)
			}
			if usable := tt.cfg.Usable(); usable != (len(tt.want) == 0) {
				t.Fatalf("Usable() = %v with MissingKeys() = %v", usable, got)
			}
		})
	}
}

// TestUnusableNetworks is FR-091's report: the network NAMED, then its keys.
// A network that vanished from a configuration when its credential was
// mistyped would have no name to report, which is why NETWORKS decides
// membership rather than the presence of a block.
func TestUnusableNetworks(t *testing.T) {
	t.Parallel()

	cfg := config.CashbackConfig{
		Enabled: true,
		Networks: []config.NetworkConfig{
			{Driver: config.NetworkDriverFixture, AccountID: "publisher-1"},
			{Driver: "linkwise"},
			{Driver: "tradetracker", AccountID: "publisher-42"},
		},
	}

	unusable := cfg.UnusableNetworks()
	if len(unusable) != 2 {
		t.Fatalf("UnusableNetworks() = %v, want the two that cannot poll", unusable)
	}
	for _, want := range []string{
		`"linkwise" cannot poll: NETWORK_LINKWISE_ACCOUNT_ID, NETWORK_LINKWISE_API_KEY are unset`,
		`"tradetracker" cannot poll: NETWORK_TRADETRACKER_API_KEY is unset`,
	} {
		var found bool
		for _, one := range unusable {
			if one.String() == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no problem reads %q; got %v", want, unusable)
		}
	}

	usable := cfg.UsableNetworks()
	if len(usable) != 1 || usable[0].Driver != config.NetworkDriverFixture {
		t.Fatalf("UsableNetworks() = %v, want only the fixture", usable)
	}
}

// TestMountable is the founder's decision of 2026-09-04, at the one function
// that carries it.
func TestMountable(t *testing.T) {
	t.Parallel()

	complete := configuredNetworks()

	tests := []struct {
		name string
		cfg  config.CashbackConfig
		want bool
	}{
		{
			name: "off is off",
			cfg:  config.CashbackConfig{Networks: complete},
			want: false,
		},
		{
			name: "on and complete",
			cfg:  config.CashbackConfig{Enabled: true, Networks: complete},
			want: true,
		},
		{
			// Nothing configured is not half-configured. A deployment that
			// names no network still serves the wallet, the money loop and
			// click-outs on the catalogue it already has.
			name: "on with no network at all",
			cfg:  config.CashbackConfig{Enabled: true},
			want: true,
		},
		{
			name: "one unusable network is enough to stop it",
			cfg: config.CashbackConfig{
				Enabled:  true,
				Networks: []config.NetworkConfig{{Driver: "linkwise"}},
			},
			want: false,
		},
		{
			// ALL of them, not merely one. The founder rejected "poll the
			// ones that work": a deployment that names two and can poll one
			// is not the deployment its operator described, and members
			// would be owed money from a network nobody is reading.
			name: "a usable network beside an unusable one does not save it",
			cfg: config.CashbackConfig{
				Enabled:  true,
				Networks: append(complete, config.NetworkConfig{Driver: "linkwise"}),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.Mountable(); got != tt.want {
				t.Fatalf("Mountable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMountableIsNotMissing pins the distinction the whole decision rests on,
// in one place, so that moving a network key into Missing() fails here rather
// than in production: Missing() stops the PROCESS - the news site with it -
// and Mountable stops only the cashback product.
func TestMountableIsNotMissing(t *testing.T) {
	t.Parallel()

	cfg := config.CashbackConfig{
		Enabled:      true,
		LedgerDriver: config.LedgerDriverMemory,
		Networks:     []config.NetworkConfig{{Driver: "linkwise"}},
	}
	if missing := cfg.Missing(); len(missing) != 0 {
		t.Fatalf("Missing() = %v; a network that cannot poll must not stop the process", missing)
	}
	if cfg.Mountable() {
		t.Fatal("Mountable() is true over a network that cannot poll")
	}
}

// TestNetworksLogValueRedactsEveryNetwork. A redaction that holds for the
// first entry and not the second is the one a single-network test cannot
// see, and the group-per-driver shape is what an operator running two reads
// to tell which key belongs to which.
func TestNetworksLogValueRedactsEveryNetwork(t *testing.T) {
	t.Parallel()

	cfg := config.CashbackConfig{
		Enabled: true,
		Networks: []config.NetworkConfig{
			{
				Driver:         "linkwise",
				AccountID:      "publisher-42",
				APIKey:         config.NewSecret("linkwise-key-value"),
				APISecret:      config.NewSecret("linkwise-secret-value"),
				SourceLanguage: "el",
			},
			{
				Driver:    "tradetracker",
				AccountID: "publisher-99",
				APIKey:    config.NewSecret("tradetracker-key-value"),
			},
		},
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cashback", "cashback", cfg)
	out := buf.String()

	for _, secret := range []string{
		"linkwise-key-value",
		"linkwise-secret-value",
		"tradetracker-key-value",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q reached the log: %s", secret, out)
		}
	}
	for _, want := range []string{
		// Keyed by driver, each with its own account beside it. Pinned as
		// key:value pairs rather than as bare fragments: a line with the two
		// accounts swapped between the networks would carry both fragments
		// and be exactly wrong.
		`"linkwise":{"account_id":"publisher-42"`,
		`"tradetracker":{"account_id":"publisher-99"`,
		`"api_key_set":true`,
		`"api_secret_set":false`,
		`"source_language":"el"`,
		`"usable":true`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the log line: %s", want, out)
		}
	}
}

// TestNetworksLogValueNamesANetworkWithNoDriver. Unreachable through
// parseNetworks, which refuses an empty entry - and asserted because the
// logging path must not fail open on a config built in code: a group with an
// empty key would collapse two networks into one line an operator cannot
// point at.
func TestNetworksLogValueNamesANetworkWithNoDriver(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cashback", "cashback", config.CashbackConfig{
		Networks: []config.NetworkConfig{{AccountID: "publisher-42"}},
	})
	if out := buf.String(); !strings.Contains(out, `"network_0":{"account_id":"publisher-42"`) {
		t.Fatalf("a network with no driver logged as %s", out)
	}
}
