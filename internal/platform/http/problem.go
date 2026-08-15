package http

import (
	"encoding/json"
	"net/http"
)

// ProblemDetails is the RFC 9457 problem+json error body every endpoint in
// the HTTP contract answers with. Type stays "about:blank", which per the
// RFC makes Title the plain rendering of the status code; Detail carries
// the human explanation of this particular occurrence. Modules use this for
// every error body; ad-hoc error shapes are a contract violation.
type ProblemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Problem writes an RFC 9457 problem+json response with the given status
// code and detail. The body is marshalled before anything is written, so a
// failure cannot leave a half-written body behind a status line that
// promised problem+json.
func Problem(w http.ResponseWriter, status int, detail string) {
	body, err := json.Marshal(ProblemDetails{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
	if err != nil {
		// Unreachable with this struct; keep the response well-formed anyway.
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	// Best effort: a client that vanished mid-write already has its status
	// line, and there is nothing further to do.
	_, _ = w.Write(body)
}
