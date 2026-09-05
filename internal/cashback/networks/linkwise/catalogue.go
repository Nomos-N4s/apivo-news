// FetchCatalogue: every retailer this publisher account can promote on
// Linkwise, read from programs.html (T245, FR-012).
//
// The endpoint is programs.html and not rest_programs.html, which is what an
// integrator's source code suggested and what the 004 research recorded. The
// real name came from the endpoint's own usage text (see
// testdata/programs-usage-400.html) - which arrived as an HTML page precisely
// because that probe was made without format=json, and is the second
// independent demonstration of why the transport sends it on every request.
//
// COMPLETENESS IS THE WHOLE CONTRACT HERE. An import reads absence as
// departure, so a read that ended early and quietly would have every retailer
// it did not reach marked left_network, every offer on them stop being
// published, and members see an emptied catalogue - from an import that
// reported nothing wrong. Contract rule 8 is what carries that, and this
// adapter has an easy time of it: the whole catalogue arrives in one
// response, so there is no page loop to end halfway through.

package linkwise

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

// catalogueEndpoint is the programme list.
const catalogueEndpoint = "programs.html"

// FetchCatalogue yields every retailer the publisher account can promote,
// each carrying its verbatim payload and each already through
// [networks.ReportedMerchant.Validate] (contract rules 1 and 7).
//
// It asks joined=yes and status=all, and both halves are deliberate.
//
// joined=yes is what makes the answer a catalogue rather than a directory:
// Linkwise lists roughly five hundred advertisers and this account is
// accepted into a few hundred of them, and a route we have not been accepted
// into cannot be promoted whatever its terms say.
//
// status=all is the one that matters, because the endpoint's default is
// status=active. Under the default an advertiser who paused their programme
// simply VANISHES from the answer - and absence is how an import spells
// "left the network". So the default would have every temporary pause read as
// a departure, and every offer on it retired rather than suspended. Asking for
// all of them keeps the paused ones present, carrying their own word for it,
// so the two outcomes stay distinguishable.
func (c *Client) FetchCatalogue(ctx context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	query := url.Values{
		"joined": {"yes"},
		"status": {"all"},
	}

	return func(yield func(networks.ReportedMerchant, error) bool) {
		body, err := c.Get(ctx, catalogueEndpoint, query)
		if err != nil {
			// A catalogue read has no precondition checkable without
			// contacting the network, so every failure travels through the
			// sequence - including one on the very first byte. Abandonment is
			// separated out for the reason rule 8 exists: only it means the
			// answer was never whole.
			yield(networks.ReportedMerchant{}, c.stoppedReading(ctx, err))
			return
		}
		rows, err := decodeRows(body)
		if err != nil {
			yield(networks.ReportedMerchant{}, fmt.Errorf("linkwise: reading the catalogue: %w", err))
			return
		}
		for i, raw := range rows {
			if err := ctx.Err(); err != nil {
				yield(networks.ReportedMerchant{}, networks.AbandonedIteration(
					fmt.Errorf("reading the catalogue: %w", err)))
				return
			}
			merchant, err := translateProgramme(raw)
			if err != nil {
				yield(networks.ReportedMerchant{}, fmt.Errorf("linkwise: programme %d of the catalogue: %w", i+1, err))
				return
			}
			if !yield(merchant, nil) {
				return
			}
		}
	}, nil
}

// stoppedReading classifies a failed catalogue fetch, the same way
// [Client.stopped] does for a window: abandonment if this process was the one
// that stopped, and the network's own failure otherwise.
func (c *Client) stoppedReading(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return networks.AbandonedIteration(fmt.Errorf("reading the catalogue: %w", cause))
	}
	return fmt.Errorf("linkwise: reading the catalogue: %w", err)
}

// programme is one row of the programme list, reduced to what a catalogue
// entry is built from.
//
// The row carries twenty-six fields and this reads six of them. The rest -
// the commission tiers, the categories, the promotion terms, the datafeeds,
// the Greek marketing HTML - are not dropped: every entry keeps its whole row
// verbatim in RawPayload, which is what a later normalisation is re-derived
// from and what the localised merchant copy is built out of.
type programme struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
	URL  string      `json:"url"`
	// Countries is a comma-separated list of alpha-2 codes, and it is
	// frequently long: one recorded programme names two hundred and thirty.
	Countries string `json:"countries"`
	// ProgramStatus is the advertiser's own state for the programme.
	ProgramStatus string `json:"program_status"`
	// AffiliateStatus is whether THIS publisher has been accepted into it.
	// Both have to be good before a route can be promoted, which is why the
	// mapping is keyed on the pair.
	AffiliateStatus string `json:"affiliate_status"`
	// Terms carries the promotion methods the advertiser permits, and
	// cashback is one of them - see [promotable].
	Terms struct {
		PromotionMethods struct {
			CashbackSites struct {
				Allow *bool `json:"allow"`
			} `json:"cashback_sites"`
		} `json:"promotion_methods"`
	} `json:"terms"`
}

// translateProgramme turns one row into a catalogue entry and validates it
// before anybody sees it (contract rule 7).
func translateProgramme(raw json.RawMessage) (networks.ReportedMerchant, error) {
	var row programme
	if err := json.Unmarshal(raw, &row); err != nil {
		return networks.ReportedMerchant{}, fmt.Errorf("%w: it will not parse: %w",
			networks.ErrNetworkUnavailable, err)
	}
	externalID := strings.TrimSpace(row.ID.String())

	statusRaw := describeRouteState(row)
	status, err := mapRouteState(externalID, row)
	if err != nil {
		return networks.ReportedMerchant{}, err
	}

	merchant := networks.ReportedMerchant{
		ExternalID: externalID,
		Name:       strings.TrimSpace(row.Name),
		Country:    soleCountry(row.Countries),
		StatusRaw:  statusRaw,
		Status:     status,
		RawPayload: raw,
	}
	if err := merchant.Validate(); err != nil {
		return networks.ReportedMerchant{}, err
	}
	return merchant, nil
}

// soleCountry returns the one country a retailer trades in, or the empty
// string for a retailer bound to no single one.
//
// The port's Country is a single alpha-2 code and Linkwise's countries is a
// comma-separated list - "GR", "GR,CY", or in seven recorded cases every
// country on earth. A retailer bound to more than one is exactly what the
// port spells as the zero value, so a list of two or more becomes empty
// rather than having its first entry chosen: picking one would publish a
// Greek-and-Cypriot retailer as Greek, which is a claim the network never
// made.
func soleCountry(countries string) string {
	only := strings.TrimSpace(countries)
	if only == "" || strings.Contains(only, ",") {
		return ""
	}
	return strings.ToUpper(only)
}

// routeStates maps the three things Linkwise says about whether a route can
// be promoted onto the domain's closed set (contract rule 2).
//
// A map rather than a switch, for the reason the transaction table gives for
// its own: rule 2 forbids a default branch, and a map lookup has no way to
// grow one by accident. The key is a TRIPLE because no single field decides
// it - see the three entries below, each of which is a different reason a
// route may not be used.
var routeStates = map[routeState]networks.MerchantStatus{
	// The ordinary case: the advertiser is running, we are accepted, and
	// cashback is a promotion method they permit.
	{programme: "active", affiliate: "accepted", cashback: true}: networks.MerchantStatusActive,
	// The advertiser paused their own programme. PAUSED and not departed,
	// which is the entire reason this adapter asks for status=all rather
	// than taking the endpoint's active-only default.
	{programme: "inactive", affiliate: "accepted", cashback: true}: networks.MerchantStatusPaused,
	// Cashback is not a promotion method this advertiser allows. Fifteen of
	// the three hundred and thirty-four joined programmes say so.
	//
	// Mapped to paused rather than active, and that IS a judgement this
	// adapter makes - the only one in it. The alternative is publishing
	// offers on a programme whose own terms forbid what this product is, so
	// members click through routes the advertiser may refuse to pay on and
	// Apivo breaches the terms it agreed to. Paused is the port's word for
	// "not a route we may send anybody down now", it is what an operator can
	// reverse from the row if the terms change, and StatusRaw below records
	// exactly which of the three fields caused it.
	{programme: "active", affiliate: "accepted", cashback: false}:   networks.MerchantStatusPaused,
	{programme: "inactive", affiliate: "accepted", cashback: false}: networks.MerchantStatusPaused,
}

// routeState is the triple the mapping is keyed on.
type routeState struct {
	programme string
	affiliate string
	cashback  bool
}

// mapRouteState translates one programme's state, refusing a combination
// nobody mapped with an error wrapping [networks.ErrUnmappableStatus].
//
// What is deliberately NOT mapped is every affiliate_status other than
// accepted. All three hundred and thirty-four joined programmes came back
// Accepted, which is what joined=yes means, so a Pending or a Rejected
// arriving here would say the query no longer means what it meant - and the
// safe reading of "we may not be in this programme any more" is not one to
// guess at while members are being sent through it.
//
// Note where a refusal lands. A catalogue import may only reconcile a
// retailer it did not see to left_network after iteration ended with NO error
// at all, so a refusal here stops the import rather than emptying it: the
// previous catalogue stands and an operator is told a word changed.
func mapRouteState(externalID string, row programme) (networks.MerchantStatus, error) {
	state := routeState{
		programme: strings.ToLower(strings.TrimSpace(row.ProgramStatus)),
		affiliate: strings.ToLower(strings.TrimSpace(row.AffiliateStatus)),
		cashback:  cashbackAllowed(row),
	}
	status, mapped := routeStates[state]
	if !mapped {
		return "", fmt.Errorf("%w: programme %s is %s, this account is %s and cashback sites are %s, and this adapter has no mapping for that combination",
			networks.ErrUnmappableStatus, strconv.Quote(externalID),
			strconv.Quote(row.ProgramStatus), strconv.Quote(row.AffiliateStatus),
			allowedWord(state.cashback))
	}
	return status, nil
}

// cashbackAllowed reports whether the advertiser permits cashback sites.
//
// A MISSING flag is read as not allowed, which is the only safe direction: a
// programme that says nothing about cashback has not said yes, and the cost
// of being wrong one way is a retailer nobody could have promoted anyway
// while the cost of being wrong the other is promoting a route the advertiser
// forbids. The pointer is what makes the two distinguishable at all - a plain
// bool would read a missing field and an explicit false identically.
func cashbackAllowed(row programme) bool {
	allow := row.Terms.PromotionMethods.CashbackSites.Allow
	return allow != nil && *allow
}

// describeRouteState is the verbatim status kept beside the normalised one
// (FR-032), and it has to name all three fields because all three decide the
// mapping.
//
// It is a composite rather than one of Linkwise's words, and that is the
// point: an evidence row carrying only "Active" could not explain why a route
// this network called active was stored as paused. Every value in it is
// Linkwise's own, under Linkwise's own field name; nothing here invents a
// vocabulary.
func describeRouteState(row programme) string {
	return "program_status=" + strings.TrimSpace(row.ProgramStatus) +
		";affiliate_status=" + strings.TrimSpace(row.AffiliateStatus) +
		";cashback_sites=" + allowedWord(cashbackAllowed(row))
}

// allowedWord renders the cashback flag for both the evidence and the
// refusal, so the two never disagree about what was read.
func allowedWord(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "not-allowed"
}
