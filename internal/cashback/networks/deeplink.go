// [DeeplinkTarget], the route a redirect points at, and contract rule 5's
// checkable half. One file, because the target exists to be validated: every
// field on it is a field [ValidateDeeplinkInputs] refuses a redirect over.

package networks

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// The sentinels a redirect is refused with. These two are wrapped TOGETHER
// where one answer is not enough: a refused redirect says both "do not
// redirect this member" and which kind of refusal it was, because those are
// different questions with different answers.
var (
	// ErrDeeplinkNotFormed reports a redirect that could not be built
	// (contract rule 5). Every refusal from BuildDeeplink wraps it, so a
	// caller that only needs to know "do not redirect this member" tests one
	// sentinel. An error is the only safe answer: a partially-formed URL
	// still redirects, so the member reaches the retailer, buys, and is never
	// credited - the failure mode that costs a member real money while
	// looking to them like success.
	ErrDeeplinkNotFormed = errors.New("networks: deeplink could not be formed")
	// ErrDeeplinkInputsRefused is wrapped BESIDE [ErrDeeplinkNotFormed] by
	// every refusal [ValidateDeeplinkInputs] makes, and says the refusal is
	// deterministic: what the adapter was handed cannot describe a redirect,
	// so the same call will fail the same way forever and no retry helps.
	//
	// The distinction is not decoration. Two of these refusals - an offer
	// routed to the wrong adapter, a reference that was never minted - are
	// bugs in our own code, and the rest are a route somebody has to fix;
	// none of them is the network being unwell. Without a second sentinel the
	// click-out handler answers 502 (contracts/http-api.md) and the on-call
	// is paged towards a network that is working perfectly, while the defect
	// is one line in the handler. A caller may also use it for the other
	// half: an offer whose deeplink can NEVER be formed is one to stop
	// publishing, and an offer whose network is merely unreachable is not.
	ErrDeeplinkInputsRefused = errors.New("networks: the inputs do not describe a redirect")
)

// DeeplinkTarget is the retailer route a redirect points at: the four facts a
// deeplink is assembled from, and nothing else.
//
// It is deliberately not the catalogue's Offer. An Offer carries the whole
// commercial band - the rate, the member's share, the conditions, both
// validity bounds - and it is a value the catalogue package maps out of its
// generated sqlc store, so a port taking one would put the Postgres driver
// and the store's types into the dependency graph of every adapter written
// from now on. Contract rule 6 says adapters never write to the database;
// handing each of them the driver is not how that rule is kept. The rate band
// matters just as much: an adapter that can read MemberShare can log it, or
// append it to a network subid "to help reconciliation", and rule 6 says an
// adapter decides nothing about the rate. What it cannot see, it cannot
// decide from.
//
// The click-out flow builds one of these from the offer it just snapshotted
// onto the click, which is the same place FR-013's click-time band is fixed.
type DeeplinkTarget struct {
	// OfferID is the band the click is issued against. Nothing in a redirect
	// is built from it; it is here so a refusal names the offer an operator
	// has to go and look at.
	OfferID uuid.UUID
	// NetworkID is the network this route is published on. The band decides
	// which network a click is issued through (migration 0011), so this is
	// what [ValidateDeeplinkInputs] compares against the adapter's own id.
	NetworkID NetworkID
	// ClickRefParam is the query parameter this network reads the click
	// reference from (FR-021), from the network's row rather than from a
	// literal in the adapter: a wrong value silently loses attribution on
	// every click.
	ClickRefParam string
	// Template is the deeplink template the redirect is assembled from
	// (offer.deeplink_template). Operator-edited data, so it is parsed
	// rather than trusted - see [ValidateDeeplinkInputs].
	Template string
}

// ValidateDeeplinkInputs is contract rule 5's checkable half: everything a
// redirect needs before a single character of URL is assembled. Adapters
// call it at the top of BuildDeeplink, so every refusal happens before any
// string is built and no partially-formed URL can exist to be returned by
// accident.
//
// It refuses five things, each of which produces a redirect that works and
// loses the money, or one that should never have been issued at all:
//
//   - an adapter with no usable network id of its own. The id is what a
//     stored row is traced back to, so an adapter that cannot name itself
//     cannot have its clicks reconciled.
//   - a target published on another network. The band decides which network
//     the click is issued through (migration 0011), so a target reaching the
//     wrong adapter means a click assembled from one network's template and
//     recognised by neither.
//   - a target whose template is not an absolute http or https URL. The
//     column only requires it to be non-blank, it is operator-edited, and
//     what comes back from BuildDeeplink is written straight into a Location
//     header - so a relative path, a padded value or a javascript: scheme is
//     refused here rather than emitted to a browser.
//   - a target with no click-reference parameter (FR-021). The redirect
//     would reach the retailer with no attribution, so the member buys and
//     nothing ever comes back.
//   - a click reference that was never minted. FR-020 requires the click
//     record and its unguessable reference to exist BEFORE the redirect is
//     issued, so a missing one here means the redirect is being built out of
//     order. A reference that exists cannot be malformed - [IssuedClickRef]
//     refuses that at construction - so there is nothing else to check.
//
// Every failure wraps [ErrDeeplinkNotFormed], which is what contract rule 5
// promises and what a caller deciding "do not redirect this member" tests.
// Every failure ALSO wraps [ErrDeeplinkInputsRefused], which says the refusal
// is deterministic rather than the network being unwell - see that sentinel
// for why the difference is worth a second errors.Is.
func ValidateDeeplinkInputs(id NetworkID, target DeeplinkTarget, ref IssuedClickRef) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("%w: %w: the adapter names no network: %w",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, err)
	}
	if target.NetworkID != id {
		return fmt.Errorf("%w: %w: offer %s is published on network %s, not %s",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID,
			strconv.Quote(target.NetworkID.String()), strconv.Quote(id.String()))
	}
	if err := validateDeeplinkTemplate(target); err != nil {
		return err
	}
	if strings.TrimSpace(target.ClickRefParam) == "" {
		return fmt.Errorf("%w: %w: offer %s names no click-reference parameter",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID)
	}
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%w: %w: offer %s has no click reference to pass to the network: %w",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID, err)
	}
	return nil
}

// validateDeeplinkTemplate holds a template to what a Location header may
// carry: present, unpadded, and an absolute http or https URL with a host.
// The column checks only that it is not blank (migration 0011), and the value
// is operator-edited, so this is the only place between an UPDATE and a
// member's browser where a relative path or a javascript: scheme is refused.
func validateDeeplinkTemplate(target DeeplinkTarget) error {
	if strings.TrimSpace(target.Template) == "" {
		return fmt.Errorf("%w: %w: offer %s carries no deeplink template",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID)
	}
	if target.Template != strings.TrimSpace(target.Template) {
		return fmt.Errorf("%w: %w: offer %s carries a deeplink template padded with space: %s",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID, strconv.Quote(target.Template))
	}
	parsed, err := url.Parse(target.Template)
	if err != nil {
		return fmt.Errorf("%w: %w: offer %s carries a deeplink template that is not a URL: %w",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID, err)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: %w: offer %s carries a deeplink template that is not an absolute http or https URL: %s",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID, strconv.Quote(target.Template))
	}
	return nil
}

// AppendClickRef puts ref in the target's click-reference parameter and
// returns the absolute URL that results: the assembly half of contract rule
// 5, for every adapter whose network reads its reference from a query
// parameter on an operator-configured template.
//
// It lives here rather than in each adapter because both rules it keeps are
// rules an adapter gets wrong silently, and the second adapter to be written
// would have to rediscover them.
//
// The first is that the template's own query is carried through byte for
// byte rather than re-encoded. Re-encoding is what url.Values.Encode does,
// and it reorders parameters and normalises their escaping - a change to a
// value an operator was told this system would pass through, made to data
// some networks are famously fussy about. Appending leaves the template
// exactly as it was written and adds one pair.
//
// The second is that a template already carrying the click-reference
// parameter is refused rather than given a second one. Which of two
// same-named parameters a network reads is its own business, so the value
// that decides whether a member is credited would be chosen by the network's
// parser rather than by us.
//
// It assumes [ValidateDeeplinkInputs] has already run and returns errors
// wrapping [ErrDeeplinkNotFormed] and [ErrDeeplinkInputsRefused], since
// everything it can still refuse is deterministic.
func AppendClickRef(target DeeplinkTarget, ref IssuedClickRef) (string, error) {
	parsed, err := url.Parse(target.Template)
	if err != nil {
		// ValidateDeeplinkInputs parsed this same string successfully a
		// moment ago, so this branch is unreachable in practice. It is not
		// unreachable to the compiler, and a deeplink that silently ignored
		// a parse failure is the exact shape of bug this function exists to
		// make impossible.
		return "", fmt.Errorf("%w: %w: offer %s carries a deeplink template that is not a URL: %w",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID, err)
	}
	if parsed.Query().Has(target.ClickRefParam) {
		return "", fmt.Errorf("%w: %w: offer %s carries a deeplink template that already sets %s, and a second one would be the value the network ignores",
			ErrDeeplinkNotFormed, ErrDeeplinkInputsRefused, target.OfferID,
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
