package dto

import "encoding/json"

// AmountJSON represents an amount with its currency.
// AmountJSON represents an amount with its currency.
type AmountJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// WithdrawalRequestBody represents the payload for creating a withdrawal.
// WithdrawalRequestBody represents the payload for creating a withdrawal.
type WithdrawalRequestBody struct {
	DestinationID string     `json:"destination_id"`
	Amount        AmountJSON `json:"amount"`
}

// WithdrawalResponse represents the response after creating a withdrawal.
// WithdrawalResponse represents the response after creating a withdrawal.
type WithdrawalResponse struct {
	RequestID      string     `json:"request_id"`
	State          string     `json:"state"`
	ReservedAmount AmountJSON `json:"reserved_amount"`
}

// WithdrawalItem represents a single withdrawal in a list.
// WithdrawalItem represents a single withdrawal in a list.
type WithdrawalItem struct {
	RequestID      string     `json:"request_id"`
	DestinationID  string     `json:"destination_id"`
	Amount         AmountJSON `json:"amount"`
	State          string     `json:"state"`
	RequestedAt    string     `json:"requested_at"`
	DecidedAt      *string    `json:"decided_at"`
	DecisionReason *string    `json:"decision_reason,omitempty"`
}

// DestinationRequestBody represents the payload for creating a destination.
// DestinationRequestBody represents the payload for creating a destination.
type DestinationRequestBody struct {
	Kind    string          `json:"kind"`
	Details json.RawMessage `json:"details"`
}

// DestinationItem represents a single destination in a list.
// DestinationItem represents a single destination in a list.
type DestinationItem struct {
	DestinationID  string  `json:"destination_id"`
	Kind           string  `json:"kind"`
	VerifiedAt     *string `json:"verified_at"`
	VerifiedMethod *string `json:"verified_method,omitempty"`
	CreatedAt      string  `json:"created_at"`
}

// WithdrawalListResponse represents a list of withdrawals.
// WithdrawalListResponse represents a list of withdrawals.
type WithdrawalListResponse struct {
	Items []WithdrawalItem `json:"items"`
}

// DestinationListResponse represents a list of destinations.
// DestinationListResponse represents a list of destinations.
type DestinationListResponse struct {
	Items []DestinationItem `json:"items"`
}
