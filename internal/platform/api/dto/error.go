// Package dto contains the HTTP API's reusable response data-transfer
// objects. DTOs describe the wire representation; domain and persistence
// types should not be exposed directly by handlers.
package dto

// APIError is the RFC 9457 problem+json response returned by the API.
//
// Type is "about:blank" for all standard API errors. Title is the standard
// HTTP status text, Status is the HTTP status code, and Detail contains the
// human-readable explanation when one is available.
type APIError struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}
