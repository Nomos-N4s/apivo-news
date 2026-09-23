package http

import (
	"encoding/json"
	"net/http"

	"github.com/Nomos-N4s/apivo-news/internal/platform/api/dto"
)

// ProblemDetails is kept as an alias for compatibility with existing
// callers and tests. New code should use dto.APIError directly.
type ProblemDetails = dto.APIError

// Problem writes an RFC 9457 problem+json response with the given status
// code and detail. The body is marshalled before anything is written, so a
// failure cannot leave a half-written body behind a status line that
// promised problem+json.
func Problem(w http.ResponseWriter, status int, detail string) {
	ProblemWith(w, status, detail, nil)
}

// ProblemWith writes the same document carrying extension members - the
// machine-readable half of RFC 9457 §3.2, alongside the human-readable
// Detail.
//
// The four standard members always win: an extension named "type", "title",
// "status" or "detail" is ignored rather than allowed to overwrite the
// document's own fields.
func ProblemWith(w http.ResponseWriter, status int, detail string, extensions map[string]any) {
	problem := dto.APIError{
		Type:       "about:blank",
		Title:      http.StatusText(status),
		Status:     status,
		Detail:     detail,
		Extensions: extensions,
	}

	body, err := json.Marshal(problem)
	if err != nil {
		// Reachable only through an extension value that cannot be
		// marshalled; the status still has to reach the client.
		http.Error(w, http.StatusText(status), status)
		return
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)

	// Best effort: a client that vanished mid-write already has its status
	// line, and there is nothing further to do.
	_, _ = w.Write(body)
}
