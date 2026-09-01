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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Prefix is the withdrawals path this module serves. The composition root
// mounts the handler at it AND at it plus a trailing slash, so a stray
// sub-path is answered here in problem+json rather than handed to whatever
// else claims the namespace.
const Prefix = "/api/v1/cashback/withdrawals"

// DestinationsPrefix is where a member records and reads the places they may
// be paid. A SIBLING of the withdrawals tree rather than a path beneath it:
// a destination outlives the withdrawals that name it, and FR-051 makes
// proving one is yours a separate act from asking to be paid at it.
//
// Served by this module's handler all the same, which the composition root
// mounts at both prefixes - one module, one auth gate, one error convention.
const DestinationsPrefix = destinationsPath

// maxBodyBytes caps a request body on this surface. The largest legal one is
// a destination's details, which is a handful of fields; anything larger is
// not a request these endpoints have a reading of.
const maxBodyBytes = 16 << 10

// destinationsPath is where a member's payout destinations live, and
// withdrawalsPath is [Prefix] itself - a member asks to be paid at the same
// address they read their requests from.
const destinationsPath = "/api/v1/cashback/payout-destinations"

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
	log          *slog.Logger
	withdrawals  *Withdrawals
	destinations *Destinations
	// vault is where bank details go instead of into this database
	// (ADR-0003). May be nil: a deployment that has not chosen one answers
	// 503 on the one endpoint that needs it and serves the rest.
	vault DetailsVault
	auth  MemberAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the withdrawal route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, withdrawals *Withdrawals, destinations *Destinations, vault DetailsVault, auth MemberAuthenticator) (http.Handler, error) {
	switch {
	case log == nil:
		return nil, errors.New("payout: the withdrawal handler needs a logger")
	case withdrawals == nil:
		return nil, ErrNoWithdrawalStore
	case destinations == nil:
		return nil, ErrNoDestinationStore
	case auth == nil:
		return nil, errors.New("payout: the withdrawal handler needs somewhere to authenticate members")
	}
	// vault is deliberately absent from that list: see [ErrNoVault].
	h := &Handler{log: log, withdrawals: withdrawals, destinations: destinations, vault: vault, auth: auth}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Both trees get the catch-all, so a stray sub-path under either is
	// answered here in problem+json rather than handed on.
	for _, prefix := range []string{Prefix, DestinationsPrefix} {
		mux.HandleFunc(prefix+"/", h.handleUnrouted)
		mux.HandleFunc(prefix, h.handleUnrouted)
	}
	return h.requireMember(mux), nil
}

// routes maps every withdrawal route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against somebody's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST " + Prefix:             h.postWithdrawal,
		"GET " + Prefix:              h.listWithdrawals,
		"GET " + Prefix + "/{id}":    h.getWithdrawal,
		"GET " + DestinationsPrefix:  h.listDestinations,
		"POST " + DestinationsPrefix: h.postDestination,
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

// timeFormat renders every timestamp on this surface: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone.
const timeFormat = time.RFC3339Nano

// stamp renders one instant, and the empty string for the zero value - a
// decision nobody has made yet has no date, and "0001-01-01" is a date a
// client would render.
func stamp(at time.Time) *string {
	if at.IsZero() {
		return nil
	}
	rendered := at.UTC().Format(timeFormat)
	return &rendered
}

// withdrawalItem is one request as a member reads it back.
type withdrawalItem struct {
	RequestID     string     `json:"request_id"`
	DestinationID string     `json:"destination_id"`
	Amount        amountJSON `json:"amount"`
	State         string     `json:"state"`
	RequestedAt   string     `json:"requested_at"`
	// DecidedAt and DecisionReason are the operator's decision, null until
	// one is made. The reason is what a refused member is owed (FR-061), so
	// it is on the member's own view of their request rather than only on
	// the operator's.
	DecidedAt      *string `json:"decided_at"`
	DecisionReason *string `json:"decision_reason"`
	// PayoutReference is what the rail called the payment, null until a
	// payout exists and the rail has answered. It is the string a member
	// quotes to their bank, which is why it reaches them at all.
	PayoutReference *string `json:"payout_reference"`
}

// listWithdrawals implements GET /api/v1/cashback/withdrawals.
func (h *Handler) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	made, err := h.withdrawals.List(r.Context(), member.ID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "listing a member's withdrawals", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	items := make([]withdrawalItem, 0, len(made))
	for _, one := range made {
		items = append(items, withdrawalItemOf(one))
	}
	h.writeJSON(w, r, http.StatusOK, struct {
		Items []withdrawalItem `json:"items"`
	}{Items: items})
}

// getWithdrawal implements GET /api/v1/cashback/withdrawals/{id}.
func (h *Handler) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the withdrawal id is not a UUID")
		return
	}
	one, err := h.withdrawals.Get(r.Context(), member.ID, id)
	switch {
	case errors.Is(err, ErrWithdrawalNotFound):
		// The same 404 for another member's request and for one that does
		// not exist, so an id cannot be probed for existence.
		platformhttp.Problem(w, http.StatusNotFound, "no such withdrawal request")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "reading a withdrawal", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	h.writeJSON(w, r, http.StatusOK, withdrawalItemOf(one))
}

// withdrawalItemOf renders one request.
func withdrawalItemOf(one Withdrawal) withdrawalItem {
	item := withdrawalItem{
		RequestID:     one.ID.String(),
		DestinationID: one.Destination.String(),
		Amount:        figure(one.Amount),
		State:         one.State.String(),
		RequestedAt:   one.RequestedAt.UTC().Format(timeFormat),
		DecidedAt:     stamp(one.DecidedAt),
	}
	if one.DecisionReason != "" {
		reason := one.DecisionReason
		item.DecisionReason = &reason
	}
	return item
}

// destinationItem is one destination as a member reads it back.
//
// There is no details field and there never will be. What a member sees is
// which rail it is for and whether it has been proved theirs; the details
// are somewhere this service cannot read them from (ADR-0003), and an
// endpoint that echoed them would be the leak the whole arrangement exists
// to prevent.
type destinationItem struct {
	DestinationID string `json:"destination_id"`
	Kind          string `json:"kind"`
	// VerifiedAt is null until the member has proved this destination is
	// theirs (FR-051), and VerifiedMethod says how. A withdrawal to one
	// that is null is refused, so a client can tell before asking.
	VerifiedAt     *string `json:"verified_at"`
	VerifiedMethod *string `json:"verified_method"`
	CreatedAt      string  `json:"created_at"`
}

func destinationItemOf(d Destination) destinationItem {
	item := destinationItem{
		DestinationID: d.ID.String(),
		Kind:          d.Kind.String(),
		VerifiedAt:    stamp(d.VerifiedAt),
		CreatedAt:     d.CreatedAt.UTC().Format(timeFormat),
	}
	if d.VerifiedMethod != "" {
		method := d.VerifiedMethod
		item.VerifiedMethod = &method
	}
	return item
}

// listDestinations implements GET /api/v1/cashback/payout-destinations.
func (h *Handler) listDestinations(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	held, err := h.destinations.List(r.Context(), member.ID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "listing a member's payout destinations", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	items := make([]destinationItem, 0, len(held))
	for _, one := range held {
		items = append(items, destinationItemOf(one))
	}
	h.writeJSON(w, r, http.StatusOK, struct {
		Items []destinationItem `json:"items"`
	}{Items: items})
}

// destinationRequestBody is what a member sends.
//
// Details arrive as a raw JSON document rather than a typed shape, because
// what they contain differs by rail and the vault is the only thing
// positioned to judge. This package does not look inside, does not store the
// value, and does not put it in an error - it passes straight through to
// [DetailsVault.Store] and what comes back is a reference.
type destinationRequestBody struct {
	Kind    string          `json:"kind"`
	Details json.RawMessage `json:"details"`
}

// postDestination implements POST /api/v1/cashback/payout-destinations.
//
// It answers 201 with verified_at null, always. Verification is a separate
// flow (FR-051) and a destination that arrived verified would be one nobody
// proved belongs to the member, which is the whole thing the check exists to
// stop.
func (h *Handler) postDestination(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	var body destinationRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	kind, err := ParseKind(body.Kind)
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"kind must be one of "+strings.Join(Kinds(), ", "))
		return
	}
	if len(body.Details) == 0 {
		platformhttp.Problem(w, http.StatusBadRequest,
			"details are required: a destination that says nowhere is one no rail could pay")
		return
	}
	if h.vault == nil {
		// 503, not 500: nothing is broken, the deployment has nowhere to
		// put bank details, and only this one endpoint needs one. Reading
		// destinations and withdrawing to them keep working.
		h.log.ErrorContext(r.Context(), "a payout destination was offered and this deployment has no details vault")
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment cannot record payout details yet")
		return
	}

	reference, err := h.vault.Store(r.Context(), kind, body.Details)
	switch {
	case errors.Is(err, ErrDetailsRefused):
		// The member's to act on, and answered WITHOUT quoting what they
		// sent: an error is the least controlled string in a system, and
		// this one would otherwise carry an IBAN into a log.
		platformhttp.Problem(w, http.StatusBadRequest,
			"those details were refused for a "+kind.String()+" destination")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "storing payout details", "kind", kind, "error", err)
		platformhttp.Problem(w, http.StatusBadGateway,
			"the details could not be stored; nothing was recorded")
		return
	}

	recorded, err := h.destinations.Record(r.Context(), NewDestination{
		AccountID: member.ID, Kind: kind, DetailsRef: reference,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidDestination) {
			platformhttp.Problem(w, http.StatusBadRequest, "that does not describe a payout destination")
			return
		}
		// The details are in the vault and no row names them. Worth seeing:
		// it is an orphan somebody has to clear, not something a member can
		// do anything about.
		h.log.ErrorContext(r.Context(), "recording a payout destination after storing its details",
			"kind", kind, "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	h.writeJSON(w, r, http.StatusCreated, destinationItemOf(recorded))
}
