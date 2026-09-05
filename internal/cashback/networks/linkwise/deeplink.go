// BuildDeeplink: contract rule 5 held for Linkwise, and the two refusals this
// network's own click URL adds to the port's (T246).
//
// One file, because assembling a redirect is the only thing this adapter does
// that never contacts Linkwise, and it is the only place where getting it
// wrong costs a member money while looking to them like success: a
// half-attributed URL still redirects, so the member reaches the retailer,
// buys, and is never credited.

package linkwise

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// subIDSlots is how many sub-id slots Linkwise has: exactly five, named
// subid1 to subid5.
//
// Established twice over. The transaction report's usage text
// (testdata/error-400.json) lists subid1..subid5 both as report fields and as
// the values search_field accepts, and Linkwise's own live click script
// enforces the same bound on the way in - subid(b, a) parses b and acts only
// while it is above zero and below six. So five is a BUDGET rather than a
// field name, and a sixth slot does not exist to be used.
const subIDSlots = 5

// destinationParam is the parameter a Linkwise click URL carries the
// member's actual destination in.
//
// It matters here for one reason: its value is a whole URL nested inside
// another URL's query, and an unescaped one leaks its own query string into
// the outer one - see [refuseUnescapedDestination].
const destinationParam = "lnkurl"

// BuildDeeplink returns the URL a click-out redirect sends the member to,
// with ref placed in the network's own click-reference parameter (contract
// rule 5, FR-021). For Linkwise that parameter is one of the five sub-id
// slots, but which one is read from the route rather than written here: it is
// per-network configuration, and a wrong value silently loses attribution on
// every click.
//
// It is assembled locally and never through Linkwise's rest_deeplink.php,
// which would also return a ready-made tracking URL. Two reasons, and the
// second is this network's own. A request costs a second at minimum on this
// API, so calling it here would put a second of network latency in front of
// the one action a member takes and turn every Linkwise slowdown into a
// slowdown of the click-out. And it would make attribution depend on the
// network being reachable at the moment of the click, when the offer's own
// template already carries everything the redirect needs.
//
// The order of what it does is the contract. Everything checkable about the
// inputs is refused first, by the port's own [networks.ValidateDeeplinkInputs],
// before a single character of URL exists - so there is no state in which a
// partially-formed URL could be returned by accident.
//
// Every refusal wraps [networks.ErrDeeplinkNotFormed], which is what a caller
// deciding "do not redirect this member" tests. Every refusal here is also
// deterministic and so also wraps [networks.ErrDeeplinkInputsRefused]: nothing
// in this method contacts Linkwise, so no failure it can report is Linkwise's.
func (c *Client) BuildDeeplink(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	if err := networks.ValidateDeeplinkInputs(ID, target, ref); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		// A caller whose request has already been abandoned should not be
		// handed a URL it is about to redirect nobody to. This is neither our
		// routing bug nor Linkwise's fault, so it carries only the umbrella
		// and the cause.
		return "", fmt.Errorf("%w: offer %s: %w", networks.ErrDeeplinkNotFormed, target.OfferID, err)
	}
	if err := refuseUnreadableSlot(target); err != nil {
		return "", err
	}
	if err := refuseUnescapedDestination(target); err != nil {
		return "", err
	}
	return networks.AppendClickRef(target, ref)
}

// refuseUnreadableSlot refuses a click-reference parameter Linkwise does not
// read.
//
// This is the refusal that earns its keep. cashback.network.click_ref_param
// is an editable column - it exists precisely so an operator can correct it
// without a release - and the value it most plausibly acquires by mistake is
// another network's. Awin's is "clickref"; set that here and every redirect
// still works, every member still reaches the retailer, and Linkwise reports
// every transaction with all five sub-ids null. Nothing fails. The first
// evidence is a month of clicks that reported no attribution, by which time
// the members who made them have been told they would be paid.
//
// The set is closed and small, so the refusal names the whole of it: an
// operator reading this has to type the right value next, and "unknown
// parameter" alone sends them to documentation this network does not publish.
func refuseUnreadableSlot(target networks.DeeplinkTarget) error {
	if slotNumber(target.ClickRefParam) > 0 {
		return nil
	}
	return fmt.Errorf("%w: %w: offer %s would place the click reference in %s, which Linkwise does not read; it has exactly %d slots, %s, and a reference in anything else is lost on every click",
		networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID,
		strconv.Quote(target.ClickRefParam), subIDSlots, slotNames())
}

// slotNumber returns which sub-id slot param names, or zero if it names none.
//
// Written as a parse rather than a set membership test so that the bound is
// the same shape as the one Linkwise's own script enforces - above zero and
// below six - rather than a list that could drift from it.
//
// The parse must be EXACT, which strconv.Atoi alone is not: it reads "01" and
// "+1" as one, and neither is a parameter Linkwise reads. A round trip back
// through Itoa is what refuses them - the digits have to be the canonical
// spelling of the number, not merely something that parses to it.
func slotNumber(param string) int {
	const prefix = "subid"
	rest, found := strings.CutPrefix(param, prefix)
	if !found {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > subIDSlots || strconv.Itoa(n) != rest {
		return 0
	}
	return n
}

// slotNames renders the five slots for the refusal above.
func slotNames() string {
	names := make([]string, 0, subIDSlots)
	for i := 1; i <= subIDSlots; i++ {
		names = append(names, "subid"+strconv.Itoa(i))
	}
	return strings.Join(names, ", ")
}

// refuseUnescapedDestination refuses a template whose lnkurl carries an
// unescaped destination.
//
// A Linkwise click URL nests the member's actual destination inside its own
// query: .../?lnkurl=<destination>. If that destination was pasted without
// being escaped, its own query string has already leaked into the outer one -
// "?lnkurl=https://shop.example/x?a=1&b=2" parses as an lnkurl of
// "https://shop.example/x?a=1" and a separate outer parameter b.
//
// The click reference is then appended to the OUTER query, after a fragment
// of somebody else's URL, and which of the two Linkwise reads is not
// something this adapter gets to decide. Refused rather than repaired: an
// adapter that re-escaped the value would be rewriting an operator's route on
// a guess about where the split was meant to be, and a template this broken
// is one somebody has to look at.
//
// A question mark inside the value is what gives it away, because a correctly
// escaped destination has none - %3F is what an escaped one carries.
//
// It has to be read off the RAW query. url.Values.Get decodes, so it hands
// back a question mark for %3F and for a literal one alike, and a check
// written against it would refuse every correctly escaped destination while
// passing none of the broken ones differently. That was not a hypothetical:
// this function was written that way first, and the test for the escaped case
// is what caught it.
func refuseUnescapedDestination(target networks.DeeplinkTarget) error {
	parsed, err := url.Parse(target.Template)
	if err != nil {
		// Unreachable: ValidateDeeplinkInputs parsed this same string a
		// moment ago. Not unreachable to the compiler, and a deeplink that
		// ignored a parse failure is the shape of bug this file exists to
		// prevent.
		return fmt.Errorf("%w: %w: offer %s carries a deeplink template that is not a URL: %w",
			networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID, err)
	}
	raw, found := rawDestination(parsed.RawQuery)
	if !found || !strings.Contains(raw, "?") {
		return nil
	}
	return fmt.Errorf("%w: %w: offer %s carries a deeplink template whose %s was not escaped - its own query string has leaked into Linkwise's, so the click reference would be appended after a fragment of the retailer's URL: %s",
		networks.ErrDeeplinkNotFormed, networks.ErrDeeplinkInputsRefused, target.OfferID,
		strconv.Quote(destinationParam), strconv.Quote(raw))
}

// rawDestination returns the lnkurl parameter's value as it was written,
// still percent-encoded, and whether the query carried one at all.
func rawDestination(rawQuery string) (string, bool) {
	for segment := range strings.SplitSeq(rawQuery, "&") {
		if value, found := strings.CutPrefix(segment, destinationParam+"="); found {
			return value, true
		}
	}
	return "", false
}
