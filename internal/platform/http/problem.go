package http

import (
	"encoding/json"
	"net/http"
)

// problem is the RFC 9457 problem-details body the API contract prescribes
// for every error response.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// Problem writes an RFC 9457 application/problem+json error response. The
// type is "about:blank", so per the RFC the title is the HTTP reason phrase
// and the detail carries the human-readable specifics. Modules use this for
// every error body; ad-hoc error shapes are a contract violation.
func Problem(w http.ResponseWriter, status int, detail string) {
	body, err := json.Marshal(problem{
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
	_, _ = w.Write(body)
}
