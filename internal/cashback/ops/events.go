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

// TypeStatementImported is published when an operator imports a network's
// statement (US6, FR-061). The subject is the run, and it is published once
// per run: a retry that finds its statement already imported announces
// nothing, because the import it would describe is already in the stream.
const TypeStatementImported = EventProducer + ".reconciliation.statement_imported"

// TypeDifferenceFound is published once for every difference detection
// records (US6, FR-060), as the events contract lists it: one event per
// row, its subject the difference itself, so a consumer following one
// disagreement reads its lane in order. A pass that records nothing
// publishes nothing: the queue already says everything it would have said.
const TypeDifferenceFound = EventProducer + ".reconciliation.difference_found"

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

// importedPayload is what a statement import announces: which run, for which
// publisher account and period, how much it says, and who imported it. The
// statement itself is not in the payload - it is in the run, immutably, and
// a consumer that needs the lines reads the run by id.
type importedPayload struct {
	RunID            string `json:"run_id"`
	NetworkAccountID string `json:"network_account_id"`
	NetworkID        string `json:"network_id"`
	PeriodStart      string `json:"period_start"`
	PeriodEnd        string `json:"period_end"`
	Lines            int    `json:"lines"`
	StatementDigest  string `json:"statement_digest"`
	ImportedBy       string `json:"imported_by"`
	At               string `json:"at"`
}

// importedEvent renders the payload for one recorded import.
func importedEvent(i ImportedStatement) (json.RawMessage, error) {
	payload, err := json.Marshal(importedPayload{
		RunID:            i.ID.String(),
		NetworkAccountID: i.Account.String(),
		NetworkID:        i.Network.String(),
		PeriodStart:      i.Period.Start.UTC().Format(time.RFC3339Nano),
		PeriodEnd:        i.Period.End.UTC().Format(time.RFC3339Nano),
		Lines:            i.Lines,
		StatementDigest:  i.Digest,
		ImportedBy:       i.ImportedBy.String(),
		At:               i.ImportedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("ops: rendering %s for run %s: %w", TypeStatementImported, i.ID, err)
	}
	return payload, nil
}

// importedKey is the event's idempotency key. A run is written once - the
// row is immutable and the constraint makes a second identical import the
// same run - so the run id is the whole key.
func importedKey(id uuid.UUID) string { return TypeStatementImported + ":" + id.String() }

// foundPayload is what one recorded difference announces: which run, which
// row, what kind, and the money as a delta - what was paid less what was
// owed, in the shape C-6 mandates. The figures behind the delta are on the
// row, which the operator queue reads; the event is the fact that a
// disagreement now exists.
type foundPayload struct {
	RunID        string     `json:"run_id"`
	DifferenceID string     `json:"difference_id"`
	Kind         string     `json:"kind"`
	Delta        amountJSON `json:"delta"`
	At           string     `json:"at"`
}

// foundEvent renders the payload for one recorded difference.
func foundEvent(run, id uuid.UUID, d Difference, at time.Time) (json.RawMessage, error) {
	delta, err := d.Delta()
	if err != nil {
		return nil, fmt.Errorf("ops: rendering %s for difference %s: %w", TypeDifferenceFound, id, err)
	}
	payload, err := json.Marshal(foundPayload{
		RunID:        run.String(),
		DifferenceID: id.String(),
		Kind:         string(d.Kind),
		Delta:        amountJSON{Minor: delta.Minor, Currency: delta.Currency.String()},
		At:           at.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("ops: rendering %s for difference %s: %w", TypeDifferenceFound, id, err)
	}
	return payload, nil
}

// foundKey is the event's idempotency key. A row is recorded once - the
// indexes of 0029 make a repeat skip it - so the row id is the whole key.
func foundKey(id uuid.UUID) string { return TypeDifferenceFound + ":" + id.String() }
