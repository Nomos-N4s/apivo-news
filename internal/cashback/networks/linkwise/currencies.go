// Which currency a transaction is denominated in, read off the programme it
// belongs to (T247, C-6).
//
// THE TRANSACTION REPORT CARRIES NO CURRENCY FIELD. That is not an omission
// in this adapter: the endpoint's own usage text lists every field the report
// can return - program_id, program, program_cat, rotator, creative,
// subid1..subid5, transaction_id, type, status, subaction, amended, amount,
// commission, click_date, transaction_date, status_date, click_ref_url,
// payout_cat, payment_status - and there is no currency among them.
//
// The programme list carries one per programme, and across the 334 programmes
// the recorded account is joined to they are NOT all the same: 329 report in
// EUR, three in PLN and two in USD. So a single declared currency is right
// for 98.5% of them and silently wrong for the rest, storing zloty as euro -
// which a member is then paid out of.
//
// Every transaction row names its programme, so the answer is a join rather
// than a declaration. This file is that join: one read of the programme list,
// cached, keyed by programme id.

package linkwise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// DefaultCurrencyRefresh is how long a programme's currency is believed
// before the list is read again.
//
// An hour, and it is generous on purpose. A programme's currency is a fact
// about an advertiser's business, not a price: it changes when a company
// redenominates, which is a thing that happens to a country rather than to a
// quarter. The refresh exists so a NEW programme becomes readable without a
// restart, and the miss path below already handles that case immediately, so
// this is only the backstop.
//
// The cost is what makes it worth naming: the programme list is a single
// response of about four megabytes and it takes several seconds, so a short
// TTL would put that in front of one poll in every few.
const DefaultCurrencyRefresh = time.Hour

// ErrUnknownProgrammeCurrency reports a transaction naming a programme the
// programme list does not carry.
//
// It is an error and NEVER a fallback, which is the whole point of this file.
// An adapter that met an unknown programme and reached for a default would be
// the silent mis-denomination this join exists to remove, arrived at by a
// different road. A window that cannot be read is visible; a window read in
// the wrong currency is not.
var ErrUnknownProgrammeCurrency = fmt.Errorf("linkwise: the programme list does not say which currency a transaction is in")

// currencyIndex is the programme-to-currency map, and the lock that keeps one
// poller from reading a four-megabyte list twice at once.
//
// The mutex is held across the fetch deliberately. Two goroutines that both
// found the index stale would otherwise both read the list - eight seconds of
// a partner's API to answer a question one request already answered - and the
// only cost of serialising is that the second waits for the first, which is
// what it wanted anyway.
type currencyIndex struct {
	mu      sync.Mutex
	byID    map[string]money.Currency
	fetched time.Time
}

// currencyFor answers which currency a programme's transactions are in,
// reading the programme list if it has not been read recently.
//
// A miss triggers ONE immediate re-read before it is reported, because the
// obvious cause of a miss is a programme joined since the last read - and a
// window that failed for an hour because a new retailer appeared would be a
// self-inflicted outage. A miss that survives the re-read is real.
func (c *Client) currencyFor(ctx context.Context, programmeID string) (money.Currency, error) {
	index := c.currencies
	index.mu.Lock()
	defer index.mu.Unlock()

	justRead := false
	if index.byID == nil || c.clock.Now().Sub(index.fetched) >= c.currencyRefresh {
		if err := c.refreshCurrencies(ctx, index); err != nil {
			return "", err
		}
		justRead = true
	}
	if currency, known := index.byID[programmeID]; known {
		return currency, nil
	}

	// The one re-read, skipped when the list was read a moment ago: reading
	// it twice in a row answers the same question twice, at four megabytes
	// and several seconds a time.
	if !justRead {
		if err := c.refreshCurrencies(ctx, index); err != nil {
			return "", err
		}
		if currency, known := index.byID[programmeID]; known {
			return currency, nil
		}
	}
	return "", fmt.Errorf("%w: programme %s is not among the %d the network lists",
		ErrUnknownProgrammeCurrency, strconv.Quote(programmeID), len(index.byID))
}

// refreshCurrencies reads the programme list and rebuilds the index. The
// caller holds the lock.
//
// It asks joined=all, not joined=yes, and that is the difference between this
// read and the catalogue's. The catalogue wants routes this account may
// promote; this wants the currency of any programme a transaction can NAME -
// including one the account has since left, which a trailing sweep re-reading
// a ninety-day-old window will meet. status=all is here for the same reason:
// a paused programme's old transactions still have to be denominated.
func (c *Client) refreshCurrencies(ctx context.Context, index *currencyIndex) error {
	body, err := c.Get(ctx, catalogueEndpoint, url.Values{
		"joined": {"all"},
		"status": {"all"},
	})
	if err != nil {
		return fmt.Errorf("linkwise: reading the programme list for its currencies: %w", err)
	}
	rows, err := decodeRows(body)
	if err != nil {
		return fmt.Errorf("linkwise: reading the programme list for its currencies: %w", err)
	}

	byID := make(map[string]money.Currency, len(rows))
	for i, raw := range rows {
		var row struct {
			ID       json.Number `json:"id"`
			Currency struct {
				Code string `json:"code"`
			} `json:"currency"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("%w: programme %d of the list will not parse: %w",
				networks.ErrNetworkUnavailable, i+1, err)
		}
		id := strings.TrimSpace(row.ID.String())
		if id == "" {
			continue
		}
		currency, err := money.ParseCurrency(strings.ToUpper(strings.TrimSpace(row.Currency.Code)))
		if err != nil {
			// Skipped rather than fatal. A programme with no readable
			// currency makes ITS OWN transactions unreadable - they meet
			// ErrUnknownProgrammeCurrency, naming the programme - and there
			// is no reason for it to stop every other programme's window
			// from being polled.
			continue
		}
		byID[id] = currency
	}

	index.byID = byID
	index.fetched = c.clock.Now()
	return nil
}
