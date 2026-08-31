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
	"slices"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Prefix is the path this module serves. The composition root mounts the
// handler at it AND at it plus a trailing slash, so a stray sub-path is
// answered here in problem+json rather than handed to whatever else claims
// the namespace.
const Prefix = "/api/v1/cashback/wallet"

// Handler serves the member wallet endpoint. Build it with NewHandler.
type Handler struct {
	log     *slog.Logger
	wallets *Wallets
	auth    MemberAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the wallet route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, wallets *Wallets, auth MemberAuthenticator) http.Handler {
	h := &Handler{log: log, wallets: wallets, auth: auth}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	mux.HandleFunc(Prefix+"/", h.handleUnrouted)
	mux.HandleFunc(Prefix, h.handleUnrouted)
	return h.requireMember(mux)
}

// routes maps every wallet route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET " + Prefix: h.getWallet,
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
