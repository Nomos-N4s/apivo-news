// BuildDeeplink: contract rule 5 held by this adapter, and the one refusal it
// adds to the port's own. One file, because assembling a redirect is the only
// thing here that is not reading a recording, and it is the only place where
// getting it wrong costs a member money while looking to them like success.

package fixture

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// BuildDeeplink returns the URL a click-out redirect sends the member to,
// with ref placed in the network's own click-reference parameter (contract
// rule 5, FR-021).
//
// The order of what it does is the contract. Everything checkable about the
// inputs is refused first, by the port's own [networks.ValidateDeeplinkInputs],
// before a single character of URL exists - so there is no state in which a
// partially-formed URL could be returned by accident. Only then is the
// injected failure consulted, and only then is anything assembled.
//
// Every refusal wraps [networks.ErrDeeplinkNotFormed], which is what a caller
// deciding "do not redirect this member" tests. A refusal that is
// deterministic - our own routing bug, or a route somebody has to fix - also
// wraps [networks.ErrDeeplinkInputsRefused], and one caused by the network
// wraps the classification from contract rule 9 instead. The difference is
// what decides whether the on-call is paged towards a network or towards us,
// and whether the offer is one to stop publishing.
//
// The context is honoured even though nothing here contacts a network: a
// caller whose request has already been abandoned should not be handed a URL
// it is about to redirect nobody to, and a cancelled context is a third kind
// of refusal - neither our routing bug nor the network's fault - so it wraps
// only [networks.ErrDeeplinkNotFormed] and the cause.
func (n *Network) BuildDeeplink(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	if err := networks.ValidateDeeplinkInputs(ID, target, ref); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: offer %s: %w", networks.ErrDeeplinkNotFormed, target.OfferID, err)
	}
	if err := n.failures.take(); err != nil {
		return "", fmt.Errorf("%w: offer %s: %w", networks.ErrDeeplinkNotFormed, target.OfferID, err)
	}
	return appendClickRef(target, ref)
}

// appendClickRef puts the reference in the target's click-reference parameter
// and returns the absolute URL that results.
//
// The template's own query is carried through byte for byte rather than
// re-encoded. Re-encoding is what url.Values.Encode does, and it reorders
// parameters and normalises their escaping - which is a change to a value an
// operator was told this system would pass through, made to data some
// networks are famously fussy about. Appending leaves the template exactly as
// it was written and adds one pair.
func appendClickRef(target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	parsed, err := url.Parse(target.Template)
	if err != nil {
		// ValidateDeeplinkInputs parsed this same string successfully a
		// moment ago, so this branch is unreachable in practice. It is not
		// unreachable to the compiler, and a deeplink that silently ignored
		// a parse failure is the exact shape of bug this method exists to
		// make impossible.
		return "", fmt.Errorf("%w: %w: offer %s carries a deeplink template that is not a URL: %w",
			networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID, err)
	}
	if parsed.Query().Has(target.ClickRefParam) {
		return "", fmt.Errorf("%w: %w: offer %s carries a deeplink template that already sets %s, and a second one would be the value the network ignores",
			networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID,
			strconv.Quote(target.ClickRefParam))
	}

	pair := url.QueryEscape(target.ClickRefParam) + "=" + url.QueryEscape(ref.Ref())
	if parsed.RawQuery == "" {
		parsed.RawQuery = pair
	} else {
		parsed.RawQuery = parsed.RawQuery + "&" + pair
	}
	return parsed.String(), nil
}
