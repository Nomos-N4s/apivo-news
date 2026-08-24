package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// Ledger driver names. This is the whole set, fixed by ADR-0002 and
// specs/002-apivo-cashback-alpha/contracts/ports.md: the Ledger port has
// exactly three implementations, and a fourth would be an architecture
// decision, not a configuration value. Rejecting an unknown name here is
// what stops LEDGER_DRIVER=blnkk from silently selecting nothing.
const (
	// LedgerDriverBlnk is the adopted open-source ledger (ADR-0002). It
	// requires BLNK_URL and a running Blnk beside the binary.
	LedgerDriverBlnk = "blnk"
	// LedgerDriverMemory is the in-process ledger: unit tests, and local
	// development while Docker is unavailable. It persists nothing and
	// must never be selected by a deployment.
	LedgerDriverMemory = "memory"
	// LedgerDriverPostgres is the in-repository Postgres ledger kept as
	// the documented exit route from Blnk (ADR-0002).
	LedgerDriverPostgres = "postgres"
)

// NetworkDriverFixture is the affiliate-network adapter that replays a
// recorded click -> pending -> approved -> reversed lifecycle from
// testdata. It needs no credentials, which is what lets the whole cashback
// chain be built and demonstrated before a publisher account exists
// (ADR-0003).
const NetworkDriverFixture = "fixture"

// ledgerDrivers is the membership test behind LEDGER_DRIVER's validation.
var ledgerDrivers = map[string]struct{}{
	LedgerDriverBlnk:     {},
	LedgerDriverMemory:   {},
	LedgerDriverPostgres: {},
}

// CashbackConfig is the cashback product's environment: whether the product
// is mounted at all, which ledger carries the money, which affiliate network
// reports the commission, and where the two supporting services live.
//
// The stance differs from TranslationConfig's on purpose. Translation may be
// half-configured and simply stay off, because the cost of that is an
// untranslated article. Cashback moves members' money, so when
// CASHBACK_ENABLED is true every key it needs is REQUIRED and startup fails
// naming the ones that are missing: a cashback deployment that quietly chose
// a default ledger is the failure this refuses to have.
//
// There is deliberately no default for LEDGER_DRIVER. "memory" would be the
// convenient one and it is the dangerous one - a deployment that forgot the
// key would run a ledger that persists nothing, and would look healthy while
// doing it.
type CashbackConfig struct {
	// Enabled (CASHBACK_ENABLED) mounts the cashback route tree. False -
	// the default, and what an untouched environment parses to - means the
	// product does not exist in this process: routes are not mounted, not
	// merely hidden from navigation.
	Enabled bool
	// LedgerDriver (LEDGER_DRIVER) selects the Ledger port's
	// implementation: LedgerDriverBlnk, LedgerDriverMemory or
	// LedgerDriverPostgres.
	LedgerDriver string
	// BlnkURL (BLNK_URL) is the Blnk sidecar's API root, e.g.
	// http://blnk:5001. Required by, and meaningful only to,
	// LedgerDriverBlnk.
	BlnkURL string
	// BlnkSecretKey (BLNK_SECRET_KEY) authenticates calls to Blnk when the
	// sidecar runs with authentication on. Optional in dev and CI, where
	// the sidecar is on a private network and runs unauthenticated;
	// required in APP_ENV=prod, because a production ledger reachable
	// without a credential is a production ledger anybody can post to.
	BlnkSecretKey Secret
	// RedisURL (REDIS_URL) locates the Redis that Blnk queues and caches
	// through. Optional: Redis is Blnk's dependency, not Apivo's, and it
	// holds no source of truth (ADR-0002) - losing it loses throughput,
	// not money. The key exists so one place names the dependency and so
	// the value is validated rather than discovered malformed by a sidecar.
	RedisURL string
	// Network is the affiliate network adapter's selection and credentials.
	Network NetworkConfig
}

// NetworkConfig is the affiliate network's adapter selection and its
// credentials. The alpha integrates one network (plan.md, Scale/Scope), so
// there is one credential set rather than a per-network map: a second
// network is a second adapter and a deliberate configuration change, not
// something a wildcard environment lookup should be able to conjure.
//
// Credentials never enter the database or the repository (ADR-0003). They
// are Secrets, so they cannot be printed by accident.
type NetworkConfig struct {
	// Driver (NETWORK_DRIVER) names the adapter package: NetworkDriverFixture,
	// or a network's own identifier once the founder's network decision
	// (Q1) is made and its adapter exists.
	Driver string
	// AccountID (NETWORK_ACCOUNT_ID) is the publisher account the adapter
	// polls as. Not a secret: it appears in deeplinks.
	AccountID string
	// APIKey (NETWORK_API_KEY) is the network credential.
	APIKey Secret
	// APISecret (NETWORK_API_SECRET) is the second half of the credential
	// where a network issues one. Optional: not every network does, and
	// the adapter - which knows its own network - is what judges that.
	APISecret Secret
}

// NeedsCredentials reports whether the selected adapter talks to a real
// network. The fixture adapter does not, which is the whole point of it.
func (n NetworkConfig) NeedsCredentials() bool {
	return n.Driver != "" && n.Driver != NetworkDriverFixture
}

// UsesBlnk reports whether the configuration points money at the Blnk
// sidecar. Callers use it to decide whether BLNK_URL and the sidecar's
// health matter at all.
func (c CashbackConfig) UsesBlnk() bool { return c.LedgerDriver == LedgerDriverBlnk }

// Missing lists the environment keys cashback still needs. Empty means the
// product can be mounted. It is evaluated only when CASHBACK_ENABLED is
// true; with cashback off, an unset key is not a mistake, it is the absence
// of a product.
func (c CashbackConfig) Missing() []string {
	var missing []string
	if c.LedgerDriver == "" {
		missing = append(missing, "LEDGER_DRIVER")
	}
	if c.Network.Driver == "" {
		missing = append(missing, "NETWORK_DRIVER")
	}
	if c.UsesBlnk() && c.BlnkURL == "" {
		missing = append(missing, "BLNK_URL")
	}
	if c.Network.NeedsCredentials() {
		if c.Network.AccountID == "" {
			missing = append(missing, "NETWORK_ACCOUNT_ID")
		}
		if c.Network.APIKey.IsZero() {
			missing = append(missing, "NETWORK_API_KEY")
		}
	}
	return missing
}

// LogValue renders the cashback configuration for a log line: the drivers
// and endpoints an operator needs to see, the credentials reduced to
// whether they are set. The URLs pass through redactedURL, so a Redis URL
// carrying a password does not print it.
//
// It exists so the composition root can log the whole struct - the natural
// thing to write, and the thing that would otherwise print a credential.
func (c CashbackConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("enabled", c.Enabled),
		slog.String("ledger_driver", c.LedgerDriver),
		slog.String("blnk_url", redactedURL(c.BlnkURL)),
		slog.Bool("blnk_secret_key_set", !c.BlnkSecretKey.IsZero()),
		slog.String("redis_url", redactedURL(c.RedisURL)),
		slog.String("network_driver", c.Network.Driver),
		slog.String("network_account_id", c.Network.AccountID),
		slog.Bool("network_api_key_set", !c.Network.APIKey.IsZero()),
		slog.Bool("network_api_secret_set", !c.Network.APISecret.IsZero()),
	)
}

// redactedURL renders a URL with any userinfo replaced by xxxxx. A value
// that will not parse renders as RedactedPlaceholder rather than as itself:
// this runs on the logging path, where failing open would print the whole
// credential-bearing string.
func redactedURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return RedactedPlaceholder
	}
	return parsed.Redacted()
}

// parseCashback reads the cashback environment and validates every value
// that is set, whether or not the product is enabled. A malformed value is
// always an error: an operator who set LEDGER_DRIVER=blnkk and left cashback
// off this time should learn about it now, not on the deploy that turns the
// product on.
//
// Completeness - as opposed to well-formedness - is required only when the
// product is enabled. See CashbackConfig.
func parseCashback(getenv func(string) string) (CashbackConfig, error) {
	c := CashbackConfig{
		LedgerDriver:  strings.TrimSpace(getenv("LEDGER_DRIVER")),
		BlnkURL:       strings.TrimSpace(getenv("BLNK_URL")),
		BlnkSecretKey: NewSecret(getenv("BLNK_SECRET_KEY")),
		RedisURL:      strings.TrimSpace(getenv("REDIS_URL")),
		Network: NetworkConfig{
			Driver:    strings.TrimSpace(getenv("NETWORK_DRIVER")),
			AccountID: strings.TrimSpace(getenv("NETWORK_ACCOUNT_ID")),
			APIKey:    NewSecret(getenv("NETWORK_API_KEY")),
			APISecret: NewSecret(getenv("NETWORK_API_SECRET")),
		},
	}

	if raw := getenv("CASHBACK_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return CashbackConfig{}, fmt.Errorf("config: CASHBACK_ENABLED %q is not a boolean: %w", raw, err)
		}
		c.Enabled = enabled
	}

	if c.LedgerDriver != "" {
		if _, known := ledgerDrivers[c.LedgerDriver]; !known {
			return CashbackConfig{}, fmt.Errorf(
				"config: LEDGER_DRIVER %q is not a ledger this binary has; the implementations are %q (the sidecar), %q (in-process, persists nothing) and %q (the exit route)",
				c.LedgerDriver, LedgerDriverBlnk, LedgerDriverMemory, LedgerDriverPostgres)
		}
	}

	if err := validateNetworkDriver(c.Network.Driver); err != nil {
		return CashbackConfig{}, err
	}
	if err := validateEndpoint("BLNK_URL", c.BlnkURL, "http", "https"); err != nil {
		return CashbackConfig{}, err
	}
	if err := validateEndpoint("REDIS_URL", c.RedisURL, "redis", "rediss"); err != nil {
		return CashbackConfig{}, err
	}

	// A BLNK_URL beside a ledger that is not Blnk is an operator who
	// believes the money is going somewhere it is not. That is the same
	// class of mistake as JWT_AUDIENCE without JWKS_URL, and it gets the
	// same refusal.
	if c.BlnkURL != "" && c.LedgerDriver != "" && !c.UsesBlnk() {
		return CashbackConfig{}, fmt.Errorf(
			"config: BLNK_URL is set but LEDGER_DRIVER is %q, so no posting would ever reach that ledger; select %q or unset BLNK_URL",
			c.LedgerDriver, LedgerDriverBlnk)
	}

	return c, nil
}

// requireCashbackComplete is the enabled-only half of the validation, split
// out because one of its rules depends on APP_ENV, which FromEnv resolves
// after the cashback block is parsed.
func requireCashbackComplete(c CashbackConfig, env string) error {
	if !c.Enabled {
		return nil
	}
	if missing := c.Missing(); len(missing) > 0 {
		return fmt.Errorf(
			"config: CASHBACK_ENABLED is true but %s %s unset; cashback moves members' money, so it starts fully configured or not at all - set them, or set CASHBACK_ENABLED=false",
			strings.Join(missing, ", "), plural(len(missing), "is", "are"))
	}
	if env == EnvProd && c.UsesBlnk() && c.BlnkSecretKey.IsZero() {
		return fmt.Errorf(
			"config: BLNK_SECRET_KEY is unset and APP_ENV=%s: refusing to start. A ledger reachable without a credential is a ledger anybody on that network can post to, and postings are money", EnvProd)
	}
	if env == EnvProd && c.LedgerDriver == LedgerDriverMemory {
		return fmt.Errorf(
			"config: LEDGER_DRIVER=%s and APP_ENV=%s: refusing to start. The in-process ledger persists nothing, so every balance it reports would vanish with the process", LedgerDriverMemory, EnvProd)
	}
	return nil
}

// plural picks the verb for a list of n things, so the error above reads as
// English in both cases.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// validateNetworkDriver requires a lowercase identifier: a package name, in
// the shape internal/cashback/networks/<driver> takes. Rejecting anything
// else keeps a typo, a path or a stray quote from reaching the adapter
// registry as a lookup that quietly finds nothing.
func validateNetworkDriver(driver string) error {
	if driver == "" {
		return nil
	}
	if !isLowerIdentifier(driver) {
		return fmt.Errorf(
			"config: NETWORK_DRIVER %q is not an adapter name; it must be lowercase letters, digits and underscores, starting with a letter (e.g. %q)",
			driver, NetworkDriverFixture)
	}
	return nil
}

// isLowerIdentifier reports whether s is [a-z][a-z0-9_]*.
func isLowerIdentifier(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return s != ""
}

// validateEndpoint requires a value, when set, to be an absolute URL with
// one of the given schemes and a host. An endpoint with a typo fails here
// rather than as a connection error on the first posting.
//
// The value is never repeated in an error: REDIS_URL may carry a password,
// and an error travels to stderr, which is a container log somebody keeps.
func validateEndpoint(key, raw string, schemes ...string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: %s is not a valid URL (the value is not repeated here: it may carry a password)", key)
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			if parsed.Host == "" {
				return fmt.Errorf("config: %s has no host; it must be an absolute URL such as %s://service:port", key, scheme)
			}
			return nil
		}
	}
	return fmt.Errorf("config: %s must use one of the schemes %s, got %q", key, strings.Join(schemes, ", "), parsed.Scheme)
}
