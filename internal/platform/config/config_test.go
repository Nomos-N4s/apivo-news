package config_test

import (
	"log/slog"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

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
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelInfo,
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
				DatabaseURL: "postgres://y",
				HTTPAddr:    ":9999",
				Env:         config.EnvProd,
				LogLevel:    slog.LevelDebug,
			},
		},
		{
			name: "warn level parsed",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "WARN"},
			want: config.Config{
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelWarn,
			},
		},
		{
			name: "error level parsed",
			env:  map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "error"},
			want: config.Config{
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelError,
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
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelInfo,
				JWKSURL:     "https://auth.example.test/jwks.json",
				JWTAudience: "authenticated",
			},
		},
		{
			name: "JWKS URL without audience is fine",
			env: map[string]string{
				"DATABASE_URL": "postgres://x",
				"JWKS_URL":     "https://auth.example.test/jwks.json",
			},
			want: config.Config{
				DatabaseURL: "postgres://x",
				HTTPAddr:    ":8080",
				Env:         config.EnvDev,
				LogLevel:    slog.LevelInfo,
				JWKSURL:     "https://auth.example.test/jwks.json",
			},
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
