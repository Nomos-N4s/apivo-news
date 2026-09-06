package config_test

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The house account names enabledCashbackEnv configures, held as constants
// so the envs and the wants below cannot drift apart by a typo.
const (
	houseRounding   = "rounding-remainder"
	houseClawback   = "clawback-loss"
	houseReceivable = "network-receivable"
)

// enabledCashbackEnv fully configures cashback: the product on, a ledger,
// one network adapter that needs no credential but does need an account
// (the cursors live on a network_account row), the two house accounts
// money is routed through (optional outside production, but a complete
// environment is the honest baseline for the wants below), and - because
// the ledger is the sidecar - where it lives.
func enabledCashbackEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":                     "postgres://x",
		"CASHBACK_ENABLED":                 "true",
		"LEDGER_DRIVER":                    config.LedgerDriverBlnk,
		"BLNK_URL":                         "http://blnk:5001",
		"NETWORKS":                         config.NetworkDriverFixture,
		"NETWORK_FIXTURE_ACCOUNT_ID":       "publisher-1",
		"HOUSE_ACCOUNT_ROUNDING":           houseRounding,
		"HOUSE_ACCOUNT_CLAWBACK":           houseClawback,
		"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": houseReceivable,
		"PAYOUT_THRESHOLD_MINOR":           "2000",
		"PAYOUT_THRESHOLD_CURRENCY":        "EUR",
	}
}

// configuredNetworks is the one network enabledCashbackEnv names, as
// FromEnv returns it. A function rather than a package-level slice because
// every want gets its own: a shared slice is one t.Parallel case away from
// being mutated under another.
func configuredNetworks() []config.NetworkConfig {
	return []config.NetworkConfig{{
		Driver:    config.NetworkDriverFixture,
		AccountID: "publisher-1",
	}}
}

// configuredThreshold is what the baseline's two threshold keys parse to.
func configuredThreshold() money.Amount {
	return money.Amount{Minor: 2000, Currency: "EUR"}
}

// configuredHouseAccounts is what every enabled want below expects the
// house names to parse to.
func configuredHouseAccounts() config.HouseAccountsConfig {
	return config.HouseAccountsConfig{
		Rounding:          houseRounding,
		Clawback:          houseClawback,
		NetworkReceivable: houseReceivable,
	}
}

func withEnv(base map[string]string, overrides map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overrides {
		if v == "" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	return merged
}

// TestCashbackDefaultsOff pins the shape of an untouched environment: the
// product does not exist, and nothing about it has been decided.
func TestCashbackDefaultsOff(t *testing.T) {
	t.Parallel()

	got, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatalf("FromEnv() error: %v", err)
	}
	// reflect.DeepEqual rather than !=, since Networks made the struct
	// uncomparable. The assertion is the same one: an untouched environment
	// decides nothing, networks included.
	if !reflect.DeepEqual(got.Cashback, config.CashbackConfig{}) {
		t.Fatalf("Cashback = %+v, want the zero value", got.Cashback)
	}
	if got.Cashback.Enabled {
		t.Fatal("cashback is enabled with CASHBACK_ENABLED unset")
	}
}

// TestCashbackFromEnv covers the whole parse: what a complete environment
// produces, every malformed value, and every incomplete one.
func TestCashbackFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want config.CashbackConfig
		// wantErr, when non-empty, is a fragment the error must name, so a
		// refusal is asserted to be the RIGHT refusal.
		wantErr string
	}{
		{
			name: "complete fixture-network environment",
			env:  enabledCashbackEnv(),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				Networks:        configuredNetworks(),
			},
		},
		{
			name: "every key set",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"BLNK_SECRET_KEY":                           "blnk-secret",
				"REDIS_URL":                                 "redis://redis:6379/0",
				"NETWORKS":                                  "reference_network",
				"NETWORK_FIXTURE_ACCOUNT_ID":                "",
				"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID":      "publisher-42",
				"NETWORK_REFERENCE_NETWORK_API_KEY":         "network-key",
				"NETWORK_REFERENCE_NETWORK_API_SECRET":      "network-secret",
				"NETWORK_REFERENCE_NETWORK_SOURCE_LANGUAGE": "DE",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				BlnkSecretKey:   config.NewSecret("blnk-secret"),
				RedisURL:        "redis://redis:6379/0",
				Networks: []config.NetworkConfig{{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
					APISecret: config.NewSecret("network-secret"),
					// Lower-cased from the "DE" the env set: the column this
					// ends up in is keyed by the tag, and a second casing
					// would be a second row nothing matches.
					SourceLanguage: "de",
				}},
			},
		},
		{
			name: "surrounding whitespace is trimmed",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER":                    "  " + config.LedgerDriverMemory + " ",
				"BLNK_URL":                         "",
				"NETWORKS":                         " " + config.NetworkDriverFixture + "  ",
				"NETWORK_FIXTURE_ACCOUNT_ID":       "  publisher-1 ",
				"HOUSE_ACCOUNT_ROUNDING":           "  " + houseRounding + " ",
				"HOUSE_ACCOUNT_CLAWBACK":           " " + houseClawback + "  ",
				"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": "\t" + houseReceivable + " ",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverMemory,
				Networks:        configuredNetworks(),
			},
		},
		{
			name: "the memory ledger needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverMemory,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverMemory,
				Networks:        configuredNetworks(),
			},
		},
		{
			name: "the postgres exit route needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverPostgres,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverPostgres,
				Networks:        configuredNetworks(),
			},
		},
		{
			name: "values are validated even with the product off",
			env: map[string]string{
				"DATABASE_URL":  "postgres://x",
				"LEDGER_DRIVER": "blnkk",
			},
			wantErr: "LEDGER_DRIVER",
		},
		{
			name:    "CASHBACK_ENABLED must be a boolean",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"CASHBACK_ENABLED": "yes please"}),
			wantErr: "CASHBACK_ENABLED",
		},
		{
			name:    "an unknown ledger driver is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"LEDGER_DRIVER": "tigerbeetle"}),
			wantErr: "not a ledger this binary has",
		},
		{
			name:    "an uppercase ledger driver is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"LEDGER_DRIVER": "Blnk"}),
			wantErr: "LEDGER_DRIVER",
		},
		{
			name:    "a network driver with a path separator is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORKS": "networks/fixture"}),
			wantErr: "NETWORKS",
		},
		{
			name:    "a network driver starting with a digit is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORKS": "1network"}),
			wantErr: "NETWORKS",
		},
		{
			name:    "an uppercase network driver is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORKS": "Fixture"}),
			wantErr: "NETWORKS",
		},
		{
			name:    "BLNK_URL with the wrong scheme is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": "redis://blnk:5001"}),
			wantErr: "BLNK_URL must use one of the schemes",
		},
		{
			name:    "BLNK_URL without a host is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": "http:///transactions"}),
			wantErr: "BLNK_URL has no host",
		},
		{
			name:    "an unparseable BLNK_URL is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": "http://blnk:5001/%zz"}),
			wantErr: "BLNK_URL is not a valid URL",
		},
		{
			name: "REDIS_URL with the wrong scheme is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"REDIS_URL": "http://redis:6379",
			}),
			wantErr: "REDIS_URL must use one of the schemes",
		},
		{
			name: "REDIS_URL without a host is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"REDIS_URL": "redis:///0",
			}),
			wantErr: "REDIS_URL has no host",
		},
		{
			name: "rediss is accepted",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"REDIS_URL": "rediss://redis.example.test:6380",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				RedisURL:        "rediss://redis.example.test:6380",
				Networks:        configuredNetworks(),
			},
		},
		{
			name: "BLNK_URL beside another ledger is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverMemory,
			}),
			wantErr: "no posting would ever reach that ledger",
		},
		{
			name: "enabled without a ledger driver is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": "",
				"BLNK_URL":      "",
			}),
			wantErr: "LEDGER_DRIVER",
		},
		{
			// It used to be refused, and the founder's decision of
			// 2026-09-04 is that it should not be: a deployment that names
			// no network still serves the wallet, the money loop and
			// click-outs on the catalogue it already has. What it does not
			// do is poll, and the composition root says so at ERROR.
			name: "enabled with no network at all is accepted",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORKS":                   "",
				"NETWORK_FIXTURE_ACCOUNT_ID": "",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
			},
		},
		{
			name:    "the blnk ledger without an endpoint is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": ""}),
			wantErr: "BLNK_URL",
		},
		{
			// PARSED, not refused, and this is the change T215 makes: an
			// incomplete network no longer fails config, because failing
			// config kills the process and would take the news site down
			// over a typo in one network's key. It comes back in the slice
			// and is reported by name - see TestUnusableNetworks - and
			// CashbackConfig.Mountable is what declines to run cashback
			// over it.
			name: "a live network with no credentials at all still parses",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORKS":                   "reference_network",
				"NETWORK_FIXTURE_ACCOUNT_ID": "",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				Networks:        []config.NetworkConfig{{Driver: "reference_network"}},
			},
		},
		{
			name: "a live network needs no API secret",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORKS":                             "reference_network",
				"NETWORK_FIXTURE_ACCOUNT_ID":           "",
				"NETWORK_REFERENCE_NETWORK_ACCOUNT_ID": "publisher-42",
				"NETWORK_REFERENCE_NETWORK_API_KEY":    "network-key",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				HouseAccounts:   configuredHouseAccounts(),
				PayoutThreshold: configuredThreshold(),
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				Networks: []config.NetworkConfig{{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
				}},
			},
		},
		{
			// The documented no-Docker loop and the CI jobs enable the
			// product with four keys and no house names, and spike S3
			// holds that environment complete (ADR-0002). The names are
			// demanded where money is real instead - see
			// TestCashbackProductionRefusals.
			name: "enabled without house names is accepted outside production",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"HOUSE_ACCOUNT_ROUNDING":           "",
				"HOUSE_ACCOUNT_CLAWBACK":           "",
				"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": "",
			}),
			want: config.CashbackConfig{
				Enabled:         true,
				LedgerDriver:    config.LedgerDriverBlnk,
				BlnkURL:         "http://blnk:5001",
				Networks:        configuredNetworks(),
				PayoutThreshold: configuredThreshold(),
			},
		},
		{
			// The two pairings the third purpose added. Both are asserted
			// because the pair a comparison forgets is exactly the pair that
			// goes unchecked, and neither of these existed to be forgotten
			// until the receivable did.
			name: "the receivable sharing the rounding account is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": houseRounding,
			}),
			wantErr: "HOUSE_ACCOUNT_ROUNDING and HOUSE_ACCOUNT_NETWORK_RECEIVABLE both name",
		},
		{
			name: "the receivable sharing the clawback account is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": houseClawback,
			}),
			wantErr: "HOUSE_ACCOUNT_CLAWBACK and HOUSE_ACCOUNT_NETWORK_RECEIVABLE both name",
		},
		{
			name: "two house purposes on one name are refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"HOUSE_ACCOUNT_CLAWBACK": houseRounding,
			}),
			wantErr: "HOUSE_ACCOUNT_ROUNDING and HOUSE_ACCOUNT_CLAWBACK both name",
		},
		{
			name: "a shared house name is refused even with the product off",
			env: map[string]string{
				"DATABASE_URL":           "postgres://x",
				"HOUSE_ACCOUNT_ROUNDING": "house-shared",
				"HOUSE_ACCOUNT_CLAWBACK": "house-shared",
			},
			wantErr: "HOUSE_ACCOUNT_ROUNDING and HOUSE_ACCOUNT_CLAWBACK both name",
		},
		{
			name: "an incomplete environment is fine with the product off",
			env: map[string]string{
				"DATABASE_URL":     "postgres://x",
				"CASHBACK_ENABLED": "false",
				"LEDGER_DRIVER":    config.LedgerDriverBlnk,
			},
			want: config.CashbackConfig{LedgerDriver: config.LedgerDriverBlnk},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.FromEnv(envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("FromEnv() = %+v, want an error naming %q", got.Cashback, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not name %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv() error: %v", err)
			}
			if !reflect.DeepEqual(got.Cashback, tt.want) {
				t.Fatalf("Cashback = %+v, want %+v", got.Cashback, tt.want)
			}
		})
	}
}

// TestCashbackProductionRefusals pins the rules that only bite in
// production. All are money rules: a ledger nobody has to authenticate to,
// a ledger that forgets everything when the process restarts, and a
// deployment that cannot name the house accounts its first commission
// will post through.
func TestCashbackProductionRefusals(t *testing.T) {
	t.Parallel()

	prod := func(overrides map[string]string) map[string]string {
		return withEnv(withEnv(enabledCashbackEnv(), map[string]string{
			"DATABASE_URL": "postgres://y?sslmode=verify-full",
			"APP_ENV":      config.EnvProd,
		}), overrides)
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "an unauthenticated ledger is refused in production",
			env:     prod(nil),
			wantErr: "BLNK_SECRET_KEY is unset",
		},
		{
			name: "the in-process ledger is refused in production",
			env: prod(map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverMemory,
				"BLNK_URL":      "",
			}),
			wantErr: "persists nothing",
		},
		{
			name:    "an authenticated blnk ledger is accepted in production",
			env:     prod(map[string]string{"BLNK_SECRET_KEY": "blnk-secret"}),
			wantErr: "",
		},
		{
			name: "missing house accounts are refused in production",
			env: prod(map[string]string{
				"BLNK_SECRET_KEY":                  "blnk-secret",
				"HOUSE_ACCOUNT_ROUNDING":           "",
				"HOUSE_ACCOUNT_CLAWBACK":           "",
				"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": "",
			}),
			wantErr: "HOUSE_ACCOUNT_ROUNDING, HOUSE_ACCOUNT_CLAWBACK, HOUSE_ACCOUNT_NETWORK_RECEIVABLE are unset",
		},
		{
			name: "one missing house account is refused in production naming its key",
			env: prod(map[string]string{
				"BLNK_SECRET_KEY":        "blnk-secret",
				"HOUSE_ACCOUNT_CLAWBACK": "",
			}),
			wantErr: "HOUSE_ACCOUNT_CLAWBACK is unset",
		},
		{
			name: "the postgres exit route is accepted in production",
			env: prod(map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverPostgres,
				"BLNK_URL":      "",
			}),
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.FromEnv(envFrom(tt.env))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("FromEnv() error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not name %q", err, tt.wantErr)
			}
		})
	}
}

// TestCashbackProductionRulesDoNotApplyInDev keeps the production rules from
// leaking into the shape the founder actually runs locally: no Docker, the
// in-process ledger, no credentials anywhere.
func TestCashbackProductionRulesDoNotApplyInDev(t *testing.T) {
	t.Parallel()

	got, err := config.FromEnv(envFrom(map[string]string{
		"DATABASE_URL":                     "postgres://x?sslmode=disable",
		"CASHBACK_ENABLED":                 "true",
		"LEDGER_DRIVER":                    config.LedgerDriverMemory,
		"NETWORKS":                         config.NetworkDriverFixture,
		"NETWORK_FIXTURE_ACCOUNT_ID":       "publisher-1",
		"HOUSE_ACCOUNT_ROUNDING":           houseRounding,
		"HOUSE_ACCOUNT_CLAWBACK":           houseClawback,
		"HOUSE_ACCOUNT_NETWORK_RECEIVABLE": houseReceivable,
	}))
	if err != nil {
		t.Fatalf("FromEnv() error: %v", err)
	}
	if got.Cashback.LedgerDriver != config.LedgerDriverMemory {
		t.Fatalf("LedgerDriver = %q, want %q", got.Cashback.LedgerDriver, config.LedgerDriverMemory)
	}
}

// TestEndpointErrorsNeverEchoTheValue holds validateEndpoint to its own doc
// comment. Startup errors go to stderr, which on this deployment is a
// container log somebody keeps, so a refusal that quotes the value it was
// given puts a credential there.
//
// The scheme is the trap: it looks like our own vocabulary, but url.Parse
// reads it out of the raw string, and a value that is not a URL at all still
// yields one - "hunter2-a-real-credential:x" parses with that whole first
// segment as the scheme.
func TestEndpointErrorsNeverEchoTheValue(t *testing.T) {
	t.Parallel()

	const credential = "hunter2-a-real-credential"

	tests := []struct {
		name string
		key  string
		env  map[string]string
	}{
		{
			name: "a credential pasted into BLNK_URL",
			key:  "BLNK_URL",
			env:  withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": credential + ":whatever"}),
		},
		{
			name: "a credential pasted into REDIS_URL",
			key:  "REDIS_URL",
			env:  withEnv(enabledCashbackEnv(), map[string]string{"REDIS_URL": credential + ":whatever"}),
		},
		{
			name: "a password inside an otherwise valid URL of the wrong scheme",
			key:  "REDIS_URL",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"REDIS_URL": "http://apivo:" + credential + "@redis:6379",
			}),
		},
		{
			name: "an unparseable value",
			key:  "BLNK_URL",
			env:  withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": "http://" + credential + "/%zz"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.FromEnv(envFrom(tt.env))
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			if strings.Contains(err.Error(), credential) {
				t.Fatalf("the refusal echoed the value: %v", err)
			}
			// Still useful: it must name the key, or an operator cannot
			// act on it.
			if !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("the refusal does not name %s: %v", tt.key, err)
			}
		})
	}
}

func TestCashbackMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.CashbackConfig
		want []string
	}{
		{
			name: "nothing set",
			cfg:  config.CashbackConfig{Enabled: true},
			want: []string{"LEDGER_DRIVER"},
		},
		{
			name: "blnk without an endpoint",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				Networks:     configuredNetworks(),
			},
			want: []string{"BLNK_URL"},
		},
		{
			// The assertion that matters most here, because it is the one
			// that changed: a network with nothing to authenticate with is
			// NOT in Missing(). Missing() fails config parsing, which stops
			// the whole process - the news site with it. A network that
			// cannot poll stops cashback and nothing else, which is
			// CashbackConfig.Mountable's job, asserted in TestMountable.
			name: "a live network with nothing to authenticate with is not missing",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Networks:     []config.NetworkConfig{{Driver: "reference_network"}},
			},
			want: nil,
		},
		{
			// Nor does naming no network at all make anything missing.
			name: "no network at all is not missing either",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
			},
			want: nil,
		},
		{
			// No house names and still complete: the keys are a
			// production startup rule, not part of what Missing() gates
			// mounting on - spike S3 pins the four-key environment.
			name: "complete",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Networks:     configuredNetworks(),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.cfg.Missing()
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("Missing() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetworkConfigNeedsCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		driver string
		want   bool
	}{
		{name: "unset", driver: "", want: false},
		{name: "fixture", driver: config.NetworkDriverFixture, want: false},
		{name: "a real network", driver: "reference_network", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := config.NetworkConfig{Driver: tt.driver}.NeedsCredentials()
			if got != tt.want {
				t.Fatalf("NeedsCredentials() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCashbackUsesBlnk(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		driver string
		want   bool
	}{
		{name: "unset", driver: "", want: false},
		{name: "blnk", driver: config.LedgerDriverBlnk, want: true},
		{name: "memory", driver: config.LedgerDriverMemory, want: false},
		{name: "postgres", driver: config.LedgerDriverPostgres, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := config.CashbackConfig{LedgerDriver: tt.driver}.UsesBlnk()
			if got != tt.want {
				t.Fatalf("UsesBlnk() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCashbackLogValue is the redaction gate: the whole struct, logged the
// obvious way, must not put a credential in the log - including the password
// inside a Redis URL, which no attribute name would have given away.
func TestCashbackLogValue(t *testing.T) {
	t.Parallel()

	cfg := config.CashbackConfig{
		Enabled:         true,
		HouseAccounts:   configuredHouseAccounts(),
		PayoutThreshold: configuredThreshold(),
		LedgerDriver:    config.LedgerDriverBlnk,
		BlnkURL:         "http://blnk:5001",
		BlnkSecretKey:   config.NewSecret("blnk-secret-value"),
		RedisURL:        "redis://apivo:redis-password-value@redis:6379/0",
		Networks: []config.NetworkConfig{
			{
				Driver:    "reference_network",
				AccountID: "publisher-42",
				APIKey:    config.NewSecret("network-key-value"),
				APISecret: config.NewSecret("network-secret-value"),
			},
			// A second network, because the redaction that only holds for
			// the first entry is the one a single-network case cannot see.
			{
				Driver:    "second_network",
				AccountID: "publisher-99",
				APIKey:    config.NewSecret("second-key-value"),
			},
		},
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cashback", "cashback", cfg)
	out := buf.String()

	for _, secret := range []string{
		"blnk-secret-value",
		"redis-password-value",
		"network-key-value",
		"network-secret-value",
		"second-key-value",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q reached the log: %s", secret, out)
		}
	}
	for _, want := range []string{
		config.LedgerDriverBlnk,
		"http://blnk:5001",
		"reference_network",
		"publisher-42",
		"second_network",
		"publisher-99",
		// The house names are pinned as key:value pairs rather than bare
		// fragments: an operator debugging a house misconfiguration reads
		// the label, and a line with the two values swapped across the
		// labels would still contain both bare fragments.
		`"house_account_rounding":"` + houseRounding + `"`,
		`"house_account_clawback":"` + houseClawback + `"`,
		"blnk_secret_key_set",
		"redis://apivo:xxxxx@redis:6379/0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the log line: %s", want, out)
		}
	}
}

// TestCashbackLogValueOnAnUnparseableURL covers the failing-open hazard: the
// logging path must withhold a value it cannot parse rather than print it.
func TestCashbackLogValueOnAnUnparseableURL(t *testing.T) {
	t.Parallel()

	cfg := config.CashbackConfig{RedisURL: "redis://apivo:pw@redis:6379/%zz"}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cashback", "cashback", cfg)
	out := buf.String()

	if strings.Contains(out, "pw@redis") {
		t.Fatalf("an unparseable URL was printed whole: %s", out)
	}
	if !strings.Contains(out, config.RedactedPlaceholder) {
		t.Fatalf("expected %q in the log line: %s", config.RedactedPlaceholder, out)
	}
}

// TestClickContextHeaderIsOptionalAndTrimmed covers the one key whose
// DEFAULT is the safe answer rather than the missing one.
//
// Empty means the deployment names no header it trusts, and the click rule
// then leaves its per-device half off rather than bracketing every member
// behind a proxy. So "unset" must parse to empty and never to something a
// caller could have set - which is why the value is trimmed and why a header
// name of nothing but spaces is the same as none.
func TestClickContextHeaderIsOptionalAndTrimmed(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, raw, want string }{
		{name: "unset is no header"},
		{name: "spaces are no header", raw: "  "},
		{name: "whitespace is no header", raw: "\t\n "},
		{name: "a header name is kept", raw: "X-Client-IP", want: "X-Client-IP"},
		{name: "a padded name is trimmed", raw: "  X-Forwarded-For  ", want: "X-Forwarded-For"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := withEnv(enabledCashbackEnv(), map[string]string{"CLICK_CONTEXT_HEADER": tc.raw})
			cfg, err := config.FromEnv(func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("FromEnv(): %v", err)
			}
			if got := cfg.Cashback.ClickContextHeader; got != tc.want {
				t.Errorf("ClickContextHeader = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestThePayoutThresholdIsOneValueInTwoKeys. An amount with no currency is a
// number somebody compares a balance against and gets right until a second
// currency is published (C-6), so half a threshold is refused in every
// environment rather than only where money is real.
func TestThePayoutThresholdIsOneValueInTwoKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "an amount with no currency is refused",
			env:     map[string]string{"PAYOUT_THRESHOLD_CURRENCY": ""},
			wantErr: "PAYOUT_THRESHOLD_CURRENCY is not",
		},
		{
			name:    "a currency with no amount is refused",
			env:     map[string]string{"PAYOUT_THRESHOLD_MINOR": ""},
			wantErr: "PAYOUT_THRESHOLD_MINOR is not",
		},
		{
			name:    "an amount that is not a whole number is refused",
			env:     map[string]string{"PAYOUT_THRESHOLD_MINOR": "20.00"},
			wantErr: "not a whole number of minor units",
		},
		{
			// Not "invalid": a negative threshold is a figure no balance
			// could fail to reach, which is a different mistake from a
			// malformed one and reads better as itself.
			name:    "a negative threshold is refused",
			env:     map[string]string{"PAYOUT_THRESHOLD_MINOR": "-1"},
			wantErr: "no balance can fail to reach it",
		},
		{
			name:    "a currency that is not ISO 4217 is refused",
			env:     map[string]string{"PAYOUT_THRESHOLD_CURRENCY": "euro"},
			wantErr: "PAYOUT_THRESHOLD_CURRENCY",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.FromEnv(envFrom(withEnv(enabledCashbackEnv(), tc.env)))
			if err == nil {
				t.Fatalf("FromEnv() accepted %v", tc.env)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestAThresholdOfNothingIsAThreshold. Zero means any confirmed balance may
// be withdrawn, which is a deployment's decision to make - so the pair being
// set is what says one was configured, never the number being non-zero.
func TestAThresholdOfNothingIsAThreshold(t *testing.T) {
	t.Parallel()

	got, err := config.FromEnv(envFrom(withEnv(enabledCashbackEnv(), map[string]string{
		"PAYOUT_THRESHOLD_MINOR": "0",
	})))
	if err != nil {
		t.Fatalf("FromEnv(): %v", err)
	}
	want := money.Amount{Minor: 0, Currency: "EUR"}
	if got.Cashback.PayoutThreshold != want {
		t.Errorf("PayoutThreshold = %v, want %v", got.Cashback.PayoutThreshold, want)
	}
}

// TestProductionNeedsAThreshold, discovered at startup rather than by the
// first member who asks to be paid.
func TestProductionNeedsAThreshold(t *testing.T) {
	t.Parallel()

	_, err := config.FromEnv(envFrom(withEnv(enabledCashbackEnv(), map[string]string{
		"DATABASE_URL":              "postgres://y?sslmode=verify-full",
		"APP_ENV":                   config.EnvProd,
		"BLNK_SECRET_KEY":           "a-secret",
		"PAYOUT_THRESHOLD_MINOR":    "",
		"PAYOUT_THRESHOLD_CURRENCY": "",
	})))

	if err == nil {
		t.Fatal("FromEnv() started a production deployment with no withdrawal threshold")
	}
	if !strings.Contains(err.Error(), "PAYOUT_THRESHOLD_MINOR and PAYOUT_THRESHOLD_CURRENCY are unset") {
		t.Errorf("error %q does not name the two keys", err)
	}
}

// TestTheHoldRulesAreReadFromTheEnvironment. Every rule is off until its
// keys are set (US7, T118); set, each is read as the count, window, age or
// amount it is.
func TestTheHoldRulesAreReadFromTheEnvironment(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv(envFrom(withEnv(enabledCashbackEnv(), map[string]string{
		"HOLD_SHARED_CONTEXT_ACCOUNTS": "3",
		"HOLD_SHARED_CONTEXT_WINDOW":   "720h",
		"HOLD_NEW_ACCOUNT_AGE":         "24h",
		"HOLD_SALE_CAP_MINOR":          "50000",
		"HOLD_SALE_CAP_CURRENCY":       "EUR",
		"HOLD_MEMBER_VELOCITY":         "5",
		"HOLD_MEMBER_VELOCITY_WINDOW":  "24h",
	})))
	if err != nil {
		t.Fatalf("FromEnv(): %v", err)
	}
	h := cfg.Cashback.HoldRules
	switch {
	case h.SharedContextAccounts != 3 || h.SharedContextWindow != 720*time.Hour:
		t.Errorf("shared context = %d in %s, want 3 in 720h", h.SharedContextAccounts, h.SharedContextWindow)
	case h.NewAccountAge != 24*time.Hour:
		t.Errorf("new account age = %s, want 24h", h.NewAccountAge)
	case h.SaleCap.Minor != 50000 || h.SaleCap.Currency != "EUR":
		t.Errorf("sale cap = %s, want 50000 EUR", h.SaleCap)
	case h.MemberVelocity != 5 || h.MemberVelocityWindow != 24*time.Hour:
		t.Errorf("velocity = %d in %s, want 5 in 24h", h.MemberVelocity, h.MemberVelocityWindow)
	}

	off, err := config.FromEnv(envFrom(enabledCashbackEnv()))
	if err != nil {
		t.Fatalf("FromEnv() with no hold keys: %v", err)
	}
	if off.Cashback.HoldRules != (config.HoldRulesConfig{}) {
		t.Errorf("with no HOLD_* keys the rules are %+v, want all off", off.Cashback.HoldRules)
	}
}

// TestTheHoldRulesAreRefusedHalfConfigured. A count without its window holds
// nothing or everything depending on which half is missing, so half a rule
// is refused in every environment rather than run as whatever it defaults
// to.
func TestTheHoldRulesAreRefusedHalfConfigured(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"a device count without its window", map[string]string{"HOLD_SHARED_CONTEXT_ACCOUNTS": "3"}, "set both or neither"},
		{"a device window without its count", map[string]string{"HOLD_SHARED_CONTEXT_WINDOW": "720h"}, "set both or neither"},
		{"a device count of one", map[string]string{"HOLD_SHARED_CONTEXT_ACCOUNTS": "1", "HOLD_SHARED_CONTEXT_WINDOW": "720h"}, "at least 2"},
		{"a velocity without its window", map[string]string{"HOLD_MEMBER_VELOCITY": "5"}, "set both or neither"},
		{"a count that is not a number", map[string]string{"HOLD_MEMBER_VELOCITY": "five", "HOLD_MEMBER_VELOCITY_WINDOW": "24h"}, "not a whole number"},
		{"a count of nothing", map[string]string{"HOLD_MEMBER_VELOCITY": "0", "HOLD_MEMBER_VELOCITY_WINDOW": "24h"}, "leave it unset"},
		{"a window in days", map[string]string{"HOLD_NEW_ACCOUNT_AGE": "30d"}, "not a duration such as 24h or 720h"},
		{"a window of nothing", map[string]string{"HOLD_NEW_ACCOUNT_AGE": "0s"}, "leave it unset"},
		{"a cap with no currency", map[string]string{"HOLD_SALE_CAP_MINOR": "50000"}, "HOLD_SALE_CAP_MINOR is set"},
		{"a currency with no cap", map[string]string{"HOLD_SALE_CAP_CURRENCY": "EUR"}, "HOLD_SALE_CAP_CURRENCY is set"},
		{"a cap of nothing", map[string]string{"HOLD_SALE_CAP_MINOR": "0", "HOLD_SALE_CAP_CURRENCY": "EUR"}, "not a cap"},
		{"a cap in a currency that is not one", map[string]string{"HOLD_SALE_CAP_MINOR": "1", "HOLD_SALE_CAP_CURRENCY": "euro"}, "HOLD_SALE_CAP_CURRENCY"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.FromEnv(envFrom(withEnv(enabledCashbackEnv(), tc.env)))
			if err == nil {
				t.Fatalf("FromEnv() accepted %v", tc.env)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
