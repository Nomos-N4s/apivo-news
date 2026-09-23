// Package dto contains the HTTP API's reusable response data-transfer
// objects. DTOs describe the wire representation; domain and persistence
// types should not be exposed directly by handlers.
package dto

import "encoding/json"

// APIError is the RFC 9457 problem+json response returned by the API.
//
// Type is "about:blank" for all standard API errors. Title is the standard
// HTTP status text, Status is the HTTP status code, and Detail contains the
// human-readable explanation when one is available.
type APIError struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     int            `json:"status"`
	Detail     string         `json:"detail,omitempty"`
	Extensions map[string]any `json:"-"`
}

// MarshalJSON implements json.Marshaler for APIError to handle arbitrary extensions.
// MarshalJSON implements json.Marshaler for APIError to handle arbitrary extensions.
func (e APIError) MarshalJSON() ([]byte, error) {
	type Alias APIError
	if len(e.Extensions) == 0 {
		return json.Marshal(&struct{ Alias }{Alias: (Alias)(e)})
	}
	m := make(map[string]any, 4+len(e.Extensions))
	for k, v := range e.Extensions {
		switch k {
		case "type", "title", "status", "detail":
			continue
		}
		m[k] = v
	}
	m["type"] = e.Type
	m["title"] = e.Title
	m["status"] = e.Status
	if e.Detail != "" {
		m["detail"] = e.Detail
	}
	return json.Marshal(m)
}
