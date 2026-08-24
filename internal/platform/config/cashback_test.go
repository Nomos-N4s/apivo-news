package config_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// enabledCashbackEnv is the smallest environment that fully configures
// cashback: the product on, a ledger, a network adapter that needs no
// credentials, and - because the ledger is the sidecar - where it lives.
func enabledCashbackEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":     "postgres://x",
		"CASHBACK_ENABLED": "true",
		"LEDGER_DRIVER":    config.LedgerDriverBlnk,
		"BLNK_URL":         "http://blnk:5001",
		"NETWORK_DRIVER":   config.NetworkDriverFixture,
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
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				BlnkURL:      "http://blnk:5001",
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
				"LEDGER_DRIVER":  "  " + config.LedgerDriverMemory + " ",
				"BLNK_URL":       "",
				"NETWORK_DRIVER": " " + config.NetworkDriverFixture + "  ",
			}),
			want: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
		},
		{
			name: "the memory ledger needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverMemory,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverMemory,
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
			},
		},
		{
			name: "the postgres exit route needs no endpoint",
			env: withEnv(enabledCashbackEnv(), map[string]string{
				"LEDGER_DRIVER": config.LedgerDriverPostgres,
				"BLNK_URL":      "",
			}),
			want: config.CashbackConfig{
				Enabled:      true,
				LedgerDriver: config.LedgerDriverPostgres,
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				BlnkURL:      "http://blnk:5001",
				RedisURL:     "rediss://redis.example.test:6380",
				Network:      config.NetworkConfig{Driver: config.NetworkDriverFixture},
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
				Enabled:      true,
				LedgerDriver: config.LedgerDriverBlnk,
				BlnkURL:      "http://blnk:5001",
				Network: config.NetworkConfig{
					Driver:    "reference_network",
					AccountID: "publisher-42",
					APIKey:    config.NewSecret("network-key"),
				},
			},
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

// TestCashbackProductionRefusals pins the two rules that only bite in
// production. Both are money rules: a ledger nobody has to authenticate to,
// and a ledger that forgets everything when the process restarts.
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
		"DATABASE_URL":     "postgres://x?sslmode=disable",
		"CASHBACK_ENABLED": "true",
		"LEDGER_DRIVER":    config.LedgerDriverMemory,
		"NETWORK_DRIVER":   config.NetworkDriverFixture,
	}))
	if err != nil {
		t.Fatalf("FromEnv() error: %v", err)
	}
	if got.Cashback.LedgerDriver != config.LedgerDriverMemory {
		t.Fatalf("LedgerDriver = %q, want %q", got.Cashback.LedgerDriver, config.LedgerDriverMemory)
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
