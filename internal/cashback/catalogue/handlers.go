// GET /merchants/{slug}: one retailer's page, in the reader's language, with
// every rate band we publish for them (T104, US5 scenario 3, FR-011).
//
// Every rate on this page is what the MEMBER earns. The band behind it
// records the network's commission and the share of it the member receives;
// the domain composes the two before a Listing or a Merchant exists, so
// there is no path from here to the commission and nothing on this surface
// to get wrong.

package catalogue

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// MerchantPrefix is the path this module serves. The composition root mounts
// the handler at it AND at it plus a trailing slash, so a stray sub-path is
// answered here in problem+json rather than handed to whatever else claims
// the namespace.
//
// It is exported so the composition root mounts exactly the string the
// catch-all claims: two copies of it would be one deployment away from a
// path that answers in text/plain because nothing under it was ever reached.
//
// Deliberately NOT the bare /api/v1/cashback prefix. A catch-all there would
// swallow every other module's routes into this one's 404.
const MerchantPrefix = "/api/v1/cashback/merchants"

// merchantPath is the one route, with the slug as a path segment.
const merchantPath = MerchantPrefix + "/{slug}"

// languageParam is how a reader says which language they want. Separate from
// place, always, because language and place are independent axes
// (constitution VII) - a Greek speaker in Munich is one reader, not a
// contradiction to resolve.
const languageParam = "lang"

// timeFormat renders every timestamp in catalogue responses: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone.
const timeFormat = time.RFC3339Nano

// Handler serves the member-facing catalogue. Build it with NewHandler.
type Handler struct {
	log       *slog.Logger
	merchants *MerchantReader
	auth      MemberAuthenticator
	// now is the clock the page is read against, injectable so a test can
	// pin the moment a band's validity window is judged at.
	now func() time.Time
	// allow is the 405 classifier, derived from routes() in NewHandler so it
	// cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// HandlerOption adjusts a Handler at construction.
type HandlerOption func(*Handler)

// WithPageClock pins the moment pages are read at. Production leaves it
// alone and gets time.Now. Named apart from the import's own clock because
// the two answer different questions: when a catalogue was retrieved, and
// when a member is looking at it.
func WithPageClock(now func() time.Time) HandlerOption {
	return func(h *Handler) {
		if now != nil {
			h.now = now
		}
	}
}

// NewHandler builds the catalogue route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireMember gate.
func NewHandler(log *slog.Logger, merchants *MerchantReader, auth MemberAuthenticator, opts ...HandlerOption) http.Handler {
	h := &Handler{log: log, merchants: merchants, auth: auth, now: time.Now}
	for _, opt := range opts {
		opt(h)
	}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under this path is problem+json, including the ones nobody
	// wrote a handler for - the convention every other module here holds.
	mux.HandleFunc(MerchantPrefix+"/", h.handleUnrouted)
	mux.HandleFunc(MerchantPrefix, h.handleUnrouted)
	return h.requireMember(mux)
}

// routes maps every catalogue route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET " + merchantPath: h.getMerchant,
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

// rateResponse is one published band as a member sees it.
//
// The rate is what the MEMBER earns, not the network's commission. Exactly
// one of Bps and Amount carries it, chosen by Kind, which is the shape the
// band itself has and the shape the click snapshot is written in: a percent
// band is basis points, a fixed band is minor units beside an explicit
// currency (C-6). Never a decimal string and never a float, at any point
// between the schema and the screen - the frontend formats for display from
// these fields.
type rateResponse struct {
	// OfferID is the band this page would issue a click against. It is the
	// value POST /clickouts takes, so a page and the click it leads to name
	// the same rate.
	OfferID string `json:"offer_id"`
	// Kind is "percent" or "fixed", and says which of the two below carries
	// the rate.
	Kind string `json:"kind"`
	// Bps is the member's rate in basis points on a percent band (4% = 400),
	// absent on a fixed one.
	Bps *int32 `json:"bps,omitempty"`
	// Amount is the member's earning per transaction on a fixed band, in
	// minor units with its currency; absent on a percent band.
	Amount *money.Amount `json:"amount,omitempty"`
	// Conditions is what this band requires, verbatim as published (FR-011),
	// or null when it records none. Null rather than an empty string,
	// because "no conditions" and "conditions we failed to load" must not
	// render the same.
	Conditions *string `json:"conditions"`
	// Exclusions is what this band does not pay on (FR-011), or null when it
	// records none.
	Exclusions *string `json:"exclusions"`
	// ValidTo is when this band stops being published, or null for one with
	// no published end. "Until the end of the month" is part of what a
	// member is being offered.
	ValidTo *string `json:"valid_to"`
}

// merchantResponse is GET /merchants/{slug}.
type merchantResponse struct {
	MerchantID string `json:"merchant_id"`
	Slug       string `json:"slug"`
	// Country is where the retailer trades, or null when they are bound to
	// none. Not where the reader is (constitution VII).
	Country *string `json:"country"`
	Name    string  `json:"name"`
	// NameLanguage is the language the copy on this page is actually in, and
	// NameIsFallback says whether that is the language that was asked for.
	// Together they are how US5 scenario 2 renders "shown in German" rather
	// than silently pretending (FR-010).
	NameLanguage   string  `json:"name_language"`
	NameIsFallback bool    `json:"name_is_fallback"`
	Summary        *string `json:"summary"`
	Terms          *string `json:"terms"`
	// TypicalConfirmationDays is how long a purchase here usually takes to
	// confirm, and it is ALWAYS null today.
	//
	// The contract names the field and the schema has nothing to compute it
	// from - not on the retailer, not on the route, not on the network. It
	// is emitted as null rather than omitted, because a client that reads
	// "we do not know yet" can say so, while a missing field is
	// indistinguishable from a client that forgot to ask. What it must never
	// be is a plausible constant: a member reads this as "you will be paid
	// in about six weeks".
	TypicalConfirmationDays *int `json:"typical_confirmation_days"`
	// Rates is every band in force right now, newest first. An empty list is
	// an ordinary answer - a retailer whose rates have lapsed pays nothing
	// today and is still a shop that exists - and never a 404.
	Rates []rateResponse `json:"rates"`
}

// getMerchant implements GET /api/v1/cashback/merchants/{slug}.
func (h *Handler) getMerchant(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	language := r.URL.Query().Get(languageParam)

	page, err := h.merchants.Detail(r.Context(), slug, language, h.now())
	switch {
	case errors.Is(err, ErrNoMerchant):
		platformhttp.Problem(w, http.StatusNotFound, "no such merchant")
		return
	case err != nil:
		// Including ErrNoCopy: a retailer we publish with no name in any
		// language is a broken row, not a missing one, and answering 404
		// would retire it into a number nobody investigates.
		h.log.ErrorContext(r.Context(), "reading a merchant page", "error", err, "merchant", slug)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	h.writeJSON(w, r, http.StatusOK, merchantOf(page))
}

// merchantOf maps the domain page onto the wire.
func merchantOf(page Merchant) merchantResponse {
	body := merchantResponse{
		MerchantID:     page.ID.String(),
		Slug:           page.Slug,
		Country:        textOrNull(page.Country),
		Name:           page.Copy.Name,
		NameLanguage:   page.Copy.Language,
		NameIsFallback: page.Copy.Fallback,
		Summary:        textOrNull(page.Copy.Summary),
		Terms:          textOrNull(page.Copy.Terms),
		// Always an empty list rather than null, so a client renders "no
		// rates today" without having to tell null from absent.
		Rates: make([]rateResponse, 0, len(page.Bands)),
	}
	for _, band := range page.Bands {
		body.Rates = append(body.Rates, rateOf(band))
	}
	return body
}

// rateOf maps one band onto the wire, writing exactly the field its kind
// uses.
func rateOf(band Band) rateResponse {
	rate := rateResponse{
		OfferID:    band.ID.String(),
		Kind:       string(band.Rate.Kind),
		Conditions: textOrNull(band.Conditions),
		Exclusions: textOrNull(band.Exclusions),
	}
	switch band.Rate.Kind {
	case RatePercent:
		bps := int32(band.Rate.Percent)
		rate.Bps = &bps
	case RateFixed:
		amount := band.Rate.Fixed
		rate.Amount = &amount
	}
	if !band.ValidTo.IsZero() {
		until := band.ValidTo.UTC().Format(timeFormat)
		rate.ValidTo = &until
	}
	return rate
}

// textOrNull renders an absent string as JSON null rather than "".
//
// The difference matters on this surface: a band with no exclusions and a
// band whose exclusions failed to load must not render identically, and an
// empty string is what a client would print as a heading with nothing under
// it.
func textOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeJSON writes a JSON response body with the given status.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing a catalogue response", "error", err)
	}
}
