package config_test

// The deployment manifests, held to this package's own production rules.
//
// requireCashbackComplete refuses to start a production deployment that
// cannot name its house accounts or its withdrawal threshold. That refusal
// is only worth having if the manifests which configure production actually
// set those keys - and twice now they have not: HOUSE_ACCOUNT_NETWORK_RECEIVABLE
// never reached the Hetzner compose overlay, and PAYOUT_THRESHOLD_MINOR and
// PAYOUT_THRESHOLD_CURRENCY reached neither deployment. Both times the api
// would have refused to start on the next deploy, and nothing would have said
// so until it did.
//
// So this reads the manifests and runs their own keys through FromEnv. It
// asserts nothing about WHICH keys are required - that is the config code's
// to decide, and restating the list here would be one more copy to drift.
// A key made required tomorrow fails this test tomorrow, in the package that
// made it required.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// repoRoot is where the manifests live, relative to this package.
const repoRoot = "../../.."

// deployment is one file that configures a running api, and the keys a
// deployment supplies from somewhere else.
type deployment struct {
	name string
	// paths is every file that configures this api. Hetzner takes two: the
	// compose overlay sets what is structural, and the operator's env file
	// sets what an operator decides. Either alone would answer the question
	// wrongly.
	paths []string
	// elsewhere is what this file legitimately does not carry: secrets from
	// a Secret or an operator-edited env file, and the two settings the
	// compose file sets on the api service rather than in the cashback
	// overlay. Listing them here is the point at which somebody has to think
	// about whether a missing key is really supplied elsewhere.
	elsewhere map[string]string
}

func deployments() []deployment {
	// Every deployed environment runs APP_ENV=prod, which is what puts the
	// rules under test in force.
	base := map[string]string{
		"APP_ENV":         config.EnvProd,
		"DATABASE_URL":    "postgres://apivo@db/apivo?sslmode=verify-full",
		"BLNK_SECRET_KEY": "supplied-from-a-secret",
	}
	return []deployment{
		{
			name:      "kubernetes",
			paths:     []string{"deploy/k8s/cashback/cashback-configmap.yaml"},
			elsewhere: base,
		},
		{
			name: "hetzner",
			paths: []string{
				"deploy/hetzner/compose/docker-compose.cashback.yml",
				"deploy/hetzner/env/api.env.example",
			},
			elsewhere: base,
		},
	}
}

// read merges every key this deployment's manifests assign, later files
// winning - which is compose's own precedence between an env file and the
// environment block that overrides it.
func (d deployment) read(t *testing.T) map[string]string {
	t.Helper()
	set := map[string]string{}
	for _, path := range d.paths {
		for key, value := range envKeys(t, path) {
			set[key] = value
		}
	}
	return set
}

// supply fills in what a deployment provides from somewhere else.
//
// It overrides an EMPTY value as well as an absent key, because that is what
// an example env file is: BLNK_SECRET_KEY= is not "unset in production", it
// is "the operator pastes the secret here". Treating the blank as unset would
// fail every deployment whose example correctly refuses to carry a
// credential.
func supply(set, elsewhere map[string]string) {
	for key, value := range elsewhere {
		if set[key] == "" {
			set[key] = value
		}
	}
}

// envKeys pulls every SCREAMING_SNAKE key this manifest assigns, in either
// YAML mapping form (`KEY: value`) or dotenv form (`KEY=value`).
//
// A regex rather than a YAML parser, deliberately: the shapes it must read
// are two, both trivial, and a parser would be a dependency and a second
// thing to be wrong. What makes it trustworthy is that the caller asserts it
// found the keys it must have found - a silent parse failure would otherwise
// read as a manifest that sets nothing, which is the one result this test
// must never accept quietly.
// HORIZONTAL whitespace only, throughout. Go's \s includes the newline, so
// `\s*` after the separator let an empty value swallow the following line -
// POLL_INTERVAL= read as a comment rule, and the config package refused it as
// a malformed duration. An empty assignment is common in these files and must
// read as empty.
var assignment = regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]+)?([A-Z][A-Z0-9_]{2,})[ \t]*[:=][ \t]*([^\n]*)$`)

func envKeys(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	found := map[string]string{}
	for _, match := range assignment.FindAllStringSubmatch(string(raw), -1) {
		key, value := match[1], strings.TrimSpace(match[2])
		value = strings.Trim(value, `"'`)
		// A commented-out assignment is not one. The regex allows leading
		// whitespace and a list dash, never a hash.
		found[key] = expandPlaceholders(value)
	}
	return found
}

// placeholder is compose's own interpolation, which it expands from the
// environment before the api ever sees the value.
var placeholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// expandPlaceholders substitutes something valid for each one.
//
// The substitution is deliberately meaningless - what is under test is
// whether a key is SET, and a value the deployment fills in at run time is
// set. Leaving "${APIVO_ENV}" in a URL would fail the endpoint validator on
// a manifest that is perfectly correct, which would teach whoever hit it to
// distrust this test rather than the manifest.
func expandPlaceholders(value string) string {
	return placeholder.ReplaceAllString(value, "x")
}

// TestEveryDeploymentSatisfiesItsOwnProductionRules is the assertion: the
// keys each manifest sets, plus the ones a deployment supplies from a secret,
// are enough for FromEnv to start.
func TestEveryDeploymentSatisfiesItsOwnProductionRules(t *testing.T) {
	t.Parallel()

	for _, d := range deployments() {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			set := d.read(t)

			// Non-vacuity. A regex that matched nothing would leave every
			// key unset, and FromEnv would then be refusing a deployment
			// this test never actually read.
			if got := set["CASHBACK_ENABLED"]; got != "true" {
				t.Fatalf("%v: read CASHBACK_ENABLED=%q, want \"true\" — the manifests were not parsed", d.paths, got)
			}

			supply(set, d.elsewhere)

			cfg, err := config.FromEnv(func(key string) string { return set[key] })
			if err != nil {
				t.Fatalf("%v does not configure a startable production api: %v\n\n"+
					"The api refuses to start without every key its production rules demand. "+
					"Add the missing key to one of those manifests, or - if a deployment "+
					"really does supply it from somewhere else - say so in this test's "+
					"elsewhere map.",
					d.paths, err)
			}

			// Startable is no longer the whole bar. Since T215 a network
			// missing its credential does not stop the process - it stops
			// CASHBACK, and the process serves the news site without it.
			// A manifest that enables cashback and then cannot mount it
			// would have passed the check above in silence, which is
			// exactly the class of miss this file exists for.
			if !cfg.Cashback.Mountable() {
				t.Fatalf("%v starts, but cashback would not mount: %v\n\n"+
					"NETWORKS names a network whose keys this deployment does not set. "+
					"Every cashback route would answer 404 while the api itself looked healthy.",
					d.paths, cfg.Cashback.UnusableNetworks())
			}
		})
	}
}

// TestTheDeploymentTestWouldNoticeAMissingKey keeps the test above from
// passing vacuously. Drop a key the production rules demand and FromEnv must
// refuse - if it does not, this file is asserting nothing.
func TestTheDeploymentTestWouldNoticeAMissingKey(t *testing.T) {
	t.Parallel()

	for _, dropped := range []string{
		"HOUSE_ACCOUNT_ROUNDING",
		"HOUSE_ACCOUNT_CLAWBACK",
		"HOUSE_ACCOUNT_NETWORK_RECEIVABLE",
		"PAYOUT_THRESHOLD_MINOR",
		"PAYOUT_THRESHOLD_CURRENCY",
	} {
		t.Run(dropped, func(t *testing.T) {
			t.Parallel()
			d := deployments()[0]
			set := d.read(t)
			supply(set, d.elsewhere)
			delete(set, dropped)

			if _, err := config.FromEnv(func(key string) string { return set[key] }); err == nil {
				t.Fatalf("FromEnv started a production api with %s unset, so the test above proves nothing about it", dropped)
			}
		})
	}
}

// TestTheDeploymentTestWouldNoticeAnUnmountableNetwork is the same
// non-vacuity check for the half of the assertion that is NOT a startup
// refusal.
//
// The network keys cannot appear in the list above, because dropping one no
// longer stops the api - that is the whole of the T215 change. Their
// non-vacuity has to be proved against Mountable instead, or the manifests
// could name a network with no account id at all and nothing here would
// notice.
func TestTheDeploymentTestWouldNoticeAnUnmountableNetwork(t *testing.T) {
	t.Parallel()

	for _, d := range deployments() {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			set := d.read(t)
			supply(set, d.elsewhere)

			cfg, err := config.FromEnv(func(key string) string { return set[key] })
			if err != nil {
				t.Fatalf("FromEnv() error: %v", err)
			}
			if len(cfg.Cashback.Networks) == 0 {
				t.Fatalf("%v names no network in %s, so the Mountable assertion above proves nothing",
					d.paths, config.NetworksKey)
			}

			// Drop the account id of the first network the manifests name,
			// by its real per-driver key - which also asserts the manifests
			// use that key rather than a legacy one.
			accountID, _, _, _ := cfg.Cashback.Networks[0].Keys()
			if set[accountID] == "" {
				t.Fatalf("%v does not set %s, and the assertion above should have caught that", d.paths, accountID)
			}
			delete(set, accountID)

			broken, err := config.FromEnv(func(key string) string { return set[key] })
			if err != nil {
				t.Fatalf("FromEnv() with %s unset: %v (it must parse, not refuse)", accountID, err)
			}
			if broken.Cashback.Mountable() {
				t.Fatalf("cashback still mounts with %s unset, so the test above proves nothing about it", accountID)
			}
		})
	}
}
