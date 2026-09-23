package dto

import "encoding/json"

type AmountJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

type WithdrawalRequestBody struct {
	DestinationID string     `json:"destination_id"`
	Amount        AmountJSON `json:"amount"`
}

type WithdrawalResponse struct {
	RequestID      string     `json:"request_id"`
	State          string     `json:"state"`
	ReservedAmount AmountJSON `json:"reserved_amount"`
}

type WithdrawalItem struct {
	RequestID      string     `json:"request_id"`
	DestinationID  string     `json:"destination_id"`
	Amount         AmountJSON `json:"amount"`
	State          string     `json:"state"`
	RequestedAt    string     `json:"requested_at"`
	DecidedAt      *string    `json:"decided_at"`
	DecisionReason *string    `json:"decision_reason,omitempty"`
}

type DestinationRequestBody struct {
	Kind    string          `json:"kind"`
	Details json.RawMessage `json:"details"`
}

type DestinationItem struct {
	DestinationID  string  `json:"destination_id"`
	Kind           string  `json:"kind"`
	VerifiedAt     *string `json:"verified_at"`
	VerifiedMethod *string `json:"verified_method,omitempty"`
	CreatedAt      string  `json:"created_at"`
}

type WithdrawalListResponse struct {
	Items []WithdrawalItem `json:"items"`
}

type DestinationListResponse struct {
	Items []DestinationItem `json:"items"`
}
