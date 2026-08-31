// FetchCatalogue: Awin's programme list mapped to the port's routes, and the
// two requests it takes to make absence mean what an import reads it as. One
// file, because a catalogue is a current state rather than a period and
// shares none of the window reasoning a transaction read needs.

package awin

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
	"strconv"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// programmesPath is the endpoint the catalogue is read from, relative to the
// base URL. Awin documents it as GET /publishers/{publisherId}/programmes,
// answering a bare JSON array with no envelope and no pagination.
//
//	https://help.awin.com/apidocs/get-program-information
const programmesPath = "publishers/%s/programmes"

// The membership values Awin's relationship filter takes, in their own
// spelling. The five they document are joined, pending, suspended, rejected
// and notjoined; only the two below name a route that exists between this
// publisher account and an advertiser.
const (
	membershipJoined    = "joined"
	membershipSuspended = "suspended"
)

// catalogueMemberships is what a catalogue read asks for, in order.
//
// Only these two, because an import reads absence as departure: a programme
// missing from the answer is reconciled to left_network and its offers stop
// being published. Asking only for joined would therefore report every
// suspension as a retailer who had left the network - and a suspension is
// exactly the reversible state MerchantStatusPaused exists for. The other
// three memberships are not routes: pending is an application, rejected and
// notjoined are refusals, and none of them can be clicked through.
//
// The order matters and is the conservative one. A programme returned under
// both - which should not happen, since membership is one value - is yielded
// twice and the LAST answer is what an upsert keeps, so suspended wins over
// joined rather than the other way round. Deduplicating instead would mean
// silently dropping one of two contradictory answers, which is worse: it
// hides the contradiction and picks a winner by iteration order anyway.
var catalogueMemberships = []string{membershipJoined, membershipSuspended}

// The programme states Awin reports in the response itself, as opposed to the
// membership above, which is the request's filter and appears nowhere in what
// comes back.
const (
	// programmeActive is the only value of status Awin documents beside
	// hidden ("Program status. Can be active or hidden").
	programmeActive = "active"
	// linksOffline is the value of linkStatus that says a programme's links
	// are down: "current status of links, which can be either online or
	// offline - this applies only to programs that are funded through
	// prepayment funds". A programme whose prepayment is exhausted is live
	// in every other respect and pays nothing.
	linksOffline = "offline"
)

// programme is the documented shape of one element of Awin's programme array.
//
// Only the fields the port needs are decoded. Everything Awin sends is kept
// anyway, verbatim, in [networks.ReportedMerchant.RawPayload] (FR-012), which
// is what a later normalisation fix is re-derived from - so a field left out
// here is a field this adapter does not yet read, never a field it discards.
type programme struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	LinkStatus    string `json:"linkStatus"`
	PrimaryRegion struct {
		CountryCode string `json:"countryCode"`
	} `json:"primaryRegion"`
}

// FetchCatalogue yields every retailer this publisher account can promote on
// Awin, each carrying its verbatim payload (FR-012) and each already through
// [networks.ReportedMerchant.Validate] (contract rule 7).
//
// It takes no window and returns no immediate error: a catalogue is a current
// state rather than a period, so there is nothing checkable before contacting
// Awin, and every failure - including one on the first request - is yielded
// (contract rule 9).
//
// Completeness is the whole contract here, which is why contract rule 8
// matters more to this method than to any other. An import reads absence as
// departure, so an answer that stopped early would have it mark live routes
// departed, stop publishing their offers, and empty the catalogue members
// see - from an import that reported nothing wrong. Every path that stops
// before both memberships have been read whole yields
// [networks.AbandonedIteration] first.
//
// It costs two of the account's twenty requests a minute. Awin returns the
// array unpaginated, so the count does not grow with the catalogue.
func (c *Client) FetchCatalogue(ctx context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	return func(yield func(networks.ReportedMerchant, error) bool) {
		for _, membership := range catalogueMemberships {
			if err := ctx.Err(); err != nil {
				yield(networks.ReportedMerchant{}, c.abandonedCatalogue(err))
				return
			}
			if !c.yieldMembership(ctx, membership, yield) {
				return
			}
		}
	}, nil
}

// yieldMembership reads one membership's programmes and yields them,
// reporting whether iteration should continue - false both for a caller that
// broke and for an error already yielded, which are the same instruction to
// the loop above.
func (c *Client) yieldMembership(ctx context.Context, membership string, yield func(networks.ReportedMerchant, error) bool) bool {
	path := fmt.Sprintf(programmesPath, c.account.ExternalID())
	body, err := c.Get(ctx, path, url.Values{"relationship": {membership}})
	if err != nil {
		// Get has already classified this per contract rule 9, and a
		// half-read catalogue is a partial answer whatever stopped it - so
		// rule 8's marker goes on as well, or an import would reconcile
		// against what it did not finish reading.
		yield(networks.ReportedMerchant{}, c.abandonedCatalogue(fmt.Errorf("the %s programmes: %w", membership, err)))
		return false
	}

	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		// Classified retryable rather than terminal, and the body is not
		// repeated. We cannot tell a CDN's HTML error page from Awin
		// changing the shape by looking at the bytes, and only one of those
		// clears on its own - but a catalogue read has no cursor to freeze,
		// so the cost of retrying a permanent change is a poll that keeps
		// failing loudly, while the cost of calling a transient one terminal
		// is ingestion stopped for the whole account.
		yield(networks.ReportedMerchant{}, c.abandonedCatalogue(fmt.Errorf(
			"%w: the %s programmes did not answer the documented array: %w", networks.ErrNetworkUnavailable, membership, err)))
		return false
	}

	for _, raw := range raws {
		if err := ctx.Err(); err != nil {
			yield(networks.ReportedMerchant{}, c.abandonedCatalogue(err))
			return false
		}
		merchant, err := merchantFrom(raw, membership)
		if err != nil {
			yield(networks.ReportedMerchant{}, c.abandonedCatalogue(err))
			return false
		}
		if !yield(merchant, nil) {
			return false
		}
	}
	return true
}

// abandonedCatalogue builds the terminal error contract rule 8 requires when
// a catalogue read stops early. It says catalogue rather than naming a window
// because there is no window to name, and because the sentence an operator
// needs is that the answer was partial - the one thing an import must not
// reconcile against.
func (c *Client) abandonedCatalogue(cause error) error {
	return networks.AbandonedIteration(fmt.Errorf("awin: %s: catalogue: %w", c.account, cause))
}

// merchantFrom turns one programme into the port's value: the route status
// mapped from the membership it was returned under and the two state fields
// on the programme itself (contract rule 2), the verbatim bytes carried
// through (FR-012), and the whole thing through its own Validate before it
// can be yielded (contract rule 7).
func merchantFrom(raw json.RawMessage, membership string) (networks.ReportedMerchant, error) {
	var p programme
	if err := json.Unmarshal(raw, &p); err != nil {
		return networks.ReportedMerchant{}, fmt.Errorf(
			"%w: a %s programme did not answer the documented shape: %w", networks.ErrNetworkUnavailable, membership, err)
	}

	merchant := networks.ReportedMerchant{
		// Awin's programme id is the advertiser id a transaction reports in
		// advertiserId, which is what makes an imported route and a reported
		// transaction match on the same value.
		ExternalID: strconv.FormatInt(p.ID, 10),
		Name:       strings.TrimSpace(p.Name),
		Country:    strings.ToUpper(strings.TrimSpace(p.PrimaryRegion.CountryCode)),
		// The membership, in Awin's own spelling. It is the one fact about
		// the route that the verbatim payload cannot carry - it is the
		// request's filter and appears in no response field - so it is what
		// StatusRaw records (FR-032). The programme's own status and
		// linkStatus are in the payload and re-derivable from it.
		StatusRaw:  membership,
		Status:     mapMerchantStatus(membership, p),
		RawPayload: raw,
	}
	if err := merchant.Validate(); err != nil {
		return networks.ReportedMerchant{}, err
	}
	return merchant, nil
}

// mapMerchantStatus is contract rule 2 for a route: total over what Awin can
// return, and never defaulted.
//
// Only two memberships reach it, and everything that is not plainly live is
// paused. That direction is deliberate. Paused is reversible on the very next
// import, so being wrong costs an offer that reappears within a poll cycle;
// publishing a route that cannot pay costs a member who shopped for cashback
// that was never going to arrive, and finds out weeks later. Awin documents
// what status and linkStatus can be but not what a joined programme in each
// combination will actually pay, so the reversible mistake is the one to
// make.
//
// left_network is not reachable from here and should not be: it is what an
// import concludes from a route's ABSENCE, after an iteration that ended with
// no error at all, and an adapter that returned it would be guessing at a
// fact only the whole answer carries.
func mapMerchantStatus(membership string, p programme) networks.MerchantStatus {
	switch {
	case membership != membershipJoined:
		// Suspended: the relationship is on hold, which is what paused
		// means, and it is the state a reinstatement clears.
		return networks.MerchantStatusPaused
	case !strings.EqualFold(strings.TrimSpace(p.Status), programmeActive):
		// Awin calls the programme something other than active - hidden, or
		// a word they have not documented.
		return networks.MerchantStatusPaused
	case strings.EqualFold(strings.TrimSpace(p.LinkStatus), linksOffline):
		// A prepayment-funded programme whose funds are out. Its links are
		// live to a browser and pay nothing.
		return networks.MerchantStatusPaused
	default:
		return networks.MerchantStatusActive
	}
}
