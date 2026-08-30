package clickout

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

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Prefix is the path this module serves, and the prefix every pattern below
// shares. The composition root mounts the handler at it AND at it plus a
// trailing slash, so a stray sub-path is answered here in problem+json
// rather than redirected or handed to whatever else claims the namespace.
//
// It is exported so the composition root mounts exactly the string the
// catch-all claims: two copies of it would be one deployment away from a
// path that answers in text/plain because nothing under it was ever reached.
const Prefix = "/api/v1/cashback/clickouts"

// maxBodyBytes bounds every request body here. The only legal one is a
// single offer id, so this is orders of magnitude of headroom and exists
// only so a hostile body cannot be read into memory.
const maxBodyBytes = 4 << 10

// timeFormat renders every timestamp in click-out responses: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone.
const timeFormat = time.RFC3339Nano

// Handler serves the member click-out endpoint. Build it with NewHandler.
type Handler struct {
	log       *slog.Logger
	clickouts *ClickOuts
	auth      MemberAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the click-out route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, clickouts *ClickOuts, auth MemberAuthenticator) http.Handler {
	h := &Handler{log: log, clickouts: clickouts, auth: auth}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under this path is problem+json, including the ones nobody
	// wrote a handler for - the convention every other module here holds, so
	// the API has one error shape rather than one per module plus ServeMux's
	// text/plain in the corners.
	mux.HandleFunc(Prefix+"/", h.handleUnrouted)
	mux.HandleFunc(Prefix, h.handleUnrouted)
	return h.requireMember(mux)
}

// routes maps every click-out route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST " + Prefix: h.createClickOut,
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
// this runs. A known path reached with the wrong method is 405 (with Allow,
// as HTTP requires); anything else is 404.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	if allow := h.allow.MethodsFor(r.URL.Path); allow != "" {
		w.Header().Set("Allow", allow)
		platformhttp.Problem(w, http.StatusMethodNotAllowed,
			r.Method+" is not allowed on this endpoint; use "+allow)
		return
	}
	platformhttp.Problem(w, http.StatusNotFound, "no such endpoint")
}

// clickOutRequest is the body of POST /clickouts: the band being clicked
// through, and nothing else. The member comes from the token and the rate
// from the offer - a body that could name either would be a body that could
// claim somebody else's click or a rate nobody published.
type clickOutRequest struct {
	OfferID string `json:"offer_id"`
}

// clickOutResponse is the tracked redirect.
type clickOutResponse struct {
	// ClickRef is the reference the network will echo back against the
	// purchase. It is returned so a member support conversation can quote
	// the same value the evidence is matched on.
	ClickRef string `json:"click_ref"`
	// RedirectURL is where to send the member.
	RedirectURL string `json:"redirect_url"`
	// ExpiresAt is when the rate band this click was issued against stops
	// being published, or null for a band with no published end.
	//
	// It is about the OFFER, not about the redirect: the credit this click
	// earns is governed by the snapshot taken a moment ago whatever happens
	// to the band afterwards (FR-013), and no network expires a deeplink.
	// What a caller can do with it is decide when to stop showing this band
	// as available - which is the same window whose end makes a later
	// click-out answer 409.
	ExpiresAt *string `json:"expires_at"`
}

// createClickOut implements POST /api/v1/cashback/clickouts.
func (h *Handler) createClickOut(w http.ResponseWriter, r *http.Request) {
	var req clickOutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	offerID, err := parseOfferID(req.OfferID)
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, err.Error())
		return
	}

	issued, err := h.clickouts.Issue(r.Context(), memberFrom(r.Context()).ID, offerID)
	switch {
	// The band is not published at this moment - expired, not yet started,
	// or with an inactive leg in its chain. A member looking at a stale page
	// gets told, rather than being redirected to a rate nobody honours.
	case errors.Is(err, ErrOfferNotAvailable):
		platformhttp.Problem(w, http.StatusConflict,
			"this offer is not available: it has expired, or its retailer is no longer active")
		return
	// The redirect could not be built, so nothing was recorded. 502 rather
	// than 500: what failed is the route out to the network, and the member
	// is told plainly that this one cannot be opened right now.
	case errors.Is(err, ErrNoRedirect):
		h.log.ErrorContext(r.Context(), "no redirect could be built for a click-out",
			"error", err, "offer", offerID.String())
		platformhttp.Problem(w, http.StatusBadGateway,
			"this offer cannot be opened right now; nothing has been recorded, so trying again later is safe")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "issuing a click-out", "error", err, "offer", offerID.String())
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	body := clickOutResponse{
		ClickRef:    issued.Click.Ref.Ref(),
		RedirectURL: issued.RedirectURL,
	}
	if !issued.OfferEndsAt.IsZero() {
		ends := issued.OfferEndsAt.UTC().Format(timeFormat)
		body.ExpiresAt = &ends
	}
	h.writeJSON(w, r, http.StatusCreated, body)
}

// writeJSON writes a JSON response body with the given status.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing a click-out response", "error", err)
	}
}

// decodeJSON decodes the request body into dst, answering the 400 itself
// when the body is not the expected shape. Unknown fields are rejected: a
// misspelled field silently ignored would read as acceptance of a click the
// caller did not describe.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	// Everything after the first document must be whitespace, and only a
	// second Decode proves it: Decoder.More() answers the narrower question
	// "is another VALUE coming?", so a stray closing delimiter after a valid
	// object would read as "no more values" and be accepted.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		platformhttp.Problem(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}

// parseOfferID reads the offer id from the request body, refusing anything
// that cannot name a band. A blank field is called out separately from a
// malformed one: they are different mistakes and a client fixing either
// wants to know which it made.
func parseOfferID(raw string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, errors.New("offer_id is required")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errors.New("offer_id is not a UUID")
	}
	if id == uuid.Nil {
		// Parseable and still not an offer. Refused here so the failure is
		// this message rather than a foreign key violation two layers down.
		return uuid.Nil, errors.New("offer_id names no offer")
	}
	return id, nil
}
