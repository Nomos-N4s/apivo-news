// POST /withdrawals: a member asking to be paid (T091).
//
// The member comes from the token and from nowhere else. There is no path
// segment and no query parameter naming one, so asking to be paid out of
// somebody else's balance is not a request this endpoint can express - the
// same rule the wallet surface is built on, and the same reason.
//
// Two of the refusals here carry machine-readable facts rather than prose. A
// client shows "you are 4.00 short" and offers the amount that would work, so
// the shortfall is an extension member of the problem document (RFC 9457
// §3.2) instead of a sentence it would have to parse.

package payout

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Prefix is the path this module serves. The composition root mounts the
// handler at it AND at it plus a trailing slash, so a stray sub-path is
// answered here in problem+json rather than handed to whatever else claims
// the namespace.
const Prefix = "/api/v1/cashback/withdrawals"

// maxBodyBytes caps a withdrawal request body. It carries an id and an
// amount; anything larger is not a request this endpoint has a reading of.
const maxBodyBytes = 4 << 10

// The codes a client branches on, carried as the problem document's "code"
// extension member. They are the contract's own spellings
// (contracts/http-api.md) and are exported because a client library in this
// repository should name them rather than repeat the string.
const (
	// CodeInsufficientConfirmedBalance is 409: the member cannot withdraw
	// this amount yet. It covers both reasons - below the deployment's
	// threshold, and beyond what they hold - because both are answered the
	// same way by a client, with a shortfall to make up. Which one it is,
	// the detail says.
	CodeInsufficientConfirmedBalance = "insufficient_confirmed_balance"
	// CodeDestinationNotVerified is 409: the destination is theirs and has
	// not been proved so (FR-051).
	CodeDestinationNotVerified = "destination_not_verified"
)

// Handler serves the member withdrawal endpoints. Build it with NewHandler.
type Handler struct {
	log         *slog.Logger
	withdrawals *Withdrawals
	auth        MemberAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the withdrawal route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, withdrawals *Withdrawals, auth MemberAuthenticator) (http.Handler, error) {
	switch {
	case log == nil:
		return nil, errors.New("payout: the withdrawal handler needs a logger")
	case withdrawals == nil:
		return nil, ErrNoWithdrawalStore
	case auth == nil:
		return nil, errors.New("payout: the withdrawal handler needs somewhere to authenticate members")
	}
	h := &Handler{log: log, withdrawals: withdrawals, auth: auth}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	mux.HandleFunc(Prefix+"/", h.handleUnrouted)
	mux.HandleFunc(Prefix, h.handleUnrouted)
	return h.requireMember(mux), nil
}

// routes maps every withdrawal route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against somebody's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST " + Prefix: h.postWithdrawal,
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

// amountJSON is one figure as the contract spells it: minor units and an
// explicit currency, the shape C-6 mandates everywhere (contracts/http-api.md).
type amountJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

func figure(a money.Amount) amountJSON {
	return amountJSON{Minor: a.Minor, Currency: string(a.Currency)}
}

// withdrawalRequestBody is what a member sends.
type withdrawalRequestBody struct {
	DestinationID string     `json:"destination_id"`
	Amount        amountJSON `json:"amount"`
}

// withdrawalResponse is the 201 body.
type withdrawalResponse struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	// ReservedAmount is what was actually taken out of confirmed, which is
	// at least what was asked for. Entries are reserved whole - each cites
	// the network report evidencing it (C-2), so there is no half of one -
	// and a member who asked for slightly less than an entry gets slightly
	// more reserved. Reporting the request's own amount here instead would
	// tell them a figure that is not what will be paid.
	ReservedAmount amountJSON `json:"reserved_amount"`
}

// postWithdrawal implements POST /api/v1/cashback/withdrawals.
func (h *Handler) postWithdrawal(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	var body withdrawalRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	destination, err := uuid.Parse(body.DestinationID)
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"destination_id must be the id of one of your payout destinations")
		return
	}
	amount, err := money.New(body.Amount.Minor, money.Currency(body.Amount.Currency))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"amount must be {minor, currency} with an ISO 4217 currency: "+err.Error())
		return
	}

	made, err := h.withdrawals.Request(r.Context(), Request{
		Member: member.ID, Destination: destination, Amount: amount,
	})
	if err != nil {
		h.refuse(w, r, err)
		return
	}

	h.writeJSON(w, r, http.StatusCreated, withdrawalResponse{
		RequestID:      made.ID.String(),
		State:          made.State.String(),
		ReservedAmount: figure(made.Amount),
	})
}

// refuse maps one failure to the answer the contract gives it.
//
// The order matters where errors nest: the typed shortfalls are matched
// before the sentinels they are built beside, so a refusal that carries
// arithmetic is never flattened into one that does not.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, err error) {
	var below BelowThreshold
	var short earnings.ShortConfirmedBalance
	switch {
	case errors.As(err, &below):
		h.shortfall(w, below.Short,
			"your confirmed balance has not reached the amount withdrawals start at",
			map[string]any{"threshold": figure(below.Threshold), "confirmed": figure(below.Confirmed)})
	case errors.As(err, &short):
		h.shortfall(w, short.Short,
			"you asked for more than your confirmed balance",
			map[string]any{"confirmed": figure(short.Have), "requested": figure(short.Want)})
	case errors.Is(err, ErrDestinationNotVerified):
		// 409 rather than 403: the destination IS theirs, and telling them
		// it is forbidden would send them looking for the wrong problem.
		platformhttp.ProblemWith(w, http.StatusConflict,
			"that destination has not been verified yet, so it cannot receive money",
			map[string]any{"code": CodeDestinationNotVerified})
	case errors.Is(err, ErrDestinationNotFound):
		// 403 for a destination that is not the caller's, and the same 403
		// for one that does not exist. A 404 for the second would confirm
		// which ids are real, one request at a time.
		platformhttp.Problem(w, http.StatusForbidden,
			"that destination is not yours to be paid at")
	case errors.Is(err, ErrCurrencyNotPaid):
		// The currencies are codes rather than amounts, so naming them here
		// is safe - and it is the one thing that tells a member which to
		// ask in.
		platformhttp.Problem(w, http.StatusBadRequest,
			"this deployment does not pay out in that currency")
	case errors.Is(err, earnings.ErrNothingToReserve):
		platformhttp.Problem(w, http.StatusBadRequest,
			"amount must be a positive number of minor units")
	case errors.Is(err, ErrNoThreshold):
		// 503, not 500: nothing is broken, the deployment is incomplete,
		// and the two are different things to whoever is paged. Named keys,
		// because the person reading this is the person who can set them.
		h.unconfigured(w, r, "this deployment has no withdrawal threshold",
			"PAYOUT_THRESHOLD_MINOR, PAYOUT_THRESHOLD_CURRENCY")
	case errors.Is(err, ErrNoReceivable):
		h.unconfigured(w, r, "this deployment has not named the account earnings are paid out of",
			"HOUSE_ACCOUNT_NETWORK_RECEIVABLE")
	default:
		h.log.ErrorContext(r.Context(), "recording a withdrawal request", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
	}
}

// unconfigured answers 503 and names the environment keys that would fix it.
//
// 503 rather than 500 throughout: nothing is broken, the deployment is
// incomplete, and the two are different things to whoever is paged. The keys
// go to the log rather than the wire - the person who can set them reads the
// log, and a member reads only that withdrawals are not available yet.
func (h *Handler) unconfigured(w http.ResponseWriter, r *http.Request, what, keys string) {
	h.log.ErrorContext(r.Context(), "a withdrawal was requested and "+what, "keys", keys)
	platformhttp.Problem(w, http.StatusServiceUnavailable,
		"this deployment cannot accept withdrawals yet")
}

// shortfall answers the one 409 a client acts on, carrying the figure that
// would make the request work (US4 scenario 1).
//
// Every figure travels as {minor, currency} and NONE of them is spelled into
// the detail. money.Amount.String renders minor units - "2500 EUR", not
// "25.00 EUR" - deliberately, so that it can never be pasted onto a
// member-facing surface and mistaken for a price. A problem document IS one,
// so the detail says what went wrong and the client formats what it needs
// to, in the member's own locale.
func (h *Handler) shortfall(w http.ResponseWriter, missing money.Amount, detail string, figures map[string]any) {
	extensions := map[string]any{
		"code":      CodeInsufficientConfirmedBalance,
		"shortfall": figure(missing),
	}
	maps.Copy(extensions, figures)
	platformhttp.ProblemWith(w, http.StatusConflict, detail, extensions)
}

// writeJSON writes the response body, logging rather than failing when the
// connection has already gone: the status line is out by then, so there is
// nothing left to tell the client.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing a withdrawal response", "error", err)
	}
}

// decodeJSON decodes the request body into dst, answering the 400 itself
// when the body is not the expected shape. Unknown fields are rejected: on a
// surface that moves money, a misspelled field silently ignored would be a
// request nobody made being accepted as one somebody did.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	// Everything after the first document must be whitespace, and only a
	// second Decode proves it. Decoder.More() answers a narrower question -
	// "is another VALUE coming?" - so a stray closing delimiter after a
	// valid object would read as "no more values" and be accepted.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		platformhttp.Problem(w, http.StatusBadRequest,
			"request body must contain a single JSON document")
		return false
	}
	return true
}
