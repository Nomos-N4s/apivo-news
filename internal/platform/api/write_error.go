package api

import (
    "encoding/json"
    "net/http"
)

// writeAPIError writes an APIError as a problem+json response.
func writeAPIError(w http.ResponseWriter, status int, err APIError) {
    w.Header().Set("Content-Type", "application/problem+json")
    w.WriteHeader(status)
    // According to RFC 7807 we also include "status" field.
    resp := struct {
        Type   string `json:"type,omitempty"`
        Title  string `json:"title,omitempty"`
        Status int    `json:"status"`
        Code   int    `json:"code"`
        Detail string `json:"detail"`
    }{
        Title:  http.StatusText(status),
        Status: status,
        Code:   err.Code,
        Detail: err.Message,
    }
    _ = json.NewEncoder(w).Encode(resp)
}
