// Package config loads and validates process configuration from the
// environment. It is a platform primitive and knows nothing about the
// business modules.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Environment names accepted in APP_ENV.
const (
	// EnvDev is the development environment: human-readable logs.
	EnvDev = "dev"
	// EnvProd is the production environment: JSON logs.
	EnvProd = "prod"
)

// DefaultPollInterval is how often the feed poll loop starts a cycle when
// POLL_INTERVAL is unset (or empty, which every environment lookup renders
// the same way). Fifteen minutes suits a handful of municipal feeds: far
// gentler than a reader's feed reader, fresh enough for local news.
const DefaultPollInterval = 15 * time.Minute

// Config holds everything the process needs to start. All values come from
// the environment; nothing is read from files.
type Config struct {
	// DatabaseURL is the Postgres connection string. Required.
	DatabaseURL string
	// HTTPAddr is the listen address for the HTTP server, e.g. ":8080".
	HTTPAddr string
	// Env is the runtime environment: EnvDev or EnvProd.
	Env string
	// LogLevel is the minimum level emitted by the logger.
	LogLevel slog.Level
	// JWKSURL is the JWKS endpoint bearer tokens are verified against -
	// for Supabase, https://<project>.supabase.co/auth/v1/.well-known/jwks.json.
	// Optional: when empty the authenticated (editorial) endpoints are not
	// served, so a deployment without auth configured exposes nothing that
	// would need it.
	JWKSURL string
	// JWTAudience, when non-empty, additionally requires every verified
	// token's aud claim to contain this value. Only meaningful alongside
	// JWKSURL; setting it alone is a configuration error.
	JWTAudience string
	// PollInterval is how often the feed poll loop starts a cycle, from
	// POLL_INTERVAL (a Go duration, e.g. "15m"). Unset means
	// DefaultPollInterval; POLL_INTERVAL=0 is the one disable switch, and
	// zero here means the loop is never started. There is deliberately no
	// separate POLL_ENABLED: two switches for one behaviour invite the
	// combination that says both yes and no.
	PollInterval time.Duration
}

// FromEnv builds a Config from the given environment lookup function,
// applying defaults and validating required values. Production passes
// os.Getenv; tests pass a map lookup.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("DATABASE_URL"),
		HTTPAddr:    getenv("HTTP_ADDR"),
		Env:         getenv("APP_ENV"),
		JWKSURL:     getenv("JWKS_URL"),
		JWTAudience: getenv("JWT_AUDIENCE"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.JWTAudience != "" && cfg.JWKSURL == "" {
		return Config{}, fmt.Errorf("config: JWT_AUDIENCE is set but JWKS_URL is not; an audience without a verification endpoint checks nothing")
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.Env == "" {
		cfg.Env = EnvDev
	}
	if cfg.Env != EnvDev && cfg.Env != EnvProd {
		return Config{}, fmt.Errorf("config: APP_ENV must be %q or %q, got %q", EnvDev, EnvProd, cfg.Env)
	}
	level, err := parseLevel(getenv("LOG_LEVEL"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level
	interval, err := parsePollInterval(getenv("POLL_INTERVAL"))
	if err != nil {
		return Config{}, err
	}
	cfg.PollInterval = interval
	return cfg, nil
}

// parsePollInterval reads POLL_INTERVAL: empty means DefaultPollInterval,
// "0" disables polling, anything else must be a positive Go duration.
func parsePollInterval(s string) (time.Duration, error) {
	if s == "" {
		return DefaultPollInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: POLL_INTERVAL %q is not a Go duration (e.g. \"15m\", or \"0\" to disable polling): %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: POLL_INTERVAL must not be negative, got %q", s)
	}
	return d, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("config: unknown LOG_LEVEL %q", s)
}
