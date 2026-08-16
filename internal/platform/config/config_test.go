package config_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// defaultTranslation is what an untouched TRANSLATION_* environment
// parses to: nothing configured, the interval at its default - and the
// pipeline therefore off, because the interval alone decides nothing.
var defaultTranslation = config.TranslationConfig{Interval: config.DefaultTranslationInterval}

func TestFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    config.Config
		wantErr bool
	}{
		{
			name: "defaults applied",
			env:  map[string]string{"DATABASE_URL": "postgres://x"},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelInfo,
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "all values set",
			env: map[string]string{
				"DATABASE_URL": "postgres://y",
				"HTTP_ADDR":    ":9999",
				"APP_ENV":      config.EnvProd,
				"LOG_LEVEL":    "debug",
			},
			want: config.Config{
				DatabaseURL:  "postgres://y",
				HTTPAddr:     ":9999",
				Env:          config.EnvProd,
				LogLevel:     slog.LevelDebug,
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "warn level parsed",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "WARN"},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelWarn,
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "error level parsed",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "error"},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelError,
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "JWKS URL and audience carried through",
			env: map[string]string{
				"DATABASE_URL": "postgres://x",
				"JWKS_URL":     "https://auth.example.test/jwks.json",
				"JWT_AUDIENCE": "authenticated",
			},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelInfo,
				JWKSURL:      "https://auth.example.test/jwks.json",
				JWTAudience:  "authenticated",
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "JWKS URL without audience is fine",
			env: map[string]string{
				"DATABASE_URL": "postgres://x",
				"JWKS_URL":     "https://auth.example.test/jwks.json",
			},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelInfo,
				JWKSURL:      "https://auth.example.test/jwks.json",
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			// The deploy configs ship both keys present but empty, so that
			// "not configured yet" has exactly one representation and no
			// placeholder can fake a configuration. Empty must therefore
			// mean unconfigured, not an audience-without-endpoint error.
			name: "explicitly empty JWKS URL and audience mean unconfigured",
			env: map[string]string{
				"DATABASE_URL": "postgres://x",
				"JWKS_URL":     "",
				"JWT_AUDIENCE": "",
			},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelInfo,
				PollInterval: config.DefaultPollInterval,
				Translation:  defaultTranslation,
			},
		},
		{
			name: "poll interval parsed",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "POLL_INTERVAL": "90s"},
			want: config.Config{
				DatabaseURL:  "postgres://x",
				HTTPAddr:     ":8080",
				Env:          config.EnvDev,
				LogLevel:     slog.LevelInfo,
				PollInterval: 90 * time.Second,
				Translation:  defaultTranslation,
			},
		},
		{
			// POLL_INTERVAL=0 is the one disable switch: zero interval, no
			// poll loop. There is deliberately no POLL_ENABLED beside it.
			name: "poll interval zero disables polling",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "POLL_INTERVAL": "0"},
			want: config.Config{
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelInfo,
				Translation: defaultTranslation,
			},
		},
		{
			name:    "non-duration POLL_INTERVAL rejected",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "POLL_INTERVAL": "often"},
			wantErr: true,
		},
		{
			name:    "negative POLL_INTERVAL rejected",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "POLL_INTERVAL": "-5m"},
			wantErr: true,
		},
		{
			name:    "missing DATABASE_URL rejected",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "audience without JWKS URL rejected",
			env: map[string]string{
				"DATABASE_URL": "postgres://x",
				"JWT_AUDIENCE": "authenticated",
			},
			wantErr: true,
		},
		{
			name:    "unknown APP_ENV rejected",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "APP_ENV": "staging"},
			wantErr: true,
		},
		{
			name:    "unknown LOG_LEVEL rejected",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "loud"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.FromEnv(envFrom(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("FromEnv() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("FromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// completeTranslationEnv is the smallest environment that fully configures
// the translation pipeline: a priced provider and both budget caps.
func completeTranslationEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":                         "postgres://x",
		"TRANSLATION_BASE_URL":                 "https://api.provider.example.test/v1",
		"TRANSLATION_MODEL":                    "test-model-1",
		"TRANSLATION_API_KEY":                  "test-key",
		"TRANSLATION_INPUT_USD_PER_MTOK":       "0.05",
		"TRANSLATION_OUTPUT_USD_PER_MTOK":      "0.08",
		"TRANSLATION_ARTICLE_CEILING_MICROUSD": "20000",
		"TRANSLATION_MONTHLY_CAP_MICROUSD":     "25000000",
	}
}

// TestTranslationConfigGate pins the no-defaults stance: nothing picks a
// provider or a budget by accident. A wholly unset environment is the
// documented off state, a partial one is a mistake the composition root
// names, and only a complete one - or "0" on the one switch that has a
// default - decides anything.
func TestTranslationConfigGate(t *testing.T) {
	t.Parallel()

	t.Run("unset means off, and recognisably untouched", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x"}))
		if err != nil {
			t.Fatalf("FromEnv() error: %v", err)
		}
		tr := cfg.Translation
		if tr.Enabled() {
			t.Error("Enabled() = true with nothing configured: the pipeline picked a provider by accident")
		}
		if tr.AnySet() {
			t.Error("AnySet() = true with nothing configured")
		}
		if tr.Interval != config.DefaultTranslationInterval {
			t.Errorf("Interval = %s, want the %s default: the interval alone decides nothing", tr.Interval, config.DefaultTranslationInterval)
		}
		if len(tr.Missing()) == 0 {
			t.Error("Missing() is empty with nothing configured; the startup log line would name no keys")
		}
	})

	t.Run("complete configuration enables the pipeline", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.FromEnv(envFrom(completeTranslationEnv()))
		if err != nil {
			t.Fatalf("FromEnv() error: %v", err)
		}
		want := config.TranslationConfig{
			BaseURL:                "https://api.provider.example.test/v1",
			Model:                  "test-model-1",
			APIKey:                 "test-key",
			InputUSDPerMTok:        0.05,
			OutputUSDPerMTok:       0.08,
			Interval:               config.DefaultTranslationInterval,
			ArticleCeilingMicroUSD: 20_000,
			MonthlyCapMicroUSD:     25_000_000,
		}
		if cfg.Translation != want {
			t.Fatalf("Translation = %+v, want %+v", cfg.Translation, want)
		}
		if !cfg.Translation.Enabled() {
			t.Error("Enabled() = false for a complete configuration")
		}
	})

	t.Run("free of charge stands in for both price lines", func(t *testing.T) {
		t.Parallel()
		env := completeTranslationEnv()
		delete(env, "TRANSLATION_INPUT_USD_PER_MTOK")
		delete(env, "TRANSLATION_OUTPUT_USD_PER_MTOK")
		env["TRANSLATION_FREE_OF_CHARGE"] = "true"
		cfg, err := config.FromEnv(envFrom(env))
		if err != nil {
			t.Fatalf("FromEnv() error: %v", err)
		}
		if !cfg.Translation.Enabled() {
			t.Errorf("Enabled() = false for a free-of-charge host; Missing() = %v", cfg.Translation.Missing())
		}
	})

	t.Run("the API key is optional even when everything else is set", func(t *testing.T) {
		t.Parallel()
		env := completeTranslationEnv()
		delete(env, "TRANSLATION_API_KEY")
		cfg, err := config.FromEnv(envFrom(env))
		if err != nil {
			t.Fatalf("FromEnv() error: %v", err)
		}
		if !cfg.Translation.Enabled() {
			t.Errorf("Enabled() = false without an API key; a self-hosted server expects none. Missing() = %v", cfg.Translation.Missing())
		}
	})

	t.Run("interval zero disables a complete configuration", func(t *testing.T) {
		t.Parallel()
		env := completeTranslationEnv()
		env["TRANSLATION_INTERVAL"] = "0"
		cfg, err := config.FromEnv(envFrom(env))
		if err != nil {
			t.Fatalf("FromEnv() error: %v", err)
		}
		if cfg.Translation.Enabled() {
			t.Error("Enabled() = true with TRANSLATION_INTERVAL=0, the one disable switch")
		}
		if !cfg.Translation.Configured() {
			t.Error("Configured() = false: disabling the cadence must not read as missing provider keys")
		}
	})

	// Every partially-configured shape stays off and names what is
	// missing, so the operator reads a list of keys, not a silent
	// reader-only deployment.
	partials := []struct {
		name   string
		remove string
	}{
		{"no base URL", "TRANSLATION_BASE_URL"},
		{"no model", "TRANSLATION_MODEL"},
		{"no input price", "TRANSLATION_INPUT_USD_PER_MTOK"},
		{"no output price", "TRANSLATION_OUTPUT_USD_PER_MTOK"},
		{"no article ceiling", "TRANSLATION_ARTICLE_CEILING_MICROUSD"},
		{"no monthly cap", "TRANSLATION_MONTHLY_CAP_MICROUSD"},
	}
	for _, tt := range partials {
		t.Run("partial: "+tt.name, func(t *testing.T) {
			t.Parallel()
			env := completeTranslationEnv()
			delete(env, tt.remove)
			cfg, err := config.FromEnv(envFrom(env))
			if err != nil {
				t.Fatalf("FromEnv() error: %v", err)
			}
			tr := cfg.Translation
			if tr.Enabled() {
				t.Errorf("Enabled() = true without %s", tt.remove)
			}
			if !tr.AnySet() {
				t.Error("AnySet() = false: a partial configuration must read as a mistake, not as untouched")
			}
			if len(tr.Missing()) == 0 {
				t.Errorf("Missing() is empty without %s; the startup log line would name no keys", tt.remove)
			}
		})
	}

	// Values that ARE set must be values somebody could have meant;
	// anything else fails startup rather than silently disabling a
	// pipeline the operator believes is on.
	rejected := []struct {
		name  string
		key   string
		value string
	}{
		{"non-numeric price", "TRANSLATION_INPUT_USD_PER_MTOK", "cheap"},
		{"zero price is not how free is declared", "TRANSLATION_INPUT_USD_PER_MTOK", "0"},
		{"negative price", "TRANSLATION_OUTPUT_USD_PER_MTOK", "-0.5"},
		{"non-finite price", "TRANSLATION_OUTPUT_USD_PER_MTOK", "Inf"},
		{"non-integer cap", "TRANSLATION_MONTHLY_CAP_MICROUSD", "25.5"},
		{"zero cap is not a budget", "TRANSLATION_MONTHLY_CAP_MICROUSD", "0"},
		{"negative ceiling", "TRANSLATION_ARTICLE_CEILING_MICROUSD", "-1"},
		{"non-boolean free switch", "TRANSLATION_FREE_OF_CHARGE", "gratis"},
		{"non-duration interval", "TRANSLATION_INTERVAL", "hourly"},
		{"negative interval", "TRANSLATION_INTERVAL", "-5m"},
	}
	for _, tt := range rejected {
		t.Run("rejected: "+tt.name, func(t *testing.T) {
			t.Parallel()
			env := completeTranslationEnv()
			env[tt.key] = tt.value
			if got, err := config.FromEnv(envFrom(env)); err == nil {
				t.Fatalf("FromEnv() = %+v, want error for %s=%q", got.Translation, tt.key, tt.value)
			}
		})
	}
}
