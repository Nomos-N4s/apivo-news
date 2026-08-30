package ops

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EventProducer is the domain every event this module publishes comes from,
// and the first segment of every type below.
const EventProducer = "cashback"

// TypeUnattributedDismissed is published when an operator closes a queue
// row without crediting anybody (FR-034, FR-061).
//
// Past tense, like every type in the contract: it is the fact that a named
// human decided this, not a request for anyone to do anything about it. The
// subject is the queue row, so a consumer that cares about one row's
// history reads its lane in order.
const TypeUnattributedDismissed = EventProducer + ".unattributed.dismissed"

// dismissedPayload is what the event carries: identifiers, the acting
// account and the recorded reason.
//
// The reason is in the payload rather than left to a lookup because that is
// what makes the event the audit record FR-061 asks for - a consumer that
// had to come back and ask the cashback schema why would be making exactly
// the synchronous call-back the event contract forbids.
type dismissedPayload struct {
	UnattributedID       string `json:"unattributed_id"`
	NetworkTransactionID string `json:"network_transaction_id"`
	ResolvedBy           string `json:"resolved_by"`
	Reason               string `json:"reason"`
	At                   string `json:"at"`
}

// dismissedEvent renders the payload for one recorded dismissal.
func dismissedEvent(d Dismissed) (json.RawMessage, error) {
	payload, err := json.Marshal(dismissedPayload{
		UnattributedID:       d.ID.String(),
		NetworkTransactionID: d.ReportID.String(),
		ResolvedBy:           d.ResolvedBy.String(),
		Reason:               d.Reason,
		At:                   d.ResolvedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("ops: rendering %s for row %s: %w", TypeUnattributedDismissed, d.ID, err)
	}
	return payload, nil
}

// dismissedKey is the event's idempotency key. A row is resolved at most
// once - the statement's own `resolved_at is null` guarantees it - so the
// row id is the whole key, and a second append under it means the
// resolution it describes is already in the stream.
func dismissedKey(id uuid.UUID) string { return TypeUnattributedDismissed + ":" + id.String() }
