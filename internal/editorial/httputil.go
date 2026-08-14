package editorial

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// timeFormat renders every timestamp in editorial responses: RFC 3339 with
// sub-second precision, in UTC, so payloads are stable across the server's
// local zone.
const timeFormat = time.RFC3339Nano

// decodeJSON decodes the request body into dst, answering the 400 itself
// when the body is not the expected shape. Unknown fields are rejected: a
// misspelled field silently ignored would read as acceptance.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	// A second document after the first is not a JSON body, it is two.
	if dec.More() {
		platformhttp.Problem(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}

// writeJSON writes a JSON response body with the given status.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing editorial response", "error", err)
	}
}

// internalError logs the failure and answers an opaque 500: internals are
// for the log, never the wire.
func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	h.log.ErrorContext(r.Context(), doing, "error", err)
	platformhttp.Problem(w, http.StatusInternalServerError, "")
}

// blank reports whether s is empty or whitespace-only, matching the
// database's btrim checks.
func blank(s string) bool { return strings.TrimSpace(s) == "" }
