package main

// Which vault the composition root builds, and what a deployment that
// configured none still does (ADR-0006).

import (
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// TestNoVaultConfiguredStartsAnyway is the stance this file exists to pin.
// A deployment with no vault must start and serve the whole cashback
// surface, because only one endpoint needs one and the rest of the payout
// path - including paying somebody whose destination was recorded earlier -
// has nothing to do with it.
func TestNoVaultConfiguredStartsAnyway(t *testing.T) {
	t.Parallel()

	vault, err := newDetailsVault(discardLogger(), config.Config{})
	if err != nil {
		t.Fatalf("newDetailsVault() with no vault configured: %v", err)
	}
	if vault != nil {
		t.Error("a vault was built although none is configured")
	}
}

// TestAVaultThatIsSetAndUnusableRefusesToStart. A deployment that named a
// vault meant it. Starting anyway would answer 503 on every new destination
// while an operator believed they were being recorded, which is the failure
// that is discovered by a member rather than by a log.
func TestAVaultThatIsSetAndUnusableRefusesToStart(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"not a url", "ftp://vault.example", "/v1/secret"} {
		cfg := config.Config{Cashback: config.CashbackConfig{PayoutVaultURL: endpoint}}
		if _, err := newDetailsVault(discardLogger(), cfg); err == nil {
			t.Errorf("newDetailsVault() accepted %q", endpoint)
		}
	}
}

// TestAConfiguredVaultIsBuilt walks the ordinary path, including the mount
// override a deployment sets when its KV engine is not at the conventional
// place.
func TestAConfiguredVaultIsBuilt(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Cashback: config.CashbackConfig{
		PayoutVaultURL:   "https://openbao.example:8200",
		PayoutVaultToken: config.NewSecret("a-token"),
		PayoutVaultMount: "cashback-kv",
	}}
	vault, err := newDetailsVault(discardLogger(), cfg)
	if err != nil {
		t.Fatalf("newDetailsVault(): %v", err)
	}
	if vault == nil {
		t.Fatal("no vault was built although one is configured")
	}
}

// TestTheVaultsCredentialNeverReachesTheConfigLog is ADR-0003's rule read
// on the logging path: the config struct is logged whole at start-up, so a
// token that rendered itself would be in every deployment's first log line.
func TestTheVaultsCredentialNeverReachesTheConfigLog(t *testing.T) {
	t.Parallel()

	const token = "s.averyrealopenbaotoken"
	cfg := config.CashbackConfig{
		PayoutVaultURL:   "https://openbao.example:8200",
		PayoutVaultToken: config.NewSecret(token),
	}
	rendered := cfg.LogValue().String()

	if strings.Contains(rendered, token) {
		t.Errorf("the configuration logs its vault token: %s", rendered)
	}
	if !strings.Contains(rendered, "payout_vault_token_set=true") {
		t.Errorf("the configuration does not say whether a vault token is set: %s", rendered)
	}
}
