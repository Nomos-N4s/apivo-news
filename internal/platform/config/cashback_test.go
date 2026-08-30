package config_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// The house account names enabledCashbackEnv configures, held as constants
// so the envs and the wants below cannot drift apart by a typo.
const (
	houseRounding = "rounding-remainder"
	houseClawback = "clawback-loss"
)

// enabledCashbackEnv fully configures cashback: the product on, a ledger,
// a network adapter that needs no credentials, the two house accounts
// money is routed through (optional outside production, but a complete
// environment is the honest baseline for the wants below), and - because
// the ledger is the sidecar - where it lives.
func enabledCashbackEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":           "postgres://x",
		"CASHBACK_ENABLED":       "true",
		"LEDGER_DRIVER":          config.LedgerDriverBlnk,
		"BLNK_URL":               "http://blnk:5001",
		"NETWORK_DRIVER":         config.NetworkDriverFixture,
		"HOUSE_ACCOUNT_ROUNDING": houseRounding,
		"HOUSE_ACCOUNT_CLAWBACK": houseClawback,
	}
}

// configuredHouseAccounts is what every enabled want below expects the
// house names to parse to.
func configuredHouseAccounts() config.HouseAccountsConfig {
	return config.HouseAccountsConfig{Rounding: houseRounding, Clawback: houseClawback}
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
	if got.Cashback != (config.CashbackConfig{}) {
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
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverBlnk,
				BlnkURL:       "http://blnk:5001",
				Network:       config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
		},
		{
			name: "every key set",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"BLNK_SECRET_KEY":    "blnk-secret",
				"REDIS_URL":          "redis://redis:6379/0",
				"NETWORK_DRIVER":     "reference_network",
				"NETWORK_ACCOUNT_ID": "publisher-42",
				"NETWORK_API_KEY":    "network-key",
				"NETWORK_API_SECRET": "network-secret",
			}),
			want: config.CashbackConfig{
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverBlnk,
				BlnkURL:       "http://blnk:5001",
				BlnkSecretKey: config.NewSecret("blnk-secret"),
				RedisURL:      "redis://redis:6379/0",
				Network: config.NetworkConfig{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
					APISecret: config.NewSecret("network-secret"),
				},
			},
		},
		{
			name: "surrounding whitespace is trimmed",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER":          "  " + config.LedgerDriverMemory + " ",
				"BLNK_URL":               "",
				"NETWORK_DRIVER":         " " + config.NetworkDriverFixture + "  ",
				"HOUSE_ACCOUNT_ROUNDING": "  " + houseRounding + " ",
				"HOUSE_ACCOUNT_CLAWBACK": " " + houseClawback + "  ",
			}),
			want: config.CashbackConfig{
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverMemory,
				Network:       config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
		},
		{
			name: "the memory ledger needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverMemory,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverMemory,
				Network:       config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
		},
		{
			name: "the postgres exit route needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverPostgres,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverPostgres,
				Network:       config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORK_DRIVER": "networks/fixture"}),
			wantErr: "NETWORK_DRIVER",
		},
		{
			name:    "a network driver starting with a digit is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORK_DRIVER": "1network"}),
			wantErr: "NETWORK_DRIVER",
		},
		{
			name:    "an uppercase network driver is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORK_DRIVER": "Fixture"}),
			wantErr: "NETWORK_DRIVER",
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
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverBlnk,
				BlnkURL:       "http://blnk:5001",
				RedisURL:      "rediss://redis.example.test:6380",
				Network:       config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
			name:    "enabled without a network driver is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"NETWORK_DRIVER": ""}),
			wantErr: "NETWORK_DRIVER",
		},
		{
			name:    "the blnk ledger without an endpoint is refused",
			env:     withEnv(enabledCashbackEnv(), map[string]string{"BLNK_URL": ""}),
			wantErr: "BLNK_URL",
		},
		{
			name: "a live network without credentials is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORK_DRIVER": "reference_network",
			}),
			wantErr: "NETWORK_ACCOUNT_ID, NETWORK_API_KEY are unset",
		},
		{
			name: "a live network without its key is refused",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORK_DRIVER":     "reference_network",
				"NETWORK_ACCOUNT_ID": "publisher-42",
			}),
			wantErr: "NETWORK_API_KEY is unset",
		},
		{
			name: "a live network needs no API secret",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"NETWORK_DRIVER":     "reference_network",
				"NETWORK_ACCOUNT_ID": "publisher-42",
				"NETWORK_API_KEY":    "network-key",
			}),
			want: config.CashbackConfig{
				Enabled:       true,
				HouseAccounts: configuredHouseAccounts(),
				LedgerDriver:  config.LedgerDriverBlnk,
				BlnkURL:       "http://blnk:5001",
				Network: config.NetworkConfig{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
				},
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
				"HOUSE_ACCOUNT_ROUNDING": "",
				"HOUSE_ACCOUNT_CLAWBACK": "",
			}),
			want: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				BlnkURL:      "http://blnk:5001",
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
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
			if got.Cashback != tt.want {
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
				"BLNK_SECRET_KEY":        "blnk-secret",
				"HOUSE_ACCOUNT_ROUNDING": "",
				"HOUSE_ACCOUNT_CLAWBACK": "",
			}),
			wantErr: "HOUSE_ACCOUNT_ROUNDING, HOUSE_ACCOUNT_CLAWBACK are unset",
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
		"DATABASE_URL":           "postgres://x?sslmode=disable",
		"CASHBACK_ENABLED":       "true",
		"LEDGER_DRIVER":          config.LedgerDriverMemory,
		"NETWORK_DRIVER":         config.NetworkDriverFixture,
		"HOUSE_ACCOUNT_ROUNDING": houseRounding,
		"HOUSE_ACCOUNT_CLAWBACK": houseClawback,
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
			want: []string{"LEDGER_DRIVER", "NETWORK_DRIVER"},
		},
		{
			name: "blnk without an endpoint",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
			want: []string{"BLNK_URL"},
		},
		{
			name: "a live network with nothing to authenticate with",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Network:      config.NetworkConfig{Driver: "reference_network"},
			},
			want: []string{"NETWORK_ACCOUNT_ID", "NETWORK_API_KEY"},
		},
		{
			// No house names and still complete: the keys are a
			// production startup rule, not part of what Missing() gates
			// mounting on - spike S3 pins the four-key environment.
			name: "complete",
			cfg: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
		Enabled:       true,
		HouseAccounts: configuredHouseAccounts(),
		LedgerDriver:  config.LedgerDriverBlnk,
		BlnkURL:       "http://blnk:5001",
		BlnkSecretKey: config.NewSecret("blnk-secret-value"),
		RedisURL:      "redis://apivo:redis-password-value@redis:6379/0",
		Network: config.NetworkConfig{
			Driver:    "reference_network",
			AccountID: "publisher-42",
			APIKey:    config.NewSecret("network-key-value"),
			APISecret: config.NewSecret("network-secret-value"),
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
