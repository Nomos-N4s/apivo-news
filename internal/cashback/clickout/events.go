// The one fact this module publishes: that a click was recorded (T076).
//
// It is appended to the outbox in the transaction that inserted the click,
// which is the whole of the contract's atomicity guarantee - "there is no
// code path that publishes an event without its state change, or commits a
// state change without its event". Nothing here opens that transaction and
// nothing here retries: the caller owns both.
//
// A click is the evidence every later credit rests on (C-2), so the order it
// is announced in is the order money is eventually decided in. That is why
// the subject is the click and not the member: per-subject ordering is the
// only ordering the stream guarantees, and what a consumer needs in order is
// what happened to one click.

package clickout

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
	// EventProducer is the domain this fact belongs to, and the first
	// segment of the type below - the writer refuses a type naming another
	// producer, because a domain publishes only its own facts.
	EventProducer = "cashback"
	// TypeClickCreated announces one recorded click: who clicked, what they
	// clicked, and when.
	TypeClickCreated = EventProducer + ".click.created"
)

// ErrNotAnnounced reports an event that could not be appended beside the
// click it describes.
//
// Always fatal to the caller that raised it. The append shares the
// transaction the click was inserted in, so a swallowed failure would either
// commit a click with no event or - because the outbox reports a collision
// as a unique violation, which aborts the transaction - commit nothing at
// all while reporting a redirect the member is about to follow.
var ErrNotAnnounced = errors.New("clickout: the event could not be appended beside the click")

// Announcer appends this module's fact to the outbox.
//
// It holds no database handle. Its method takes the transaction that carries
// the insert, because the placement of the append - not anything this type
// does - is what makes the event and the click atomic.
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

// Created announces one click the database has just inserted.
//
// The payload carries identifiers and the instant, and nothing else.
// Consumer rule 5 is explicit that events carry identifiers and the data
// stays in its owning schema - so no click reference, which is the secret
// the redirect is built from, and no context digest, which exists to be read
// by the abuse rules and by nothing else.
//
// Keyed on the click, because a click is recorded once: click_ref is unique,
// so a second insert of one click is already impossible, and a second append
// under this key would mean two announcements in one transaction.
func (a *Announcer) Created(ctx context.Context, db events.RowQuerier, click Click) error {
	if click.ID == uuid.Nil {
		return fmt.Errorf("%w: %s about a click the database did not insert", ErrNotAnnounced, TypeClickCreated)
	}
	payload, err := json.Marshal(struct {
		ClickID   uuid.UUID `json:"click_id"`
		AccountID uuid.UUID `json:"account_id"`
		OfferID   uuid.UUID `json:"offer_id"`
		At        time.Time `json:"at"`
	}{
		ClickID:   click.ID,
		AccountID: click.AccountID,
		OfferID:   click.OfferID,
		// The instant the ROW carries, read back rather than taken from the
		// clock, so the event and the evidence name one moment. A consumer
		// joining the two on time would otherwise find them disagreeing by
		// however long the transaction stayed open.
		At: click.ClickedAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeClickCreated, err)
	}
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:           TypeClickCreated,
		Subject:        click.ID,
		IdempotencyKey: TypeClickCreated + ":" + click.ID.String(),
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about click %s: %w", ErrNotAnnounced, TypeClickCreated, click.ID, err)
	}
	return nil
}
