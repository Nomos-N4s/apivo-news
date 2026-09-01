package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
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

// NetworkDriverAwin is the adapter for Awin, the affiliate network this
// deployment integrates (spec Q1, decided 2026-08-31). It needs
// NETWORK_ACCOUNT_ID and NETWORK_API_KEY; the credential is read from the
// environment and never written to the repository or the database
// (ADR-0003).
const NetworkDriverAwin = "awin"

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
	// ClickContextHeader (CLICK_CONTEXT_HEADER) names the header this
	// deployment's edge sets to carry the real client address, for the
	// privacy-minimised context digest a click records (FR-022) and for the
	// per-device half of the click rule (US7 scenario 1).
	//
	// Empty - the default - means the deployment names none, and then the
	// digest is built from the connection's own peer. Behind a proxy that is
	// the PROXY, which is still a context and is not one that tells devices
	// apart, so the per-device half of the rule stays off rather than
	// bracketing every member behind it. The per-member half is unaffected
	// and always applies.
	//
	// It is a statement of trust and so it is a deployment's to make: a
	// header a client can set is a context a client can choose, and a chosen
	// context evades a per-device rule by changing on every request. Name
	// only a header an edge sets itself and strips any inbound copy of.
	ClickContextHeader string
	// HouseAccounts is the configured names of the ledger's house
	// accounts. The names live here because the design puts them in
	// configuration and nowhere else (data-model.md 2.6): domain code
	// takes them from this struct, never from a literal.
	HouseAccounts HouseAccountsConfig

	// PayoutThreshold (PAYOUT_THRESHOLD_MINOR, PAYOUT_THRESHOLD_CURRENCY)
	// is the confirmed balance a member must reach before a withdrawal may
	// be requested (FR-050). Q5 left the figure to configuration, because
	// it is a commercial decision that changes without a deploy.
	//
	// One value in two keys, and refused unless both are set. A threshold
	// is money, and money without its currency is a number somebody
	// compares a balance against and gets right until the day two
	// currencies are published (C-6). Zero is a legitimate threshold - it
	// means any confirmed balance may be withdrawn - so the amount alone
	// cannot say whether one was configured, which is why the pair is
	// checked rather than the number.
	PayoutThreshold money.Amount
}

// HouseAccountsConfig names the house accounts the cashback design needs:
// exactly the two the design names (data-model.md 2.6), one key per
// purpose. There are no defaults, for the same reason LEDGER_DRIVER has
// none - a house account is where members' money meets the business's, and
// a name nobody chose would be an account nobody meant to open. Renaming
// one after money has accrued strands the old balance under the old name,
// so these are values an operator sets once and leaves alone.
//
// The keys are required only where money is real: every deployed
// environment runs APP_ENV=prod and refuses to start without them
// (requireCashbackComplete), while the documented no-Docker loop and the
// CI jobs enable the product with four keys and no house names at all -
// spike S3 pins that environment as complete, so the requirement must not
// bite outside production.
//
// The names are not secrets - they are account labels, logged in clear -
// but they are load-bearing: two purposes configured to one name would
// merge two figures the design keeps separate, and parseCashback refuses
// that outright, in every environment, whether or not the product is on.
type HouseAccountsConfig struct {
	// Rounding (HOUSE_ACCOUNT_ROUNDING) names the account the
	// sub-minor-unit remainder of every percent earning accrues to, so
	// each commission split still sums to zero and the rounding is never
	// lost (D6, FR-040).
	Rounding string
	// Clawback (HOUSE_ACCOUNT_CLAWBACK) names the account an absorbed
	// loss is recorded against when a transaction reverses after payout:
	// the loss is absorbed, recorded, and the member is never chased (Q3).
	Clawback string
	// NetworkReceivable (HOUSE_ACCOUNT_NETWORK_RECEIVABLE) names the
	// account holding commission a network has reported. It is the source
	// side of every earning: the member's share moves out of it, the
	// sub-minor-unit rounding moves to Rounding, and what stays is Apivo's
	// own cut (FR-040, D6).
	//
	// A configured account rather than a derived one, for the reason the
	// other two are: this is where members' money meets the business's, and
	// a name nobody chose is an account nobody meant to open. Apivo's
	// revenue is the residue rather than a fourth account, because a
	// separate revenue posting would be a second figure to reconcile
	// against a balance that already answers the same question.
	NetworkReceivable string
}

// purposes pairs every configured house account with what it is for, so a
// refusal can say which two purposes collided rather than printing a name.
func (h HouseAccountsConfig) purposes() [][2]string {
	return [][2]string{
		{"HOUSE_ACCOUNT_ROUNDING", h.Rounding},
		{"HOUSE_ACCOUNT_CLAWBACK", h.Clawback},
		{"HOUSE_ACCOUNT_NETWORK_RECEIVABLE", h.NetworkReceivable},
	}
}

// distinct refuses two purposes configured to one account name.
//
// One name for two purposes is one balance absorbing two meanings: the
// rounding remainder D6 keeps visible, the clawback losses Q3 says to record
// and the commission an earning is paid out of would merge into a single
// figure none of them can be read back out of. The wallet refuses the same
// misconfiguration when the accounts are constructed; refusing it here as
// well - whether or not the product is enabled - is what lets an operator
// learn on this deploy instead of the one that turns cashback on.
//
// A loop over every pair rather than a hand-written comparison, because the
// comparison is the shape that grows wrong: the two-account version compared
// the only pair there was, and adding a third purpose would have left two of
// the three pairs unchecked while still looking complete.
func (h HouseAccountsConfig) distinct() error {
	named := h.purposes()
	for i, a := range named {
		for _, b := range named[i+1:] {
			if a[1] != "" && a[1] == b[1] {
				return fmt.Errorf(
					"config: %s and %s both name %q; each house purpose is a figure the others cannot be read out of, so a shared account makes all of them unreadable - give every purpose its own account name",
					a[0], b[0], a[1])
			}
		}
	}
	return nil
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
	// SourceLanguage (NETWORK_SOURCE_LANGUAGE) is the BCP-47 primary
	// language subtag the network supplies its catalogue copy in, as an
	// operator states it.
	//
	// Configuration and not detection, deliberately. No network this port
	// speaks to says what language a programme name is in, and
	// merchant_copy's whole design is that a fallback is LABELLED rather
	// than guessed (US5 scenario 2) - so the label has to come from
	// somebody who knows. A guess here would put a wrong language tag on
	// every retailer this deployment ever imports.
	//
	// Optional, and its absence is not a broken deployment: the catalogue
	// import is simply not scheduled, one ERROR line says so, and
	// everything else - clicks on an existing catalogue, the wallet, the
	// money loop - runs. That is the same stance the network sweeps take on
	// a missing publisher account.
	SourceLanguage string
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
		slog.String("house_account_rounding", c.HouseAccounts.Rounding),
		slog.String("house_account_clawback", c.HouseAccounts.Clawback),
		slog.String("house_account_network_receivable", c.HouseAccounts.NetworkReceivable),
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
		LedgerDriver:       strings.TrimSpace(getenv("LEDGER_DRIVER")),
		BlnkURL:            strings.TrimSpace(getenv("BLNK_URL")),
		BlnkSecretKey:      NewSecret(getenv("BLNK_SECRET_KEY")),
		RedisURL:           strings.TrimSpace(getenv("REDIS_URL")),
		ClickContextHeader: strings.TrimSpace(getenv("CLICK_CONTEXT_HEADER")),
		Network: NetworkConfig{
			Driver:    strings.TrimSpace(getenv("NETWORK_DRIVER")),
			AccountID: strings.TrimSpace(getenv("NETWORK_ACCOUNT_ID")),
			APIKey:    NewSecret(getenv("NETWORK_API_KEY")),
			APISecret: NewSecret(getenv("NETWORK_API_SECRET")),
			// Lower-cased here so "DE" and "de" name one language: the
			// column this ends up in is keyed by the tag, and a second
			// casing would be a second row nothing matches.
			SourceLanguage: strings.ToLower(strings.TrimSpace(getenv("NETWORK_SOURCE_LANGUAGE"))),
		},
		HouseAccounts: HouseAccountsConfig{
			Rounding:          strings.TrimSpace(getenv("HOUSE_ACCOUNT_ROUNDING")),
			Clawback:          strings.TrimSpace(getenv("HOUSE_ACCOUNT_CLAWBACK")),
			NetworkReceivable: strings.TrimSpace(getenv("HOUSE_ACCOUNT_NETWORK_RECEIVABLE")),
		},
	}

	threshold, err := parsePayoutThreshold(getenv)
	if err != nil {
		return CashbackConfig{}, err
	}
	c.PayoutThreshold = threshold

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

	// Two house purposes on one account name is one balance absorbing
	// two meanings: the rounding remainder D6 keeps visible and the
	// clawback losses Q3 says to record would merge into a single figure
	// neither can be read back out of. The wallet refuses the same
	// misconfiguration when the accounts are constructed; refusing it
	// here as well - whether or not the product is enabled - is what
	// lets the operator learn on this deploy instead of the one that
	// turns cashback on.
	if err := c.HouseAccounts.distinct(); err != nil {
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
	// The house account names are a production rule rather than part of
	// Missing(), because the two audiences differ. The documented
	// no-Docker loop and the CI jobs enable the product with four keys
	// and no house names, and spike S3 holds that environment complete
	// (ADR-0002). A deployed environment runs APP_ENV=prod, and there
	// the first percent earning posts a remainder (D6) - a deployment
	// that could not say where would learn so on its first commission
	// rather than at startup, which is exactly the discovery order this
	// function exists to prevent.
	if env == EnvProd {
		var unset []string
		if c.HouseAccounts.Rounding == "" {
			unset = append(unset, "HOUSE_ACCOUNT_ROUNDING")
		}
		if c.HouseAccounts.Clawback == "" {
			unset = append(unset, "HOUSE_ACCOUNT_CLAWBACK")
		}
		if c.HouseAccounts.NetworkReceivable == "" {
			unset = append(unset, "HOUSE_ACCOUNT_NETWORK_RECEIVABLE")
		}

		if len(unset) > 0 {
			return fmt.Errorf(
				"config: %s %s unset and APP_ENV=%s: refusing to start. The house accounts are where the rounding remainder (D6) and absorbed clawback losses (Q3) live and where an earning is paid out of (FR-040), and a production deployment that cannot name them would discover that on its first commission",
				strings.Join(unset, ", "), plural(len(unset), "is", "are"), EnvProd)
		}
		// The threshold is its own refusal rather than a fourth name in the
		// list above, because it is a different mistake with a different
		// consequence: the house accounts are discovered missing by the first
		// commission, and the threshold by the first member who asks to be
		// paid. A merged message would explain neither.
		if c.PayoutThreshold.Currency == "" {
			return fmt.Errorf(
				"config: PAYOUT_THRESHOLD_MINOR and PAYOUT_THRESHOLD_CURRENCY are unset and APP_ENV=%s: refusing to start. The threshold is the confirmed balance a withdrawal is checked against (FR-050), and a production deployment that cannot say what it is would answer every member's wallet with a figure it invented",
				EnvProd)
		}
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
	// The scheme is NOT quoted back, though it looks like the one part of
	// the value that could not be sensitive. url.Parse takes it from the raw
	// string, and a value that is not a URL at all still yields one:
	// "hunter2:x" parses with Scheme "hunter2". A credential pasted into the
	// wrong key would therefore print its own first half in a startup error,
	// which is exactly what this function's doc comment promises will never
	// happen.
	return fmt.Errorf("config: %s must use one of the schemes %s (the value is not repeated here: it may carry a password)",
		key, strings.Join(schemes, ", "))
}

// parsePayoutThreshold reads the withdrawal threshold as one amount.
//
// Neither key set is not a misconfiguration: outside production the product
// runs without one, exactly as it runs without house account names, and the
// zero Amount says plainly that none was configured. One key set is always a
// misconfiguration, in every environment - PAYOUT_THRESHOLD_MINOR=2000 with
// no currency is a number nobody can compare a balance against, and
// PAYOUT_THRESHOLD_CURRENCY=EUR alone is a currency for a threshold that does
// not exist.
func parsePayoutThreshold(getenv func(string) string) (money.Amount, error) {
	minor := strings.TrimSpace(getenv("PAYOUT_THRESHOLD_MINOR"))
	currency := strings.TrimSpace(getenv("PAYOUT_THRESHOLD_CURRENCY"))
	switch {
	case minor == "" && currency == "":
		return money.Amount{}, nil
	case minor == "":
		return money.Amount{}, fmt.Errorf(
			"config: PAYOUT_THRESHOLD_CURRENCY is set to %q and PAYOUT_THRESHOLD_MINOR is not: a currency with no amount is not a threshold", currency)
	case currency == "":
		return money.Amount{}, fmt.Errorf(
			"config: PAYOUT_THRESHOLD_MINOR is set to %q and PAYOUT_THRESHOLD_CURRENCY is not: an amount with no currency is a number nobody can compare a balance against (C-6)", minor)
	}

	value, err := strconv.ParseInt(minor, 10, 64)
	if err != nil {
		return money.Amount{}, fmt.Errorf("config: PAYOUT_THRESHOLD_MINOR %q is not a whole number of minor units: %w", minor, err)
	}
	// Negative is refused and zero is not. A threshold of nothing means
	// every confirmed balance may be withdrawn, which is a deployment's to
	// choose; a threshold below nothing is not a figure any balance could
	// fail to reach.
	if value < 0 {
		return money.Amount{}, fmt.Errorf("config: PAYOUT_THRESHOLD_MINOR %d is negative: no balance can fail to reach it", value)
	}
	threshold, err := money.New(value, money.Currency(currency))
	if err != nil {
		return money.Amount{}, fmt.Errorf("config: PAYOUT_THRESHOLD_CURRENCY %q: %w", currency, err)
	}
	return threshold, nil
}
