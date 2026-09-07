package main

// Answering what this deployment is connected to (T228).
//
// The ops module reads the rows; two of the answers are not rows.
//
//	driver_shipped      is this binary able to poll that network at all?
//	credential_present  did anybody set the key the row names?
//
// The first is a fact about the build and the second about the environment,
// and the composition root is the only place permitted to know either -
// internal/arch/network_isolation_test.go rule A forbids a domain package
// naming an adapter, and ADR-0003 keeps credentials out of every layer that
// could persist or serve one. So this type sits here, joins the store's read
// to what the root knows, and hands the ops module a finished answer.

import (
	"context"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// networkStore is the read this decorator wraps. *ops.PGStore satisfies it.
type networkStore interface {
	ConnectedNetworks(ctx context.Context) ([]ops.ConnectedNetwork, error)
}

// networkInspector answers ops.NetworkInspector by annotating the stored rows
// with the two facts only this package holds.
type networkInspector struct {
	cfg   config.CashbackConfig
	store networkStore
}

// ConnectedNetworks reads the seeded networks and says, per row, whether this
// build can poll them and whether their credential is set.
//
// The credential is never read for its value, only asked whether it has one.
// config.NetworkConfig.MissingKeys is what answers that, and it is the same
// check CashbackConfig.Mountable uses to decide whether to build the cashback
// surface at all - so this endpoint and that decision cannot disagree about
// which network is usable, which would be the worst possible thing for a
// screen an operator consults when nothing is happening.
//
// A network with a row but no entry in NETWORKS has no configuration block at
// all. Its credential is reported absent, which is true: nothing names a key
// for it, so nothing could have set one.
func (i networkInspector) ConnectedNetworks(ctx context.Context) ([]ops.ConnectedNetwork, error) {
	networks, err := i.store.ConnectedNetworks(ctx)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]config.NetworkConfig, len(i.cfg.Networks))
	for _, network := range i.cfg.Networks {
		configured[network.Driver] = network
	}
	for index := range networks {
		network := &networks[index]
		_, shipped := shippedNetworks[network.ID]
		network.DriverShipped = shipped

		block, named := configured[network.ID]
		// Usable() is len(MissingKeys()) == 0, and MissingKeys covers the
		// account id and the source language as well as the credential
		// itself. That is deliberate here: this boolean answers "is this
		// account's configuration complete enough to poll with", which is
		// the question an operator staring at a silent network is actually
		// asking. The individual missing keys are named at start-up, at
		// ERROR, by reportNetworkConfiguration.
		present := named && block.Usable()
		for account := range network.Accounts {
			network.Accounts[account].CredentialPresent = present
		}
	}
	return networks, nil
}
