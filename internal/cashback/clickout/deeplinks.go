package clickout

import (
	"context"
	"errors"
	"fmt"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// ErrNoNetworkAdapter reports an offer published on a network this
// deployment has no adapter for.
//
// It wraps [networks.ErrDeeplinkNotFormed] and [networks.ErrDeeplinkInputsRefused]
// deliberately: no redirect can be built, and the refusal is deterministic
// rather than the network having a bad day - so nobody is paged towards a
// network that is working perfectly. What is wrong is either a binary
// missing an adapter or a band published on a network this deployment does
// not integrate, and both are fixed by a person, not by a retry.
var ErrNoNetworkAdapter = errors.New("clickout: no adapter for this offer's network")

// Redirector is what an adapter must be to build a redirect: it names its
// network, and it builds the URL.
//
// Deliberately narrower than [networks.Network], which every adapter also
// satisfies. The polling half of that port - the catalogue and transaction
// reads - has nothing to do with issuing a redirect, and a registry that
// demanded it would make this constructor unusable by anything that only
// knows how to send a member somewhere.
type Redirector interface {
	// ID names the network this adapter serves, in the vocabulary the
	// network table is keyed by.
	ID() networks.NetworkID
	// BuildDeeplink returns the absolute URL the redirect sends the member
	// to, with the reference in the network's own parameter (FR-021).
	BuildDeeplink(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error)
}

// adapterDeeplinks builds redirects from the adapters a binary was wired
// with, choosing by the network the target names.
//
// The map is fixed at construction. A registry that could be added to while
// serving would be one where two requests for the same offer could take
// different adapters, and an adapter is what decides the shape of the URL a
// member is sent to.
type adapterDeeplinks struct {
	byNetwork map[networks.NetworkID]Redirector
}

// NewDeeplinks builds the [Deeplinks] the composition root wires, over the
// network adapters this binary has. Adapters are keyed by their own id, so
// two adapters claiming one network is a wiring mistake rather than a silent
// last-one-wins.
func NewDeeplinks(adapters ...Redirector) (Deeplinks, error) {
	byNetwork := make(map[networks.NetworkID]Redirector, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("clickout: a nil network adapter cannot build a redirect")
		}
		id := adapter.ID()
		if err := id.Validate(); err != nil {
			return nil, fmt.Errorf("clickout: an adapter cannot name its network: %w", err)
		}
		if _, taken := byNetwork[id]; taken {
			return nil, fmt.Errorf("clickout: two adapters claim network %s; one of them would silently serve every click", id)
		}
		byNetwork[id] = adapter
	}
	return &adapterDeeplinks{byNetwork: byNetwork}, nil
}

// Build assembles the redirect for the target, through the adapter for the
// network the target names.
func (d *adapterDeeplinks) Build(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	adapter, ok := d.byNetwork[target.NetworkID]
	if !ok {
		return "", fmt.Errorf("%w: %w: %w: offer %s is published on %s",
			ErrNoNetworkAdapter, networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused,
			target.OfferID, target.NetworkID)
	}
	return adapter.BuildDeeplink(ctx, target, ref)
}
