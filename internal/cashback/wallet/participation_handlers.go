// The member's opt-in over HTTP: read it, accept the terms, leave
// (T080, US3, FR-001..003).
//
// Three methods on one path, because there is one participation per member
// and the member is the bearer of the token. There is no path segment and no
// query parameter naming an account, so opting somebody else in - or out -
// is not a request these endpoints can express.

package wallet

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// maxParticipationBody bounds the opt-in body. It carries one short version
// string, so anything approaching this is not a client that means well.
const maxParticipationBody = 4 << 10

// participationResponse is the participation resource, as
// contracts/http-api.md spells it.
type participationResponse struct {
	Status string `json:"status"`
	// OptedInAt is when they accepted, re-stated on a re-join: the date is
	// half of the FR-002 record, and a member reading it is reading when
	// the terms beside it took effect for them.
	OptedInAt    string `json:"opted_in_at"`
	TermsVersion string `json:"terms_version"`
	// DefaultCurrency is the currency their wallet is denominated in, as an
	// explicit ISO-4217 code (C-6).
	DefaultCurrency string `json:"default_currency"`
	// LeftAt is null unless they have left. Leaving is a status and a date,
	// never a deletion (FR-003), and both are here so a client can say when
	// rather than only that.
	LeftAt *string `json:"left_at"`
}

// participationFrom renders one participation for the wire.
func participationBody(held Participation) participationResponse {
	body := participationResponse{
		Status:          held.Status,
		OptedInAt:       held.OptedInAt.UTC().Format(timeFormat),
		TermsVersion:    held.TermsVersion,
		DefaultCurrency: string(held.Currency),
	}
	if !held.LeftAt.IsZero() {
		left := held.LeftAt.UTC().Format(timeFormat)
		body.LeftAt = &left
	}
	return body
}

// optInRequest is the body of POST /participation.
type optInRequest struct {
	// TermsVersion is the revision the member is accepting. Required, and
	// checked against the one in force: a stale consent is never recorded
	// (FR-002).
	TermsVersion string `json:"terms_version"`
}

// getParticipation implements GET /api/v1/cashback/participation.
//
// 404 when they have never opted in, which is not an error condition: it is
// how the frontend knows to render the opt-in rather than a wallet.
func (h *Handler) getParticipation(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	held, err := h.participations.Of(r.Context(), member.ID)
	switch {
	case errors.Is(err, ErrNotJoined):
		platformhttp.Problem(w, http.StatusNotFound,
			"this account has not opted into cashback")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "reading a participation", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	h.writeJSON(w, r, http.StatusOK, participationBody(held))
}

// postParticipation implements POST /api/v1/cashback/participation (FR-002).
func (h *Handler) postParticipation(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	var req optInRequest
	if !h.decodeBody(w, r, &req) {
		return
	}

	joined, err := h.participations.Join(r.Context(), member.ID, req.TermsVersion)
	switch {
	case errors.Is(err, ErrNoBrand):
		// 503 rather than 500: nothing is broken, the deployment has no
		// brand definition and so has no terms to offer. Named key, because
		// the person reading this log line is the person who can set it.
		h.log.ErrorContext(r.Context(), "a member tried to opt in and this deployment has no brand definition",
			"key", "BRAND_DIR")
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment has not configured a brand, so there are no terms to accept")
		return
	case errors.Is(err, ErrStaleTerms):
		// The version in force is named in the refusal. A client that had
		// to guess would guess, and a member would end up accepting
		// whatever the client had cached.
		platformhttp.Problem(w, http.StatusBadRequest,
			"terms_version must be the version in force, which is "+
				h.participations.Terms().Version)
		return
	case errors.Is(err, ErrAlreadyJoined):
		platformhttp.Problem(w, http.StatusConflict,
			"this account is already in cashback; read the participation to see which terms it accepted and when")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "recording an opt-in", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	h.writeJSON(w, r, http.StatusCreated, participationBody(joined))
}

// deleteParticipation implements DELETE /api/v1/cashback/participation
// (FR-003).
//
// 200 with the closed participation rather than 204, because leaving has a
// date and the member is owed it. Financial records are untouched: entries
// already earned continue to resolve, and payouts already made stand.
//
// Idempotent. A second DELETE answers the same 200 with the same date, and
// publishes nothing further.
func (h *Handler) deleteParticipation(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())

	left, err := h.participations.Leave(r.Context(), member.ID)
	switch {
	case errors.Is(err, ErrNotJoined):
		platformhttp.Problem(w, http.StatusNotFound,
			"this account has not opted into cashback, so there is nothing to leave")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "closing a participation", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}
	h.writeJSON(w, r, http.StatusOK, participationBody(left))
}

// decodeBody reads the request body into dst, answering the 400 itself when
// it is not the expected shape.
//
// Unknown fields are rejected, for the reason this module rejects unknown
// query parameters: a misspelled terms_version silently dropped would reach
// the service as the empty string, and the member would be told their
// consent was stale when what was wrong was the client.
func (h *Handler) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxParticipationBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"request body is not valid JSON for this endpoint: "+err.Error())
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
