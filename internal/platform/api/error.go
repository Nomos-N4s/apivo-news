package api

// APIError is the standard error response for all API endpoints.
// It provides a machine‑readable error code and a human‑readable message.
type APIError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}
