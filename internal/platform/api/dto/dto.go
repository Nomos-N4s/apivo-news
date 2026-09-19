// Package dto provides standard response DTOs for all API endpoints.
// All handlers should use these types to ensure consistent API responses.
package dto

// APIError is the standard error response for all API endpoints.
// It provides a machine‑readable code and a human‑readable message.
//
// swagger:response apiError
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// SuccessResponse is a standard success response wrapper.
//
// swagger:response successResponse
type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}
