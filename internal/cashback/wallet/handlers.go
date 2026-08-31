// GET /wallet: the three totals a member is shown, what they have been paid,
// and what they must reach to ask to be paid again (T078, FR-041, FR-050).
//
// Every figure is {minor, currency}, the shape C-6 mandates everywhere: a
// bare integer would be a number the client decides a currency for, and the
// one it decided would be right until a second currency was published.

package wallet

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Prefix is the path this module serves. The composition root mounts the
// handler at it AND at it plus a trailing slash, so a stray sub-path is
// answered here in problem+json rather than handed to whatever else claims
// the namespace.
const Prefix = "/api/v1/cashback/wallet"

// timeFormat renders every timestamp in wallet responses: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone.
const timeFormat = time.RFC3339Nano

// entriesPath is the member's own history, under the wallet's prefix so one
// auth gate and one error convention cover both.
const entriesPath = Prefix + "/entries"

// ParticipationPrefix is the member's opt-in, and it is a SIBLING of the
// wallet rather than a path beneath it (contracts/http-api.md). That is not
// an accident of naming: a member who has left cashback still has a wallet
// full of entries that continue to resolve (FR-003), so participation is
// not part of the wallet, it is the thing that decides whether new money
// ever enters one.
//
// It is served by this module's handler all the same, which the composition
// root mounts at both prefixes. One module, one auth gate, one error
// convention - and this file is where every member-facing cashback route
// can be read in a single list.
const ParticipationPrefix = "/api/v1/cashback/participation"

// Handler serves the member wallet endpoints. Build it with NewHandler.
type Handler struct {
	log            *slog.Logger
	wallets        *Wallets
	history        *History
	participations *Participations
	auth           MemberAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the wallet route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, wallets *Wallets, history *History, participations *Participations, auth MemberAuthenticator) http.Handler {
	h := &Handler{log: log, wallets: wallets, history: history, participations: participations, auth: auth}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Both trees get the catch-all, so a stray sub-path under either is
	// answered here in problem+json rather than handed on.
	for _, prefix := range []string{Prefix, ParticipationPrefix} {
		mux.HandleFunc(prefix+"/", h.handleUnrouted)
		mux.HandleFunc(prefix, h.handleUnrouted)
	}
	return h.requireMember(mux)
}

// routes maps every wallet route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET " + Prefix:                 h.getWallet,
		"GET " + entriesPath:            h.getEntries,
		"GET " + ParticipationPrefix:    h.getParticipation,
		"POST " + ParticipationPrefix:   h.postParticipation,
		"DELETE " + ParticipationPrefix: h.deleteParticipation,
	}
}

// Patterns lists this module's ServeMux patterns ("METHOD /path"), sorted.
// The catch-all is deliberately absent: it is the error convention for paths
// nobody serves, not an endpoint a client can call.
func Patterns() []string {
	var h Handler
	return slices.Sorted(maps.Keys(h.routes()))
}

// handleUnrouted answers anything under the path that no route claimed -
// still behind the auth gate, so an unauthenticated probe answers 401 before
// this runs.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	if allow := h.allow.MethodsFor(r.URL.Path); allow != "" {
		w.Header().Set("Allow", allow)
		platformhttp.Problem(w, http.StatusMethodNotAllowed,
			r.Method+" is not allowed on this endpoint; use "+allow)
		return
	}
	platformhttp.Problem(w, http.StatusNotFound, "no such endpoint")
}

// amountJSON is one figure as the contract spells it. The same shape as the
// ledger's own and as the entry rows', so a member, an operator and an
// auditor read one representation of money across the whole API (C-6).
type amountJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

func figure(a money.Amount) amountJSON {
	return amountJSON{Minor: a.Minor, Currency: string(a.Currency)}
}

// walletResponse is the body of GET /wallet.
//
// Held is deliberately absent. A hold is the business deciding not to count
// money yet (US7), and a fifth total would invite the member to count it -
// so the projection carries it for operators and this does not carry it to
// members. What a member sees held instead is the ENTRY, in
// GET /wallet/entries, with the rule that is holding it.
type walletResponse struct {
	Pending   amountJSON `json:"pending"`
	Confirmed amountJSON `json:"confirmed"`
	Reserved  amountJSON `json:"reserved"`
	PaidOut   amountJSON `json:"paid_out"`
	// PayoutThreshold is what Confirmed must reach before a withdrawal may
	// be requested (FR-050). Returned beside the totals rather than left to
	// the client, because a client that hard-coded it would show one figure
	// while the server enforced another.
	PayoutThreshold amountJSON `json:"payout_threshold"`
}

// getWallet implements GET /api/v1/cashback/wallet.
//
// The member comes from the token and from nowhere else: there is no path
// segment and no query parameter naming one, so reading somebody else's
// wallet is not a request this endpoint can express.
func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	held, err := h.wallets.Of(r.Context(), member.ID)
	switch {
	case errors.Is(err, ErrNoThreshold):
		// 503 rather than 500: nothing is broken, the deployment is
		// incomplete, and the two are different things to whoever is paged.
		// Named keys, because the person reading this is the person who can
		// set them.
		h.log.ErrorContext(r.Context(), "a wallet was requested and this deployment has no withdrawal threshold",
			"keys", "PAYOUT_THRESHOLD_MINOR, PAYOUT_THRESHOLD_CURRENCY")
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment has not configured a withdrawal threshold, so a wallet cannot be answered")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "reading a wallet", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	h.writeJSON(w, r, http.StatusOK, walletResponse{
		Pending:         figure(held.Pending),
		Confirmed:       figure(held.Confirmed),
		Reserved:        figure(held.Reserved),
		PaidOut:         figure(held.PaidOut),
		PayoutThreshold: figure(held.Threshold),
	})
}

// writeJSON writes the response body, logging rather than failing when the
// connection has already gone: the status line is out by then, so there is
// nothing left to tell the client.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing a wallet response", "error", err)
	}
}

// entryItem is one line of history as the contract spells it.
type entryItem struct {
	EntryID string `json:"entry_id"`
	// MerchantName is the retailer, or null for an entry an operator
	// attributed by hand: there was no click, so there is no route to a
	// retailer to follow (FR-034).
	MerchantName *string `json:"merchant_name"`
	// MerchantNameLanguage is what MerchantName is written in, and
	// MerchantNameIsFallback says it is not the language that was asked
	// for. US5 scenario 2 requires the fallback to be LABELLED rather than
	// passed off as the member's own language, and a client cannot label
	// what it was not told.
	MerchantNameLanguage   *string `json:"merchant_name_language"`
	MerchantNameIsFallback bool    `json:"merchant_name_is_fallback"`
	// TransactedAt is when the purchase happened, as the network reported
	// it - the instant a member recognises.
	TransactedAt   string     `json:"transacted_at"`
	SaleAmount     amountJSON `json:"sale_amount"`
	CashbackAmount amountJSON `json:"cashback_amount"`
	State          string     `json:"state"`
	// ExpectedConfirmationAt is when this entry is expected to confirm, and
	// is ALWAYS null today. Nothing in the schema records a confirmation
	// window - no column on the network or the route holds one - so there
	// is no honest value to compute, and a plausible one invented here is a
	// date a member would plan around. See the task notes: the natural home
	// is a nullable window on cashback.merchant_network, filled by the
	// catalogue import, and that is a migration rather than this endpoint's
	// to guess.
	ExpectedConfirmationAt *string `json:"expected_confirmation_at"`
	// HoldRule names the rule holding this entry back, and is null unless
	// the state is held. The totals do not show held money at all; a member
	// looking at an entry they cannot count is owed the reason.
	HoldRule *string `json:"hold_rule"`
	// ReversalOfID names the entry this one undoes, and Reason why. Both
	// null on an entry that undoes nothing.
	ReversalOfID *string `json:"reversal_of_id"`
	Reason       *string `json:"reason"`
}

// entriesPage is the body of GET /wallet/entries.
type entriesPage struct {
	Items []entryItem `json:"items"`
	// NextCursor is null on the last page, so a client stops by reading the
	// answer rather than by making one more request to discover it.
	NextCursor *string `json:"next_cursor"`
}

// getEntries implements GET /api/v1/cashback/wallet/entries.
func (h *Handler) getEntries(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	req, detail, ok := parseEntriesPage(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	req.Member = member.ID

	page, err := h.history.Page(r.Context(), req)
	switch {
	case errors.Is(err, ErrBadCursor):
		platformhttp.Problem(w, http.StatusBadRequest,
			"cursor is not one this endpoint issued; send back what next_cursor gave you, or omit it for the first page")
		return
	case errors.Is(err, ErrUnknownState):
		platformhttp.Problem(w, http.StatusBadRequest,
			"state must be one of "+strings.Join(States, ", "))
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "listing a member's entries", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	body := entriesPage{Items: make([]entryItem, 0, len(page.Entries))}
	for _, entry := range page.Entries {
		body.Items = append(body.Items, itemFrom(entry))
	}
	if page.NextCursor != "" {
		body.NextCursor = &page.NextCursor
	}
	h.writeJSON(w, r, http.StatusOK, body)
}

// itemFrom renders one entry.
func itemFrom(entry Entry) entryItem {
	item := entryItem{
		EntryID:                entry.ID.String(),
		TransactedAt:           entry.TransactedAt.UTC().Format(timeFormat),
		SaleAmount:             figure(entry.Sale),
		CashbackAmount:         figure(entry.Amount),
		State:                  entry.State,
		MerchantNameIsFallback: entry.Merchant.Name != "" && !entry.Merchant.Asked,
	}
	if entry.Merchant.Name != "" {
		name, language := entry.Merchant.Name, entry.Merchant.Language
		item.MerchantName, item.MerchantNameLanguage = &name, &language
	}
	if entry.HoldRule != "" {
		rule := entry.HoldRule
		item.HoldRule = &rule
	}
	if entry.ReversalOf != uuid.Nil {
		of := entry.ReversalOf.String()
		item.ReversalOfID = &of
	}
	if entry.Reason != "" {
		reason := entry.Reason
		item.Reason = &reason
	}
	return item
}

// parseEntriesPage reads the query parameters, refusing anything it does not
// serve rather than ignoring it: a misspelled filter silently dropped would
// answer with every entry, and a member who asked to see only their reversed
// ones would read the whole list as reversals.
func parseEntriesPage(values url.Values) (PageRequest, string, bool) {
	for name := range values {
		switch name {
		case "state", "limit", "cursor", "lang":
		default:
			return PageRequest{}, "unknown query parameter " + strconv.Quote(name) +
				"; this endpoint accepts state, limit, cursor and lang", false
		}
	}
	req := PageRequest{
		State:    values.Get("state"),
		Language: values.Get("lang"),
		Cursor:   values.Get("cursor"),
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return PageRequest{}, "limit must be a positive whole number", false
		}
		req.Limit = limit
	}
	return req, "", true
}
