// BuildDeeplink: contract rule 5 held for Awin, and the one refusal Awin's
// own documentation adds to the port's. One file, because assembling a
// redirect is the only thing this adapter does that never contacts Awin, and
// it is the only place where getting it wrong costs a member money while
// looking to them like success.

package awin

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// PlaceholderMarker delimits Awin's dynamic parameters, which they write as
// !!!name!!! - !!!id!!! for the publisher id, !!!clickref!!! for the click
// reference, and so on.
//
// It appears here because a publisher's own deeplink must never contain one.
// Awin's Link Builder emits two kinds of tracking link: a specific one, with
// the publisher id substituted, and a GENERAL one that carries a literal
// !!!id!!! their documentation says "partners will have to manually replace
// with their own unique Publisher ID". The two are a paste apart, and only
// one of them earns anybody money.
const PlaceholderMarker = "!!!"

// BuildDeeplink returns the URL a click-out redirect sends the member to,
// with ref placed in the network's own click-reference parameter (contract
// rule 5, FR-021). For Awin that parameter is clickref, but the name is read
// from the route rather than written here: it is per-network configuration,
// and a wrong value silently loses attribution on every click.
//
// It is assembled locally and never through Awin's Link Builder API, which
// would also return a ready-made tracking URL. The reason is the rate limit.
// A publisher account gets twenty API calls a minute in total, and the link
// builder is rationed further behind its own quota endpoint - an endpoint
// that exists because the quota runs out. Calling it here would cap the
// product at twenty click-outs a minute across all members, spend the budget
// the transaction poller needs, and turn every Awin outage into an outage of
// the one action a member takes. Assembling from the offer's own template
// costs nothing and cannot fail.
//
// The order of what it does is the contract. Everything checkable about the
// inputs is refused first, by the port's own [networks.ValidateDeeplinkInputs],
// before a single character of URL exists - so there is no state in which a
// partially-formed URL could be returned by accident.
//
// Every refusal wraps [networks.ErrDeeplinkNotFormed], which is what a caller
// deciding "do not redirect this member" tests. Every refusal here is also
// deterministic and so also wraps [networks.ErrDeeplinkInputsRefused]: nothing
// in this method contacts Awin, so no failure it can report is Awin's.
func (c *Client) BuildDeeplink(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	if err := networks.ValidateDeeplinkInputs(ID, target, ref); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		// A caller whose request has already been abandoned should not be
		// handed a URL it is about to redirect nobody to. This is neither
		// our routing bug nor Awin's fault, so it carries only the umbrella
		// and the cause.
		return "", fmt.Errorf("%w: offer %s: %w", networks.ErrDeeplinkNotFormed, target.OfferID, err)
	}
	if err := refuseUnreplacedPlaceholder(target); err != nil {
		return "", err
	}
	return networks.AppendClickRef(target, ref)
}

// refuseUnreplacedPlaceholder refuses a template still carrying one of Awin's
// !!!name!!! dynamic parameters.
//
// This is the one refusal Awin adds to the port's own, and it is here because
// the failure it prevents is invisible from every other vantage point. A
// general tracking link pasted into offer.deeplink_template redirects
// perfectly: Awin accepts it, the member reaches the retailer and buys, and
// the click is attributed to the publisher named by the literal string
// "!!!id!!!", which is nobody. Nothing goes red. The first evidence is a
// month of clicks that reported no transactions, by which time the members
// who made them have been told they would be paid.
//
// A single marker is enough to refuse on, rather than a matched pair: a
// half-typed placeholder is as wrong as a whole one, and no URL that belongs
// in this column has three exclamation marks in it. A retailer URL that did
// would be refused here, loudly, naming the offer - which is the cheaper of
// the two mistakes by a wide margin.
func refuseUnreplacedPlaceholder(target networks.DeeplinkTarget) error {
	if !strings.Contains(target.Template, PlaceholderMarker) {
		return nil
	}
	return fmt.Errorf("%w: %w: offer %s carries a deeplink template with an unreplaced Awin placeholder, which would attribute every click to nobody: %s",
		networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID,
		strconv.Quote(namePlaceholder(target.Template)))
}

// namePlaceholder returns the placeholder an operator has to go and fix,
// whole if it is whole and from the marker onwards if it is not. A refusal
// that says only "there is a placeholder" sends somebody to read a URL
// character by character.
func namePlaceholder(template string) string {
	start := strings.Index(template, PlaceholderMarker)
	if start < 0 {
		// Unreachable: the only caller has already found a marker. It is
		// not unreachable to the compiler, and a slice built from -1 is a
		// panic in the click-out path.
		return ""
	}
	rest := template[start+len(PlaceholderMarker):]
	end := strings.Index(rest, PlaceholderMarker)
	if end < 0 {
		return template[start:]
	}
	return template[start : start+len(PlaceholderMarker)+end+len(PlaceholderMarker)]
}
