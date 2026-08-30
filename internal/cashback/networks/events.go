// The two facts this module publishes: that a report was stored, and that
// one of them could not be attributed to anybody (T062).
//
// Both are appended to the outbox in the transaction that made the fact
// true, which is the whole of the contract's atomicity guarantee: "there is
// no code path that publishes an event without its state change, or commits
// a state change without its event". Nothing here opens a transaction, and
// nothing here retries - a failure to append is a failure of the poll, and
// the window is read again with its cursor where it was.
//
// These are the first events any part of the cashback product publishes.

package networks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

const (
	// EventProducer is the domain these facts belong to, and the first
	// segment of every type below - the writer refuses a type that names
	// another producer, because a domain publishes only its own facts.
	EventProducer = "cashback"
	// TypeTransactionIngested announces one stored report: what the network
	// said, and when we learned it.
	TypeTransactionIngested = EventProducer + ".transaction.ingested"
	// TypeTransactionUnattributed announces a report nobody can be credited
	// for yet (FR-034). It is a separate fact rather than a flag on the
	// first, because a consumer that cares about one rarely cares about the
	// other: unattributed work is an operator's queue, and ingestion is
	// everybody else's.
	TypeTransactionUnattributed = EventProducer + ".transaction.unattributed"
)

// ErrNotAnnounced reports an event that could not be appended beside the
// state change it describes.
//
// It is always fatal to the poll that raised it, and deliberately so. The
// append shares the poll's transaction, so a swallowed failure would commit
// evidence with no event - the one thing the contract says no code path may
// do - and the window would never be read again to notice.
var ErrNotAnnounced = errors.New("networks: the event could not be appended beside the evidence")

// Announcer appends this module's facts to the outbox.
//
// It holds no database handle. Every method takes the transaction that
// carries the state change, because the placement of the append - not
// anything this type does - is what makes the event and the fact atomic.
type Announcer struct {
	writer *events.Writer
}

// NewAnnouncer builds the announcer for this domain.
func NewAnnouncer() (*Announcer, error) {
	writer, err := events.NewWriter(EventProducer)
	if err != nil {
		return nil, err
	}
	return &Announcer{writer: writer}, nil
}

// Ingested announces one report the database stored (FR-030).
//
// Announced per STORED ROW, which is per new fact rather than per report
// read: an unchanged re-report writes no row and says nothing new, so it
// announces nothing. That is what keeps a trailing sweep over a quiet period
// from publishing the same fact four times a day forever.
//
// The payload carries identifiers and the normalised status, and nothing
// else. Consumer rule 5 is explicit that events carry identifiers and the
// data stays in its owning schema - so no amounts, no click reference, and
// above all no raw network payload.
func (a *Announcer) Ingested(ctx context.Context, db events.RowQuerier, network NetworkID, status Status, stored Recorded) error {
	if stored.ID == uuid.Nil {
		return fmt.Errorf("%w: %s about a report the database did not store", ErrNotAnnounced, TypeTransactionIngested)
	}
	payload, err := json.Marshal(struct {
		NetworkTransactionID uuid.UUID `json:"network_transaction_id"`
		NetworkID            NetworkID `json:"network_id"`
		Status               Status    `json:"status"`
		At                   time.Time `json:"at"`
	}{
		NetworkTransactionID: stored.ID,
		NetworkID:            network,
		Status:               status,
		// The retrieval instant the ROW carries, read back rather than
		// taken from the clock, so the event and the evidence name one
		// moment. A consumer joining the two on time would otherwise find
		// them disagreeing by however long the window took to persist.
		At: stored.RetrievedAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeTransactionIngested, err)
	}
	return a.append(ctx, db, TypeTransactionIngested, stored.ID, payload)
}

// Unattributed announces one recorded observation (FR-034).
//
// It says only which report and when it was noticed, which is the whole of
// the contract's payload for this type. What an operator needs to act - the
// money, the network's own identifier, whether it may be attributed at all -
// is read from the queue, not carried here: an event that carried it would
// be a second copy of facts that can change underneath it.
func (a *Announcer) Unattributed(ctx context.Context, db events.RowQuerier, queued Queued) error {
	if queued.ReportID == uuid.Nil {
		return fmt.Errorf("%w: %s about a report the database did not store", ErrNotAnnounced, TypeTransactionUnattributed)
	}
	payload, err := json.Marshal(struct {
		NetworkTransactionID uuid.UUID `json:"network_transaction_id"`
		At                   time.Time `json:"at"`
	}{
		NetworkTransactionID: queued.ReportID,
		// The detection instant the queue row carries, for the same reason.
		At: queued.DetectedAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeTransactionUnattributed, err)
	}
	return a.append(ctx, db, TypeTransactionUnattributed, queued.ReportID, payload)
}

// append writes one event, keyed so that a redelivery downstream is a no-op.
//
// The key carries the TYPE as well as the report, and that is not
// decoration: the outbox's unique index is on the key alone, so two
// different facts about one report keyed only by its id would collide - and
// the collision would be silent in the worst way, because a report that went
// unattributed would simply never be announced as such.
//
// An already-appended key is a FAILURE here, not a no-op, and the reason is
// worth stating because the opposite is the tempting reading. The outbox
// reports a collision as a unique violation, and a failed statement aborts
// the whole transaction - so a caller that shrugged it off would carry on
// through a poll whose every later statement raises "current transaction is
// aborted", and would commit nothing while reporting success. It is the same
// hazard the evidence insert avoids by swallowing its conflict in SQL rather
// than in Go; the outbox does not offer that, so the honest answer is to
// fail.
//
// It should never fire. One event of each type is announced per row the
// database has just stored, and an unchanged re-report stores no row - so a
// collision means two announcements of one fact in one transaction, which is
// a defect in the caller and not a state to recover from.
func (a *Announcer) append(ctx context.Context, db events.RowQuerier, eventType string, report uuid.UUID, payload json.RawMessage) error {
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:           eventType,
		Subject:        report,
		IdempotencyKey: eventType + ":" + report.String(),
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about report %s: %w", ErrNotAnnounced, eventType, report, err)
	}
	return nil
}
