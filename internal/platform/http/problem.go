package http

import (
	"encoding/json"
	"net/http"
)

// ProblemDetails is the RFC 9457 problem+json error body every endpoint in
// the HTTP contract answers with. Type stays "about:blank", which per the
// RFC makes Title the plain rendering of the status code; Detail carries
// the human explanation of this particular occurrence.
type ProblemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Problem writes an RFC 9457 problem+json response with the given status
// code and detail. The response body is best-effort: a client that vanished
// mid-write already has its status line, and there is nothing further to do.
func Problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ProblemDetails{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}
