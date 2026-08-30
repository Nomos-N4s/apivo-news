// FetchCatalogue: the recorded catalogue read whole, and why completeness is
// the entire contract here. One file, because a catalogue is a current state
// rather than a period and so shares none of the window reasoning above it.

package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// FetchCatalogue yields every retailer the recorded catalogue lists, each
// carrying its verbatim payload (FR-012) and each already through
// [networks.ReportedMerchant.Validate] (contract rule 7).
//
// It takes no window and has no immediate error: a catalogue is a current
// state rather than a period, so there is nothing checkable before contacting
// the network, and every failure - including one on the first page - is
// yielded (contract rule 9). The clock is not touched either. The transaction
// lifecycle is a story about one purchase; the catalogue is what the
// publisher account could promote, and re-reading it does not move a purchase
// along.
//
// Completeness is the whole contract here, which is why contract rule 8
// matters more to this method than to any other. An import reads absence as
// departure: a retailer that was not in the answer is reconciled to
// left_network, its offers stop being published, and members see an emptied
// catalogue. So an import may only do that after an iteration that ended with
// no error at all, and this adapter yields one whenever it stops early -
// cancelled, or told to report a network failure - rather than returning
// quietly and letting a truncated answer be read as a mass departure.
//
// The recording holds one retailer in each of the three route states,
// including one bound to no country at all, because those are the three
// branches an import has and the one column it must be able to leave null.
func (n *Network) FetchCatalogue(ctx context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	return func(yield func(networks.ReportedMerchant, error) bool) {
		pages := n.recorded.merchantPages(n.unmappable)
		failAt := failurePage(len(pages))

		for index, page := range pages {
			if err := ctx.Err(); err != nil {
				yield(networks.ReportedMerchant{}, n.abandonedCatalogue(err))
				return
			}
			if index == failAt {
				if err := n.failures.take(); err != nil {
					yield(networks.ReportedMerchant{}, fmt.Errorf("fixture: %s: catalogue: %w", n.account, err))
					return
				}
			}
			if !n.yieldMerchantPage(ctx, page, yield) {
				return
			}
		}
	}, nil
}

// yieldMerchantPage walks one recorded catalogue body, reporting whether
// iteration should continue - false both for a caller that broke and for an
// error this adapter has already yielded, which are the same instruction from
// the loop above.
func (n *Network) yieldMerchantPage(ctx context.Context, page merchantPage, yield func(networks.ReportedMerchant, error) bool) bool {
	for _, raw := range page.Merchants {
		if err := ctx.Err(); err != nil {
			yield(networks.ReportedMerchant{}, n.abandonedCatalogue(err))
			return false
		}
		var recorded recordedMerchant
		if err := json.Unmarshal(raw, &recorded); err != nil {
			yield(networks.ReportedMerchant{}, fmt.Errorf("%w: page %d of the catalogue: %w", ErrRecordingUnreadable, page.Page, err))
			return false
		}
		entry, err := merchantFrom(recorded, raw)
		if err != nil {
			yield(networks.ReportedMerchant{}, err)
			return false
		}
		if !yield(entry, nil) {
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
func (n *Network) abandonedCatalogue(cause error) error {
	return networks.AbandonedIteration(fmt.Errorf("fixture: %s: catalogue: %w", n.account, cause))
}

// merchantFrom turns one recorded catalogue fragment into the port's value:
// the route status mapped (contract rule 2), a null country carried across as
// the empty string the port spells an unbound retailer with, the verbatim
// bytes carried through (FR-012), and the whole thing through its own
// Validate before it can be yielded (contract rule 7).
//
// The payload is cloned for the reason reportFrom clones one: the recording
// is shared, and evidence a later caller can edit is not evidence.
func merchantFrom(recorded recordedMerchant, raw json.RawMessage) (networks.ReportedMerchant, error) {
	status, err := mapMerchantStatus(recorded.ExternalID, recorded.Status)
	if err != nil {
		return networks.ReportedMerchant{}, err
	}
	country := ""
	if recorded.Country != nil {
		country = *recorded.Country
	}
	entry := networks.ReportedMerchant{
		ExternalID: recorded.ExternalID,
		Name:       recorded.Name,
		Country:    country,
		StatusRaw:  recorded.Status,
		Status:     status,
		RawPayload: clonePayload(raw),
	}
	if err := entry.Validate(); err != nil {
		return networks.ReportedMerchant{}, fmt.Errorf("fixture: merchant %s: %w", strconv.Quote(recorded.ExternalID), err)
	}
	return entry, nil
}
