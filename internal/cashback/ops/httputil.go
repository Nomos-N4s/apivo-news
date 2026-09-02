package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// timeFormat renders every timestamp in operator responses: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone - and so a timestamp a client echoes back into a cursor is the
// one the row carries, to the microsecond Postgres stores.
const timeFormat = time.RFC3339Nano

// stamp renders one timestamp for the wire.
func stamp(at time.Time) string { return at.UTC().Format(timeFormat) }

// writeJSON writes a JSON response body with 200.
//
// Nearly every operator endpoint decides an existing row - dismissing,
// approving, refusing, resolving - and none of those creates a resource, so
// 200 is the whole answer. The one that does create, the statement import,
// says so through writeJSONStatus; keeping 200 the default here means a
// future endpoint has to ask for 201 rather than getting it by accident.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, body any) {
	h.writeJSONStatus(w, r, http.StatusOK, body)
}

// writeJSONStatus writes a JSON response body with the given status.
func (h *Handler) writeJSONStatus(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing operator response", "error", err)
	}
}

// internalError logs the failure and answers an opaque 500: internals are
// for the log, never the wire.
func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	h.log.ErrorContext(r.Context(), doing, "error", err, slog.String("operator", operatorFrom(r.Context()).ID.String()))
	platformhttp.Problem(w, http.StatusInternalServerError, "")
}

// decodeJSON decodes the request body into dst, answering the 400 itself
// when the body is not the expected shape. Unknown fields are rejected: on
// a surface where every action records a reason, a misspelled field
// silently ignored would read as acceptance of a decision nobody described.
//
// Bounded at maxBodyBytes, which every decision body fits in with room to
// spare; the statement import, whose body is a network's document, asks
// decodeJSONUpTo for more.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONUpTo(w, r, dst, maxBodyBytes)
}

// decodeJSONUpTo is decodeJSON with the body bounded at limit bytes.
func decodeJSONUpTo(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// Too large is its own answer, not a syntax error: the body may
		// have been perfectly well formed, and a client told its JSON was
		// invalid would look in the wrong place.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			platformhttp.Problem(w, http.StatusRequestEntityTooLarge,
				"request body is larger than this endpoint accepts ("+strconv.FormatInt(limit, 10)+" bytes)")
			return false
		}
		platformhttp.Problem(w, http.StatusBadRequest, "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	// Everything after the first document must be whitespace, and only a
	// second Decode proves it. Decoder.More() answers a narrower question -
	// "is another VALUE coming?" - so a stray closing delimiter (`}`, `]`)
	// after a valid object reads as "no more values" and a malformed body
	// would be accepted. Requiring io.EOF here rejects both a second
	// document and trailing syntax errors.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		platformhttp.Problem(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}

// emptyBody refuses a request body on an endpoint that takes none, answering
// the 400 itself.
//
// Silently ignoring one would be the wrong default on this surface. An
// approval records who and when, both taken from the token and the clock, so
// a client that sent a body believed it was saying something - most likely
// naming an approver - and C-4 means that is exactly what must not be
// accepted quietly.
func emptyBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the request body could not be read")
		return false
	}
	if len(bytes.TrimSpace(body)) != 0 {
		platformhttp.Problem(w, http.StatusBadRequest,
			"this endpoint takes no request body: who approves and when are read from the token and the clock (C-4)")
		return false
	}
	return true
}
