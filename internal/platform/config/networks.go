package config

// NETWORKS and the per-driver blocks beneath it (T215-T217, FR-090..FR-093).
//
// The single-network shape this replaces was deliberate and is quoted here
// because the reasoning still holds: "a second network is a second adapter
// and a deliberate configuration change, not something a wildcard
// environment lookup should be able to conjure." An explicit ordered list is
// how that survives becoming plural. Nothing is inferred from which blocks
// happen to be present.
//
// WHY THE LIST DECIDES, and not the presence of a block. FR-091 requires an
// incomplete network be reported BY NAME. If presence implied intent, a typo
// in NETWORK_LINKWISE_API_KEY would make Linkwise vanish - no name to
// report, nothing to complain about, and a deployment quietly polling one
// network while its operator believes it polls two. Naming the network first
// is what turns a missing key into a failure with a name in it.

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// NetworksKey is the environment variable naming which networks run.
const NetworksKey = "NETWORKS"

// legacyNetworkKeys are the flat keys NETWORKS replaces.
//
// Refused rather than aliased, and that is the whole point: a silent alias
// lets a deployment that meant to run two networks run one, with every
// diagnostic green. Refusing costs one edit at upgrade time and cannot be
// misread.
var legacyNetworkKeys = []string{
	"NETWORK_DRIVER",
	"NETWORK_ACCOUNT_ID",
	"NETWORK_API_KEY",
	"NETWORK_API_SECRET",
	"NETWORK_SOURCE_LANGUAGE",
}

// parseNetworks reads NETWORKS and the block under each entry.
//
// It returns an error only for a configuration that is WRONG - a legacy key,
// a malformed driver name, a duplicate. A network that is merely INCOMPLETE
// is returned in the slice and reported by [CashbackConfig.UnusableNetworks];
// FR-091 is explicit that one incomplete network must not stop the others,
// and that the deployment starts either way.
func parseNetworks(getenv func(string) string) ([]NetworkConfig, error) {
	if err := refuseLegacyNetworkKeys(getenv); err != nil {
		return nil, err
	}

	raw := strings.TrimSpace(getenv(NetworksKey))
	if raw == "" {
		// Not an error. Zero usable networks is a deployment that serves
		// the wallet, the money loop and click-outs on an existing
		// catalogue; it simply polls nothing. That is the stance the sweeps
		// already take on a missing publisher account.
		return nil, nil
	}

	var networks []NetworkConfig
	seen := map[string]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		driver := strings.TrimSpace(entry)
		if driver == "" {
			// A trailing or doubled comma. Ignoring it would silently
			// accept "awin,,linkwise" as two networks and "awin," as one,
			// which reads as a third the operator cannot see.
			return nil, fmt.Errorf(
				"config: %s has an empty entry (%s); write the drivers separated by single commas",
				NetworksKey, strconv.Quote(raw))
		}
		if err := validateNetworkDriver(driver); err != nil {
			return nil, err
		}
		if _, dup := seen[driver]; dup {
			// Two adapters for one driver is two publisher accounts at one
			// network, which cashback.network_account already models and
			// NETWORKS does not. Refusing here keeps the two from being
			// confused.
			return nil, fmt.Errorf(
				"config: %s names %s twice; two accounts at one network is a different arrangement, expressed on cashback.network_account rather than here",
				NetworksKey, strconv.Quote(driver))
		}
		seen[driver] = struct{}{}
		networks = append(networks, networkBlock(getenv, driver))
	}
	return networks, nil
}

// refuseLegacyNetworkKeys fails a deployment still configured the old way.
func refuseLegacyNetworkKeys(getenv func(string) string) error {
	var found []string
	for _, key := range legacyNetworkKeys {
		if strings.TrimSpace(getenv(key)) != "" {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(
		"config: %s %s no longer read; name the networks in %s and move each one's settings to %s (for example %s)",
		strings.Join(found, ", "), isAre(len(found)), NetworksKey,
		"NETWORK_<DRIVER>_*", "NETWORK_AWIN_ACCOUNT_ID")
}

// networkBlock reads one network's settings. The driver is the list entry,
// so the block is found by name rather than by index - which is what lets an
// operator read a deployment's environment and see which network each
// setting belongs to.
func networkBlock(getenv func(string) string, driver string) NetworkConfig {
	key := func(suffix string) string {
		return "NETWORK_" + strings.ToUpper(driver) + "_" + suffix
	}
	return NetworkConfig{
		Driver:    driver,
		AccountID: strings.TrimSpace(getenv(key("ACCOUNT_ID"))),
		APIKey:    NewSecret(getenv(key("API_KEY"))),
		APISecret: NewSecret(getenv(key("API_SECRET"))),
		// Lower-cased here so "DE" and "de" name one language: the column
		// this ends up in is keyed by the tag, and a second casing would be
		// a second row nothing matches.
		SourceLanguage: strings.ToLower(strings.TrimSpace(getenv(key("SOURCE_LANGUAGE")))),
	}
}

// Keys names the environment variables this network reads, in the order an
// operator would set them. Used to report what is missing by its real name.
func (n NetworkConfig) Keys() (accountID, apiKey, apiSecret, sourceLanguage string) {
	prefix := "NETWORK_" + strings.ToUpper(n.Driver) + "_"
	return prefix + "ACCOUNT_ID", prefix + "API_KEY", prefix + "API_SECRET", prefix + "SOURCE_LANGUAGE"
}

// MissingKeys lists what this network still needs before it can poll, by the
// environment key an operator would set. Empty means it can be connected.
//
// The fixture needs no credential, which is the whole point of it - but it
// still needs an account id, because the cursors live on a network_account
// row and this is the only value that says which one.
func (n NetworkConfig) MissingKeys() []string {
	accountID, apiKey, apiSecret, _ := n.Keys()
	var missing []string
	if n.AccountID == "" {
		missing = append(missing, accountID)
	}
	if n.NeedsCredentials() && n.APIKey.IsZero() {
		missing = append(missing, apiKey)
	}
	if n.NeedsCredentialPair() && n.APISecret.IsZero() {
		missing = append(missing, apiSecret)
	}
	return missing
}

// NeedsCredentialPair reports whether this driver's credential is TWO values
// rather than one.
//
// Linkwise authenticates with HTTP Basic, so its credential is a username
// and a password and both are required. Awin's is a single bearer token, and
// the fixture's is nothing at all.
//
// It is a fact about the network rather than about this package, and naming
// the driver here is the same compromise [NetworkConfig.NeedsCredentials]
// already makes for the fixture. The alternative is worse: without it a
// deployment carrying a username and no password reports every key present,
// mounts cashback, and is refused by the network on its first poll - which
// an operator reads as the publisher account having been rejected rather
// than as an environment variable nobody set.
func (n NetworkConfig) NeedsCredentialPair() bool {
	return n.Driver == NetworkDriverLinkwise
}

// Usable reports whether this network has everything it needs to poll.
func (n NetworkConfig) Usable() bool { return len(n.MissingKeys()) == 0 }

// UnusableNetwork is one configured network that cannot poll, and why.
type UnusableNetwork struct {
	// Driver is the network as NETWORKS named it.
	Driver string
	// Missing are the environment keys it still needs.
	Missing []string
}

// String renders the problem the way FR-091 asks for it: the network named,
// then the keys.
func (u UnusableNetwork) String() string {
	return fmt.Sprintf("%s cannot poll: %s %s unset",
		strconv.Quote(u.Driver), strings.Join(u.Missing, ", "), isAre(len(u.Missing)))
}

// UnusableNetworks lists every configured network that cannot poll.
//
// Deliberately NOT part of [CashbackConfig.Missing]: that one decides
// whether the product mounts at all, and FR-091 says an incomplete network
// must not stop the deployment or the other networks. This is a report for
// the composition root to log at ERROR, once per network, by name.
func (c CashbackConfig) UnusableNetworks() []UnusableNetwork {
	var unusable []UnusableNetwork
	for _, n := range c.Networks {
		if missing := n.MissingKeys(); len(missing) > 0 {
			unusable = append(unusable, UnusableNetwork{Driver: n.Driver, Missing: missing})
		}
	}
	return unusable
}

// Mountable reports whether the cashback product should be built into this
// process at all.
//
// Enabled is the operator's intent; this is intent AND a configuration that
// can honour it. A network named in NETWORKS but missing its credential
// turns cashback OFF rather than running it half-configured: cashback moves
// members' money, and a network that cannot poll is one whose transactions
// never arrive - members would click, buy, and never be credited, with
// nothing failing anywhere.
//
// ALL configured networks, not merely one: a deployment that names two and
// can poll one is not the deployment its operator described, and quietly
// running the half of it that works is how a member ends up owed money from
// a network nobody is reading.
//
// Deliberately NOT part of [CashbackConfig.Missing]. That one fails config
// parsing, which stops the PROCESS - so a typo in a second network's key
// would take the news site down with it. This stops the cashback product and
// leaves the rest of the deployment serving.
//
// An empty NETWORKS is a different state and stays mountable: nothing is
// configured, so nothing is half-configured. Cashback serves the wallet, the
// money loop and click-outs on an existing catalogue, and one ERROR line
// says no network is being polled.
func (c CashbackConfig) Mountable() bool {
	return c.Enabled && len(c.UnusableNetworks()) == 0
}

// UsableNetworks are the configured networks that can actually poll.
func (c CashbackConfig) UsableNetworks() []NetworkConfig {
	var usable []NetworkConfig
	for _, n := range c.Networks {
		if n.Usable() {
			usable = append(usable, n)
		}
	}
	return usable
}

// networksLogValue renders the configured networks for a log line: the
// account and whether each credential is SET, never its value, grouped under
// the driver's own name.
//
// A group PER NETWORK, keyed by driver, rather than one flattened line:
// an operator reading a startup log with two networks configured has to be
// able to tell which key belongs to which, and that is exactly the confusion
// the per-driver environment keys exist to remove. Keying by the driver
// rather than by position is safe because [parseNetworks] refuses a repeated
// one, and it is what lets a human search the line for "linkwise".
//
// It returns a [slog.Value] and not a slice of them. A slice of slog.Values
// handed to slog.Any renders as [{},{}] - the handler marshals the elements
// rather than resolving them, and every field is unexported - which is a
// redaction that succeeds by printing nothing at all. A group nests.
func networksLogValue(networks []NetworkConfig) slog.Value {
	rendered := make([]slog.Attr, 0, len(networks))
	for i, n := range networks {
		key := n.Driver
		if key == "" {
			// Unreachable through parseNetworks, which refuses an empty
			// entry. Positional rather than empty so a config built in code
			// still logs something an operator can point at.
			key = "network_" + strconv.Itoa(i)
		}
		rendered = append(rendered, slog.Group(key,
			slog.String("account_id", n.AccountID),
			slog.Bool("api_key_set", !n.APIKey.IsZero()),
			slog.Bool("api_secret_set", !n.APISecret.IsZero()),
			slog.String("source_language", n.SourceLanguage),
			slog.Bool("usable", n.Usable()),
		))
	}
	return slog.GroupValue(rendered...)
}
