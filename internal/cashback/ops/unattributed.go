// The unattributed queue as an operator sees it (T059, FR-034, FR-060).
//
// The queue is recorded by the poller and read here. What "still open" means
// is not re-decided in this package: an observation stops being work when a
// later report supersedes the one it names, when something has been credited
// against it, or when somebody has resolved it, and all three are answered
// where the evidence lives. This module asks the question and renders the
// answer; a second definition of "open" is the place the two would
// eventually disagree, over money.

package ops

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Pagination bounds from the contract's conventions: limit defaults to 20
// and never exceeds 100, so no caller can ask for the whole queue at once.
const (
	defaultQueueLimit = 20
	maxQueueLimit     = 100
)

// UnattributedStore is what this module needs of the unattributed queue,
// named here per the boundary rules - the consumer names its dependency -
// and satisfied by the networks module's queue over the platform pool.
//
// It speaks the networks module's own vocabulary rather than a copy of it.
// The alternative is a parallel set of types whose only job is to be
// converted, and the conversion is where a status or an amount would
// eventually be dropped without a compiler noticing.
type UnattributedStore interface {
	// Open returns one page of open work, oldest first, starting after the
	// given position.
	Open(ctx context.Context, after networks.After, limit int) ([]networks.OpenReport, error)
	// Dismiss closes one row, recording who closed it and why, and
	// publishing that fact - all in one transaction, because a resolution
	// whose event was lost is a decision no other part of the system will
	// ever hear about.
	//
	// It reports ErrNoSuchQueueRow for an id that names no row, and a
	// ClosedError (which wraps networks.ErrNoLongerOpen) for one that is no
	// longer the operator's to close.
	Dismiss(ctx context.Context, d Dismissal) (Dismissed, error)
}

// unattributedItem is one line of the queue as a client reads it.
//
// Every field is a fact the network reported or the database stamped, and
// none is a suggestion: there is deliberately no "recommended action" here.
// What an operator may do with a row is derivable from attributable, and
// the endpoint that acts checks it again against the row rather than
// trusting what a page rendered minutes ago said.
type unattributedItem struct {
	ID         string `json:"id"`
	DetectedAt string `json:"detected_at"`
	// NetworkTransactionID is the evidence row. An operator quoting this
	// into a support conversation is quoting the thing an entry would have
	// to cite, which is what makes the two conversations the same one.
	NetworkTransactionID string `json:"network_transaction_id"`
	NetworkAccountID     string `json:"network_account_id"`
	// NetworkID and ExternalID are how an operator finds the transaction in
	// the network's own dashboard.
	NetworkID  string `json:"network_id"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	// Sale is what the member spent and Commission what the network says it
	// will pay. money.Amount marshals to the contract's {minor, currency}
	// and refuses to marshal at all without a valid currency, so a decimal
	// can never appear here and neither can a bare number (C-6).
	Sale         money.Amount `json:"sale"`
	Commission   money.Amount `json:"commission"`
	TransactedAt string       `json:"transacted_at"`
	RetrievedAt  string       `json:"retrieved_at"`
	// Attributable reports whether an entry could lawfully be created for
	// this report at all: false where the network named a click reference
	// that matched no click, because the evidence guard refuses a credit
	// that cites no click there and there is no click to cite. Dismissing
	// is then the only outcome, and saying so here is what stops an
	// operator interface offering an action the database will refuse.
	Attributable bool `json:"attributable"`
}

// unattributedPage is one page of the queue: the contract's list shape.
type unattributedPage struct {
	Items []unattributedItem `json:"items"`
	// NextCursor is null on the last page. It is present only when a
	// further page actually exists - a cursor that leads to an empty page
	// reads to a paging client as "there is more", and on a work queue that
	// is an operator told there is work when there is none.
	NextCursor *string `json:"next_cursor"`
}

// listUnattributed implements GET /api/v1/cashback/ops/unattributed.
func (h *Handler) listUnattributed(w http.ResponseWriter, r *http.Request) {
	after, limit, detail, ok := parseQueuePage(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	// One more than the page, so "is there another page?" is answered by
	// what came back rather than by a second query - and never by guessing
	// from a full page, which reports more work after the last row exactly
	// when the queue divides evenly.
	rows, err := h.store.Open(r.Context(), after, limit+1)
	if err != nil {
		h.internalError(w, r, "listing unattributed work", err)
		return
	}

	page := unattributedPage{Items: make([]unattributedItem, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, unattributedItemOf(row))
	}
	if len(rows) > limit {
		last := rows[limit-1]
		next := encodeCursor(unattributedCursors, last.DetectedAt, last.ID)
		page.NextCursor = &next
	}
	h.writeJSON(w, r, page)
}

// unattributedItemOf renders one open report for the wire.
func unattributedItemOf(row networks.OpenReport) unattributedItem {
	return unattributedItem{
		ID:                   row.ID.String(),
		DetectedAt:           stamp(row.DetectedAt),
		NetworkTransactionID: row.Report.String(),
		NetworkAccountID:     row.Account.String(),
		NetworkID:            string(row.Network),
		ExternalID:           row.ExternalID,
		Status:               string(row.Status),
		Sale:                 row.Sale,
		Commission:           row.Commission,
		TransactedAt:         stamp(row.TransactedAt),
		RetrievedAt:          stamp(row.RetrievedAt),
		Attributable:         row.Attributable,
	}
}

// parseQueuePage reads the paging parameters, answering the 400 detail
// itself for anything it refuses.
//
// An unrecognised query parameter is rejected rather than ignored: a
// misspelled filter silently dropped would answer a wider question than the
// caller asked, and on this queue a wider question is more money on screen
// than the operator meant to look at. A repeated known parameter is rejected
// for the same reason - url.Values keeps every value but Get returns only
// the first, so `?limit=10&limit=20` would silently answer one of two
// contradictory requests.
func parseQueuePage(values url.Values) (after networks.After, limit int, detail string, ok bool) {
	at, rowID, limit, detail, ok := parsePage(values, unattributedCursors)
	return networks.After{DetectedAt: at, ID: rowID}, limit, detail, ok
}

// parsePage reads the paging parameters every operator queue shares, for
// the list whose cursors it accepts. The position is the zero value when no
// cursor was supplied.
func parsePage(values url.Values, list string) (at time.Time, rowID uuid.UUID, limit int, detail string, ok bool) {
	for name, supplied := range values {
		switch name {
		case "limit", "cursor":
			if len(supplied) > 1 {
				return time.Time{}, uuid.Nil, 0, "query parameter " + strconv.Quote(name) + " was supplied " + strconv.Itoa(len(supplied)) + " times; supply it at most once", false
			}
		default:
			return time.Time{}, uuid.Nil, 0, "unknown query parameter " + strconv.Quote(name) + "; this endpoint accepts limit and cursor", false
		}
	}

	limit = defaultQueueLimit
	// Presence, not a non-empty value: `?limit=` is a supplied limit that
	// happens to be unparseable, and answering it with the default page
	// would read as acceptance of whatever the caller meant to send.
	if values.Has("limit") {
		parsed, err := strconv.ParseInt(values.Get("limit"), 10, 32)
		if err != nil || parsed < 1 || parsed > maxQueueLimit {
			return time.Time{}, uuid.Nil, 0, "limit must be a whole number between 1 and " + strconv.Itoa(maxQueueLimit), false
		}
		limit = int(parsed)
	}

	if values.Has("cursor") {
		at, rowID, err := decodeCursor(list, values.Get("cursor"))
		if err != nil {
			return time.Time{}, uuid.Nil, 0, "cursor is not one this endpoint issued", false
		}
		return at, rowID, limit, "", true
	}
	return time.Time{}, uuid.Nil, limit, "", true
}
