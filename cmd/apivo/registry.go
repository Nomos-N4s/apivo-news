package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// A network driver is known here, once, or it is not known at all.
//
// It used to be known in two places that disagreed. `networkAdapter` shipped
// the fixture; `documentedNetwork` shipped the fixture AND awin. So
// `connect-network --driver awin` wrote a cashback.network row and then the
// server refused to start against it - a deployment seeded for a network the
// binary cannot poll, which is FR-092's defect exactly. Nothing caught it,
// because nothing compared the two lists.
//
// One map removes the possibility rather than the instance. A driver is
// servable and seedable together or neither, because both answers come from
// the same entry.
//
// It lives in the composition root and not in the domain because
// internal/arch/network_isolation_test.go rule A forbids a domain package
// naming an adapter. That rule is what makes SC-008 - a second network costs
// one package and one table row - checkable rather than aspirational.

// registration is what this binary knows about one shipped network driver:
// what the network publishes about itself, and how to build the adapter.
//
// Both halves are required. A driver that could supply one and not the other
// is the state this type exists to make unrepresentable.
type registration struct {
	// documented is what a cashback.network row is seeded with - the facts
	// the network publishes about how it may be queried.
	documented func() networks.Documented

	// construct builds the adapter for one publisher account.
	construct func(networks.PublisherAccount) (networks.Network, error)
}

// shippedNetworks is every driver this binary has.
//
// Membership is a compile-time fact: an adapter is a release, not a
// configuration change. config.validateNetworkDriver checks only the SHAPE of
// NETWORK_DRIVER and says so - it defers membership here deliberately, so
// that a typo fails by name rather than by finding nothing.
//
// awin is absent, and that is the point. *awin.Client does not implement
// networks.Network: it has no FetchTransactions and no Limits, proved at the
// compiler. Listing it here would restore the seedable-but-not-servable state
// above. It returns when it implements the port (T236-T241, deferred by
// founder decision of 2026-09-04).
var shippedNetworks = map[string]registration{
	config.NetworkDriverFixture: {
		documented: fixture.Documented,
		construct:  newFixtureAdapter,
	},
}

// newFixtureAdapter adapts fixture.New's concrete return to the port.
//
// fixture.New returns *fixture.Network so its own tests can reach the knobs
// the conformance table needs. The registry holds the interface, because the
// composition root has no business knowing which one it built.
func newFixtureAdapter(account networks.PublisherAccount) (networks.Network, error) {
	return fixture.New(account)
}

// errNoDriverNamed reports a lookup with no driver to look up. It is
// separated from "not a driver this binary has" because the two are different
// operator mistakes: nothing was configured, versus something was.
var errNoDriverNamed = errors.New("NETWORK_DRIVER names no adapter, and it is the network this would connect")

// lookupNetwork resolves a driver name to its registration.
//
// An unknown name is refused by name, listing what the binary does ship. A
// deployment that believes it is connected to a network the binary cannot
// poll is worse than one that will not start.
func lookupNetwork(driver string) (registration, error) {
	if driver == "" {
		return registration{}, errNoDriverNamed
	}
	entry, ok := shippedNetworks[driver]
	if !ok {
		return registration{}, fmt.Errorf(
			"NETWORK_DRIVER %s is not an adapter this binary has; it ships %s, and a network's own adapter is a release rather than a configuration change",
			strconv.Quote(driver), shippedDrivers())
	}
	return entry, nil
}

// shippedDrivers names every shipped driver, sorted, for an error message.
// Sorted because map iteration is not, and an error that reads differently on
// each run is one nobody can search for.
func shippedDrivers() string {
	names := make([]string, 0, len(shippedNetworks))
	for name := range shippedNetworks {
		names = append(names, strconv.Quote(name))
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "no adapter at all"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// theConfiguredNetwork picks the one network this composition root wires.
//
// NETWORKS is a list, and config parses all of it - but the root still
// builds ONE adapter, one poller and one catalogue import. Making those
// plural is T220-T224; until then this is where the list narrows, in one
// named place rather than at each of the four call sites that used to read
// the flat keys.
//
// The FIRST entry, not the first USABLE one. It briefly scanned for a usable
// entry, so that a deployment whose leading network was missing a credential
// would poll the one behind it. That is no longer what happens:
// [config.CashbackConfig.Mountable] declines to build the cashback surface
// at all while any configured network is unusable, so by the time the server
// asks this question every entry is usable and "the first" and "the first
// usable" are the same network. Scanning would only hide the day they
// stopped being.
//
// ok is false when NETWORKS is empty. That is not an error: a deployment
// that polls nothing still serves the wallet, the money loop and click-outs
// on an existing catalogue.
//
// Callers that are NOT behind the Mountable gate - connect-network is the
// one - must check [config.NetworkConfig.MissingKeys] themselves. Returning
// an unusable network to them is deliberate: the subcommand can then name
// every key that is missing, which is more use than being told the list is
// empty.
func theConfiguredNetwork(cfg config.CashbackConfig) (config.NetworkConfig, bool) {
	if len(cfg.Networks) == 0 {
		return config.NetworkConfig{}, false
	}
	return cfg.Networks[0], true
}

// reportNetworkConfiguration says, once, what this deployment will and will
// not poll, and what that costs.
//
// Called on CASHBACK_ENABLED - the operator's INTENT - rather than on
// Mountable, because the case worth reporting most is exactly the one where
// cashback does not mount: an operator who set CASHBACK_ENABLED=true and
// finds no cashback routes has to read why here rather than deduce it from a
// 404.
func reportNetworkConfiguration(ctx context.Context, log *slog.Logger, cfg config.CashbackConfig) {
	unusable := cfg.UnusableNetworks()
	for _, network := range unusable {
		log.ErrorContext(ctx, "a configured network cannot poll", "problem", network.String())
	}
	if len(unusable) > 0 {
		// The consequence, said separately from the cause. FR-091 asks for
		// the network named; the founder's decision of 2026-09-04 is that
		// naming it is not enough on its own, because "one network is
		// misconfigured" and "the cashback product is not running" are
		// different sizes of news and an operator must not have to infer
		// the second from the first.
		log.ErrorContext(ctx,
			"CASHBACK IS NOT MOUNTED: every cashback route will answer 404 and no member can click out, because a configured network cannot poll. "+
				"The rest of this deployment - the news site included - is unaffected and keeps serving",
			"configured", len(cfg.Networks), "unusable", len(unusable), "drivers", unusableDrivers(unusable))
		return
	}
	if extra := len(cfg.Networks) - 1; extra > 0 {
		log.ErrorContext(ctx,
			"more than one network is configured and this build wires only the first; the rest are not polled",
			"wired", cfg.Networks[0].Driver, "not_wired", extra)
	}
}

// unusableDrivers names them in the order NETWORKS did, for the one line
// that reports the consequence rather than each cause.
func unusableDrivers(unusable []config.UnusableNetwork) string {
	names := make([]string, 0, len(unusable))
	for _, network := range unusable {
		names = append(names, network.Driver)
	}
	return strings.Join(names, ", ")
}
