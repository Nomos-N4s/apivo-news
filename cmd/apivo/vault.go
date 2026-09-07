package main

// Choosing where a member's payout details live (ADR-0006).
//
// The composition root is the only place that may name the vendor, exactly
// as it is the only place that names a ledger driver or a network adapter.
// Everything above this file speaks [payout.DetailsVault] and never learns
// what is behind it.

import (
	"log/slog"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/openbao"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// newDetailsVault builds the vault this deployment configured, or nil when
// it configured none.
//
// A nil vault is not an error, and this is the one place that decision is
// visible. The alternative - refusing to start - would take the whole
// cashback surface down over an endpoint that only new destinations need,
// and a deployment paying members to destinations recorded earlier would
// stop paying them. So an unconfigured vault is a loud line at start-up and
// a 503 on one route, which is the same stance the wallet takes on a
// missing payout threshold.
//
// An endpoint that is SET and unusable IS a startup failure. A deployment
// that named a vault meant it, and starting anyway would answer 503 while
// an operator believed destinations were being recorded.
func newDetailsVault(log *slog.Logger, cfg config.Config) (payout.DetailsVault, error) {
	endpoint := cfg.Cashback.PayoutVaultURL
	if endpoint == "" {
		log.Error("PAYOUT_VAULT_URL is unset, so this deployment has nowhere to put payout details: POST /api/v1/cashback/payout-destinations will answer 503 and no member can add somewhere to be paid. Every other payout route is unaffected",
			"key", "PAYOUT_VAULT_URL")
		return nil, nil
	}

	opts := []openbao.Option{openbao.WithToken(cfg.Cashback.PayoutVaultToken.Reveal())}
	if mount := cfg.Cashback.PayoutVaultMount; mount != "" {
		opts = append(opts, openbao.WithMount(mount))
	}
	vault, err := openbao.New(endpoint, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Cashback.PayoutVaultToken.IsZero() {
		// Not refused here, because a vault may legitimately run
		// unauthenticated on a private network, exactly as the Blnk
		// sidecar may. Said out loud because the far likelier cause is a
		// key nobody set, and the symptom would otherwise be every member
		// being told their details were refused.
		log.Warn("PAYOUT_VAULT_TOKEN is unset: payout details will be written unauthenticated, and a vault with authentication on will refuse every destination",
			"key", "PAYOUT_VAULT_TOKEN")
	}
	log.Info("payout details vault configured", "mount", cfg.Cashback.PayoutVaultMount)
	return vault, nil
}
