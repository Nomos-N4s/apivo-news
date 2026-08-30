package ops

import (
	"encoding/json"
	"log/slog"
	"net/http"
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

// writeJSON writes a JSON response body with the given status.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
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
