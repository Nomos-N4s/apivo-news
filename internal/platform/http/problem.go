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
	ProblemWith(w, status, detail, nil)
}

// ProblemWith writes the same document carrying EXTENSION MEMBERS - the
// machine-readable half of RFC 9457 §3.2, alongside the human-readable
// Detail.
//
// It exists because some refusals are ones a client acts on rather than
// displays. "Insufficient confirmed balance, 4.00 short" is two facts: a
// code the client branches on and a figure it renders, and a client that had
// to parse either out of Detail would be parsing prose that changes when
// somebody improves the wording. The RFC's answer is extension members, so
// that is the answer here rather than a second error shape.
//
// The four standard members always win: an extension named "type", "title",
// "status" or "detail" is dropped rather than allowed to overwrite the
// document's own. A caller doing that has made a mistake, and the one thing
// worse than dropping it is answering with a status field that disagrees
// with the status line.
func ProblemWith(w http.ResponseWriter, status int, detail string, extensions map[string]any) {
	document := map[string]any{}
	for name, value := range extensions {
		switch name {
		case "type", "title", "status", "detail":
			continue
		}
		document[name] = value
	}
	document["type"] = "about:blank"
	document["title"] = http.StatusText(status)
	document["status"] = status
	if detail != "" {
		document["detail"] = detail
	}

	body, err := json.Marshal(document)
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
