// Package config loads and validates process configuration from the
// environment. It is a platform primitive and knows nothing about the
// business modules.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strconv"
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

// DefaultTranslationInterval is the translation pipeline's cadence when
// TRANSLATION_INTERVAL is unset. The interval is the one TRANSLATION_*
// value with a default, because it decides nothing on its own: without a
// provider and a budget - which have NO defaults - the pipeline never
// starts, whatever the interval says.
const DefaultTranslationInterval = 15 * time.Minute

// Config holds everything the process needs to start. All values come from
// the environment; nothing is read from files.
type Config struct {
	// DatabaseURL is the Postgres connection string. Required. In
	// APP_ENV=prod it must carry an sslmode that cannot fall back to an
	// unencrypted session - see requireEncryptedDatabase.
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
	// Translation is the machine-translation pipeline's configuration:
	// which provider, at what prices, under which budget. See
	// TranslationConfig for the no-defaults stance.
	Translation TranslationConfig
	// Cashback is the cashback product's configuration: whether it is
	// mounted at all, which ledger carries the money, which affiliate
	// network reports the commission. See CashbackConfig for why its
	// stance on incompleteness is stricter than Translation's.
	Cashback CashbackConfig
	// BrandDir (BRAND_DIR) is the directory holding this deployment's
	// brand.json: who the product is, which legal entity stands behind it,
	// and which revision of the terms members are being asked to accept
	// (ADR-0004).
	//
	// Optional, and that is a decision worth stating rather than a gap. A
	// brand definition names a real company, a real address and real
	// support mailboxes; there is no value this repository could default it
	// to that would not be a lie about somebody. So a deployment without
	// one runs, and the surfaces that need a brand say what is missing -
	// the participation endpoints answer 503 naming this key, exactly as
	// the wallet does for the payout threshold - rather than the process
	// refusing to start or, worse, inventing a brand to start with.
	//
	// The path is not validated here. Whether it holds a readable,
	// complete brand definition is the brand package's question, and the
	// composition root asks it at start-up: a malformed brand file IS a
	// startup failure, because a deployment that named one meant it.
	BrandDir string
}

// TranslationConfig is the TRANSLATION_* environment, parsed but not
// judged: which OpenAI-compatible host to call, the host's published
// prices, and the FR-006 budget. This package only parses values and
// reports completeness - the translation module validates the budget and
// the adapter validates the provider, each owning its own rules.
//
// NO DEFAULTS, deliberately, for the provider, the model, the prices and
// the caps: the production choice of provider and budget is a pending
// founder decision, and nothing here may make it by accident. Unset means
// the pipeline stays OFF - the composition root logs one clear line
// saying so - and the one representation of "not configured yet" is
// emptiness, exactly like JWKS_URL above.
type TranslationConfig struct {
	// BaseURL is the provider's API root (TRANSLATION_BASE_URL), e.g.
	// https://api.openai.com/v1 or a self-hosted http://vllm.internal:8000/v1.
	BaseURL string
	// Model is the model identifier as the host names it
	// (TRANSLATION_MODEL).
	Model string
	// APIKey is the provider credential (TRANSLATION_API_KEY). Optional
	// even when everything else is set: a self-hosted server started
	// without a key expects none.
	APIKey string
	// InputUSDPerMTok and OutputUSDPerMTok are the host's published
	// prices in US dollars per million tokens - the unit these hosts
	// publish, and the shape the provider adapter takes
	// (TRANSLATION_INPUT_USD_PER_MTOK, TRANSLATION_OUTPUT_USD_PER_MTOK).
	// Zero means unset: an explicitly zero price is rejected at parse
	// time, because "free" is declared with FreeOfCharge, never with a
	// price line that a forgotten value could imitate.
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
	// FreeOfCharge (TRANSLATION_FREE_OF_CHARGE=true) declares the host
	// charges nothing per token - a self-hosted server, or quota already
	// paid for - and stands in for both price lines.
	FreeOfCharge bool
	// Interval is the pipeline's cadence (TRANSLATION_INTERVAL, a Go
	// duration). Unset means DefaultTranslationInterval; "0" disables the
	// pipeline and is the only disable switch, like POLL_INTERVAL.
	Interval time.Duration
	// ArticleCeilingMicroUSD and MonthlyCapMicroUSD are the FR-006 budget
	// (TRANSLATION_ARTICLE_CEILING_MICROUSD,
	// TRANSLATION_MONTHLY_CAP_MICROUSD), in micro-USD - the unit the
	// ledger counts in. Zero means unset; an explicit zero is rejected at
	// parse time, because a zero budget is not a budget anyone chose.
	ArticleCeilingMicroUSD int64
	MonthlyCapMicroUSD     int64
}

// Missing lists the required TRANSLATION_* keys still unset. Empty means
// the provider and budget are fully configured.
func (t TranslationConfig) Missing() []string {
	var missing []string
	if t.BaseURL == "" {
		missing = append(missing, "TRANSLATION_BASE_URL")
	}
	if t.Model == "" {
		missing = append(missing, "TRANSLATION_MODEL")
	}
	if !t.FreeOfCharge {
		if t.InputUSDPerMTok == 0 {
			missing = append(missing, "TRANSLATION_INPUT_USD_PER_MTOK (or TRANSLATION_FREE_OF_CHARGE=true)")
		}
		if t.OutputUSDPerMTok == 0 {
			missing = append(missing, "TRANSLATION_OUTPUT_USD_PER_MTOK (or TRANSLATION_FREE_OF_CHARGE=true)")
		}
	}
	if t.ArticleCeilingMicroUSD == 0 {
		missing = append(missing, "TRANSLATION_ARTICLE_CEILING_MICROUSD")
	}
	if t.MonthlyCapMicroUSD == 0 {
		missing = append(missing, "TRANSLATION_MONTHLY_CAP_MICROUSD")
	}
	return missing
}

// Configured reports whether every required key is set. It says nothing
// about the values being usable - the translation module and the adapter
// judge that at wiring time, loudly.
func (t TranslationConfig) Configured() bool { return len(t.Missing()) == 0 }

// AnySet reports whether any provider or budget key is set at all. It
// separates "nobody configured translation" (an expected state, logged
// quietly) from "somebody configured half of it" (a mistake, logged as
// one). The interval is excluded: it has a default and decides nothing
// alone.
func (t TranslationConfig) AnySet() bool {
	return t.BaseURL != "" || t.Model != "" || t.APIKey != "" ||
		t.InputUSDPerMTok != 0 || t.OutputUSDPerMTok != 0 || t.FreeOfCharge ||
		t.ArticleCeilingMicroUSD != 0 || t.MonthlyCapMicroUSD != 0
}

// Enabled reports whether the pipeline should run: fully configured and
// not disabled by TRANSLATION_INTERVAL=0.
func (t TranslationConfig) Enabled() bool { return t.Configured() && t.Interval > 0 }

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
		BrandDir:    strings.TrimSpace(getenv("BRAND_DIR")),
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
	if cfg.Env == EnvProd {
		if err := requireEncryptedDatabase(cfg.DatabaseURL); err != nil {
			return Config{}, err
		}
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
	translation, err := parseTranslation(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg.Translation = translation
	cashback, err := parseCashback(getenv)
	if err != nil {
		return Config{}, err
	}
	// Completeness is checked after Env is resolved: two of the rules -
	// no unauthenticated ledger, no in-process ledger - only apply in
	// production.
	if err := requireCashbackComplete(cashback, cfg.Env); err != nil {
		return Config{}, err
	}
	cfg.Cashback = cashback
	return cfg, nil
}

// sslModesPermittingCleartext are the libpq sslmode values under which the
// driver will happily speak an unencrypted session: "disable" never
// negotiates TLS, "allow" and "prefer" fall back to plaintext when the
// server does not insist, and an absent sslmode means "prefer" - the
// default that made every deployment's TLS the server's decision rather
// than ours.
var sslModesPermittingCleartext = map[string]struct{}{
	"":        {},
	"disable": {},
	"allow":   {},
	"prefer":  {},
}

// requireEncryptedDatabase refuses a production DATABASE_URL whose sslmode
// permits an unencrypted session.
//
// The database holds the whole public record and, in production, lives at
// a managed provider across the public internet. Every example in this
// repository says sslmode=disable, because every example describes the
// local compose Postgres on a loopback address - so the value most likely
// to be copied into production is the one that must never be there. This
// is the check that turns copying it into a refusal at startup rather than
// a plaintext session nobody notices.
//
// The refusal names sslmode=verify-full, which is what the managed
// provider (Supabase, EU region) documents and the only mode that also
// verifies the server is who it claims. "require" and "verify-ca" encrypt
// and are accepted; they are the operator's call to make, and only
// cleartext is ours.
func requireEncryptedDatabase(databaseURL string) error {
	mode, err := sslMode(databaseURL)
	if err != nil {
		return err
	}
	if _, cleartext := sslModesPermittingCleartext[mode]; !cleartext {
		return nil
	}
	named := "carries no sslmode, so libpq defaults to \"prefer\""
	if mode != "" {
		named = fmt.Sprintf("carries sslmode=%s", mode)
	}
	return fmt.Errorf("config: DATABASE_URL %s, which permits an unencrypted session, and APP_ENV=%s: refusing to start. Production connects to a managed database over the public internet - use sslmode=verify-full (the documented value; sslmode=disable belongs to the local compose database only)", named, EnvProd)
}

// sslMode reads the sslmode out of a connection string in either shape
// libpq accepts: a URL (postgres://…?sslmode=…) or a keyword/value DSN
// (host=… sslmode=…). An unparseable string is an error here rather than
// at the first query, because a startup check that silently passes on
// input it did not understand is not a check.
//
// NOTHING returned from here may carry the connection string. It holds the
// production database password, the error travels up to main.go and is
// printed to stderr, and stderr on this deployment is a container log
// Cloudflare keeps - so a DSN with one typo in it would put the password
// where anyone with log access reads it. That is the same hazard as a
// credential in a feed URL, and it gets the same treatment the fetcher
// gives one (#83): report the cause, never the value.
func sslMode(databaseURL string) (string, error) {
	if strings.Contains(databaseURL, "://") {
		parsed, err := url.Parse(databaseURL)
		if err != nil {
			// net/url returns a *url.Error whose Error() embeds the WHOLE
			// string it was given. Wrapping that with %w prints the DSN,
			// password and all. Only the inner cause is reportable.
			var parseErr *url.Error
			if errors.As(err, &parseErr) {
				err = parseErr.Err
			}
			return "", fmt.Errorf("config: DATABASE_URL is not a valid connection URL (the value is not repeated here: it carries the database password): %w", err)
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			// Safe to wrap: ParseQuery names only the offending escape
			// sequence ("invalid URL escape \"%zz\""), never the query it
			// was reading.
			return "", fmt.Errorf("config: DATABASE_URL has an unparseable query string: %w", err)
		}
		return strings.ToLower(query.Get("sslmode")), nil
	}
	// Keyword/value form. Values may be single-quoted; nothing else in a
	// realistic sslmode needs unquoting, and a value we cannot read is
	// reported as absent, which fails closed.
	for _, field := range strings.Fields(databaseURL) {
		key, value, found := strings.Cut(field, "=")
		if found && strings.EqualFold(key, "sslmode") {
			return strings.ToLower(strings.Trim(value, "'\"")), nil
		}
	}
	return "", nil
}

// parseTranslation reads the TRANSLATION_* environment. Absence is legal
// everywhere - unset keys mean the pipeline stays off - but a value that
// IS set must parse and must be one somebody could have meant: a
// malformed price or a zero cap fails startup rather than silently
// disabling a pipeline the operator believes is on.
func parseTranslation(getenv func(string) string) (TranslationConfig, error) {
	t := TranslationConfig{
		BaseURL: getenv("TRANSLATION_BASE_URL"),
		Model:   getenv("TRANSLATION_MODEL"),
		APIKey:  getenv("TRANSLATION_API_KEY"),
	}
	var err error
	if t.InputUSDPerMTok, err = parsePrice("TRANSLATION_INPUT_USD_PER_MTOK", getenv("TRANSLATION_INPUT_USD_PER_MTOK")); err != nil {
		return TranslationConfig{}, err
	}
	if t.OutputUSDPerMTok, err = parsePrice("TRANSLATION_OUTPUT_USD_PER_MTOK", getenv("TRANSLATION_OUTPUT_USD_PER_MTOK")); err != nil {
		return TranslationConfig{}, err
	}
	if raw := getenv("TRANSLATION_FREE_OF_CHARGE"); raw != "" {
		if t.FreeOfCharge, err = strconv.ParseBool(raw); err != nil {
			return TranslationConfig{}, fmt.Errorf("config: TRANSLATION_FREE_OF_CHARGE %q is not a boolean: %w", raw, err)
		}
	}
	if t.ArticleCeilingMicroUSD, err = parseMicroUSD("TRANSLATION_ARTICLE_CEILING_MICROUSD", getenv("TRANSLATION_ARTICLE_CEILING_MICROUSD")); err != nil {
		return TranslationConfig{}, err
	}
	if t.MonthlyCapMicroUSD, err = parseMicroUSD("TRANSLATION_MONTHLY_CAP_MICROUSD", getenv("TRANSLATION_MONTHLY_CAP_MICROUSD")); err != nil {
		return TranslationConfig{}, err
	}
	if t.Interval, err = parseTranslationInterval(getenv("TRANSLATION_INTERVAL")); err != nil {
		return TranslationConfig{}, err
	}
	return t, nil
}

// parsePrice reads one price in USD per million tokens. Empty means
// unset. A set price must be a finite positive number: zero is not a
// price but a free-of-charge declaration, which has its own explicit
// switch precisely so a forgotten value cannot imitate it.
func parsePrice(key, raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	price, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a number (USD per million tokens): %w", key, raw, err)
	}
	if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
		return 0, fmt.Errorf("config: %s must be a finite positive price in USD per million tokens, got %q; a host that charges nothing is declared with TRANSLATION_FREE_OF_CHARGE=true, never with a zero price", key, raw)
	}
	return price, nil
}

// parseMicroUSD reads one budget amount in micro-USD. Empty means unset.
// A set amount must be positive: a zero ceiling refuses every call and a
// zero cap halts the month before it starts, and neither is a budget
// anyone chose - the way to leave the pipeline off is to leave the keys
// unset.
func parseMicroUSD(key, raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	amount, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not an integer amount of micro-USD: %w", key, raw, err)
	}
	if amount <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive amount of micro-USD, got %q; to keep the pipeline off, leave the key unset", key, raw)
	}
	return amount, nil
}

// parseTranslationInterval reads TRANSLATION_INTERVAL exactly like
// POLL_INTERVAL: empty means the default, "0" disables, anything else
// must be a positive Go duration.
func parseTranslationInterval(s string) (time.Duration, error) {
	if s == "" {
		return DefaultTranslationInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: TRANSLATION_INTERVAL %q is not a Go duration (e.g. \"15m\", or \"0\" to disable the translation pipeline): %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: TRANSLATION_INTERVAL must not be negative, got %q", s)
	}
	return d, nil
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
