package events

// The dead-letter half of the durable subscriber layer: what happens when
// a delivery spends its retry budget, and how an operator gets the lane
// moving again.
//
// # Parking, and what it holds back
//
// The Dispatcher answers a poisoned delivery with a halted lane that a
// restart retries. Made durable, that posture would hammer a permanently
// failing handler on every restart forever, so the Registry refines it:
// the delivery is PARKED - written to event_dead_letter with its attempts
// and last error - and OnDeadLetter, Tick's error and the operator queue
// over the table (ListDeadLetters) make the failure visible, exactly as
// consumer rule 4 requires. A parked delivery is not retried again until
// an operator asks.
//
// What parking must not do is reorder a subject's history. Delivering
// event N+1 while event N of the same (type, subject) lane sits parked
// would hand the subscriber the subject's facts out of order, so a parked
// delivery keeps blocking ITS OWN lane for its subscriber: later events
// on that lane are held - preserved by the subscriber's pinned
// checkpoint, never attempted - while every other lane keeps flowing, up
// to the batch bound (see Registry). Only the parked head gets a
// dead-letter row; the events held behind it need none, because the
// checkpoint cannot pass them.
//
// # Requeue
//
// Redelivery becomes possible when an operator requeues the parked row.
// Requeue only marks it: the subscriber's own next Tick performs the
// redelivery, before its main poll and in stream order, so delivery stays
// single-threaded under the scheduler's fleet-wide lock instead of
// running on whatever connection the operator called from. A requeued
// delivery gets a fresh retry budget; spending it parks the row again
// with the attempts accumulated. The HTTP surface for the operator queue
// is a later task; the primitives live here.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotParked reports a requeue that named no parked delivery: never
// parked, already resolved, or the wrong subscriber for the event.
var ErrNotParked = errors.New("events: no parked delivery for this subscriber and event")

// DeadLetter is one parked delivery, as OnDeadLetter reports it and
// ListDeadLetters returns it: which subscriber failed on which event, how
// hard it was tried, and what the last attempt said.
type DeadLetter struct {
	// Subscriber is the subscriber whose delivery is parked. The same
	// event may be parked for one subscriber and long delivered for
	// another.
	Subscriber string
	// Event is the full envelope, read back from the stream the row
	// references.
	Event Event
	// Attempts is the total handler attempts so far, every failed
	// requeue included.
	Attempts int
	// LastError is what the final attempt reported.
	LastError string
	// ParkedAt is when the delivery (last) entered the queue.
	ParkedAt time.Time
	// RequeuedAt is when an operator asked for redelivery; zero while the
	// row waits for one. It is always zero in the OnDeadLetter callback:
	// a row is parked precisely by ceasing to be requeued.
	RequeuedAt time.Time
}

// ListDeadLetters reads the operator queue: every unresolved dead letter
// for the named subscriber, oldest parking first, or for every subscriber
// when the name is empty. Requeued rows are included - they are marked,
// not resolved - and recognisable by a non-zero RequeuedAt.
func ListDeadLetters(ctx context.Context, db Querier, subscriber string) ([]DeadLetter, error) {
	rows, err := db.Query(ctx,
		`select dl.subscriber, dl.attempts, dl.last_error, dl.parked_at, dl.requeued_at,
		        e.id::text, e.type, e.version, e.occurred_at, e.producer, e.subject::text, e.idempotency_key, e.payload
		 from event_dead_letter dl
		 join domain_event e on e.id = dl.event_id
		 where $1 = '' or dl.subscriber = $1
		 order by dl.parked_at, dl.subscriber, e.id`,
		subscriber)
	if err != nil {
		return nil, fmt.Errorf("events: list dead letters: %w", err)
	}
	defer rows.Close()

	var letters []DeadLetter
	for rows.Next() {
		var (
			dl                      DeadLetter
			requeuedAt              *time.Time
			id, eventType, producer string
			version                 int
			occurredAt              time.Time
			subject, key            *string
			payload                 []byte
		)
		if err := rows.Scan(&dl.Subscriber, &dl.Attempts, &dl.LastError, &dl.ParkedAt, &requeuedAt,
			&id, &eventType, &version, &occurredAt, &producer, &subject, &key, &payload); err != nil {
			return nil, fmt.Errorf("events: scan dead letter: %w", err)
		}
		if requeuedAt != nil {
			dl.RequeuedAt = *requeuedAt
		}
		if dl.Event, err = buildEvent(id, eventType, version, occurredAt, producer, subject, key, payload); err != nil {
			return nil, fmt.Errorf("events: dead letter: %w", err)
		}
		letters = append(letters, dl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: read dead letters: %w", err)
	}
	return letters, nil
}

// Requeue marks one parked delivery for redelivery: the subscriber's next
// Tick re-attempts it - with a fresh retry budget, ahead of everything
// held behind it on its lane - and either resolves the row or parks it
// again. Requeuing a row that is already marked refreshes the mark, so an
// operator retrying the call does no harm; naming a delivery that is not
// in the queue fails with ErrNotParked.
func Requeue(ctx context.Context, db Execer, subscriber string, eventID uuid.UUID) error {
	tag, err := db.Exec(ctx,
		`update event_dead_letter set requeued_at = now() where subscriber = $1 and event_id = $2`,
		subscriber, eventID.String())
	if err != nil {
		return fmt.Errorf("events: requeue event %s for %q: %w", eventID, subscriber, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("events: requeue event %s for %q: %w", eventID, subscriber, ErrNotParked)
	}
	return nil
}

// requeuedDelivery is one dead letter an operator marked for redelivery:
// the event to attempt again, and the attempts already spent on it.
type requeuedDelivery struct {
	event    Event
	attempts int
}

// requeuedDeadLetters reads the subscriber's requeued rows in stream
// order - the order their redeliveries must run in, since each is the
// head of its lane.
func requeuedDeadLetters(ctx context.Context, db Querier, subscriber string) ([]requeuedDelivery, error) {
	rows, err := db.Query(ctx,
		`select e.id::text, e.type, e.version, e.occurred_at, e.producer, e.subject::text, e.idempotency_key, e.payload, dl.attempts
		 from event_dead_letter dl
		 join domain_event e on e.id = dl.event_id
		 where dl.subscriber = $1 and dl.requeued_at is not null
		 order by e.occurred_at, e.id`,
		subscriber)
	if err != nil {
		return nil, fmt.Errorf("read requeued dead letters: %w", err)
	}
	defer rows.Close()

	var requeued []requeuedDelivery
	for rows.Next() {
		var (
			id, eventType, producer string
			version, attempts       int
			occurredAt              time.Time
			subject, key            *string
			payload                 []byte
		)
		if err := rows.Scan(&id, &eventType, &version, &occurredAt, &producer, &subject, &key, &payload, &attempts); err != nil {
			return nil, fmt.Errorf("scan requeued dead letter: %w", err)
		}
		e, err := buildEvent(id, eventType, version, occurredAt, producer, subject, key, payload)
		if err != nil {
			return nil, fmt.Errorf("requeued dead letter: %w", err)
		}
		requeued = append(requeued, requeuedDelivery{event: e, attempts: attempts})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read requeued dead letters: %w", err)
	}
	return requeued, nil
}

// blockedLanes reads the lanes a subscriber may not deliver on: one per
// parked, un-requeued dead letter, which the database keeps to at most
// one per lane.
func blockedLanes(ctx context.Context, db Querier, subscriber string) (map[Lane]bool, error) {
	rows, err := db.Query(ctx,
		`select event_type, subject::text from event_dead_letter where subscriber = $1 and requeued_at is null`,
		subscriber)
	if err != nil {
		return nil, fmt.Errorf("read blocked lanes: %w", err)
	}
	defer rows.Close()

	lanes := make(map[Lane]bool)
	for rows.Next() {
		var (
			eventType string
			subject   *string
		)
		if err := rows.Scan(&eventType, &subject); err != nil {
			return nil, fmt.Errorf("scan blocked lane: %w", err)
		}
		lane := Lane{Type: eventType}
		if subject != nil {
			if lane.Subject, err = uuid.Parse(*subject); err != nil {
				return nil, fmt.Errorf("blocked lane on %s: subject %q: %w", eventType, *subject, err)
			}
		}
		lanes[lane] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read blocked lanes: %w", err)
	}
	return lanes, nil
}

// deliveryRecorded reports whether the completion row for one delivery
// exists - the delivery idempotency key check that turns a redelivery
// into a recorded no-op before the handler runs.
func deliveryRecorded(ctx context.Context, db RowQuerier, subscriber string, eventID uuid.UUID) (bool, error) {
	var recorded bool
	err := db.QueryRow(ctx,
		`select exists (select 1 from event_delivery where subscriber = $1 and event_id = $2)`,
		subscriber, eventID.String()).Scan(&recorded)
	if err != nil {
		return false, fmt.Errorf("check the delivery record: %w", err)
	}
	return recorded, nil
}

// recordDelivery writes the completion row for one delivery. The conflict
// clause makes losing a race to an earlier recording a success: the row
// says delivered either way, and the immutable table refuses anything
// beyond saying it once.
func recordDelivery(ctx context.Context, db Execer, subscriber string, eventID uuid.UUID, attempts int) error {
	_, err := db.Exec(ctx,
		`insert into event_delivery (subscriber, event_id, attempts)
		 values ($1, $2, $3)
		 on conflict (subscriber, event_id) do nothing`,
		subscriber, eventID.String(), attempts)
	if err != nil {
		return fmt.Errorf("record the delivery: %w", err)
	}
	return nil
}

// parkDeadLetter writes the dead-letter row for a delivery that spent its
// retry budget, returning the parking time the database recorded.
func parkDeadLetter(ctx context.Context, db RowQuerier, subscriber string, e Event, attempts int, cause error) (time.Time, error) {
	var parkedAt time.Time
	err := db.QueryRow(ctx,
		`insert into event_dead_letter (subscriber, event_id, event_type, subject, occurred_at, attempts, last_error)
		 values ($1, $2, $3, $4, $5, $6, $7)
		 returning parked_at`,
		subscriber, e.EventID.String(), e.Type, nilIfNilUUID(e.Subject), e.OccurredAt, attempts, errorText(cause),
	).Scan(&parkedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("park the delivery: %w", err)
	}
	return parkedAt, nil
}

// reparkDeadLetter returns a requeued row to the parked state after its
// redelivery failed too, folding the new attempts into the total.
func reparkDeadLetter(ctx context.Context, db RowQuerier, subscriber string, eventID uuid.UUID, attempts int, cause error) (time.Time, error) {
	var parkedAt time.Time
	err := db.QueryRow(ctx,
		`update event_dead_letter
		 set attempts = attempts + $3, last_error = $4, parked_at = now(), requeued_at = null
		 where subscriber = $1 and event_id = $2
		 returning parked_at`,
		subscriber, eventID.String(), attempts, errorText(cause),
	).Scan(&parkedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("park the delivery again: %w", err)
	}
	return parkedAt, nil
}

// resolveDeadLetter removes one row from the queue once its delivery
// completed. Deletion is the design, not a shortcut: the table is the
// open operator queue, the durable outcome lives in event_delivery, and
// the audit stream stays domain_event.
func resolveDeadLetter(ctx context.Context, db Execer, subscriber string, eventID uuid.UUID) error {
	if _, err := db.Exec(ctx,
		`delete from event_dead_letter where subscriber = $1 and event_id = $2`,
		subscriber, eventID.String()); err != nil {
		return fmt.Errorf("clear the dead letter: %w", err)
	}
	return nil
}

// recordedDeliveries reports which of the given events already have a
// completion row for this subscriber.
func recordedDeliveries(ctx context.Context, db Querier, subscriber string, ids []string) (map[uuid.UUID]bool, error) {
	return recordedIDs(ctx, db,
		`select event_id::text from event_delivery where subscriber = $1 and event_id = any($2::uuid[])`,
		subscriber, ids)
}

// recordedDeadLetters reports which of the given events sit in the
// dead-letter table for this subscriber, requeued or not.
func recordedDeadLetters(ctx context.Context, db Querier, subscriber string, ids []string) (map[uuid.UUID]bool, error) {
	return recordedIDs(ctx, db,
		`select event_id::text from event_dead_letter where subscriber = $1 and event_id = any($2::uuid[])`,
		subscriber, ids)
}

// recordedIDs runs one membership query over a batch of event ids - one
// round trip per batch instead of one per event.
func recordedIDs(ctx context.Context, db Querier, query, subscriber string, ids []string) (map[uuid.UUID]bool, error) {
	rows, err := db.Query(ctx, query, subscriber, ids)
	if err != nil {
		return nil, fmt.Errorf("read delivery state: %w", err)
	}
	defer rows.Close()

	recorded := make(map[uuid.UUID]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan delivery state: %w", err)
		}
		eventID, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("delivery state holds malformed event id %q: %w", id, err)
		}
		recorded[eventID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read delivery state: %w", err)
	}
	return recorded, nil
}

// errorText renders a delivery failure for the last_error column, which
// the schema refuses to leave blank: an error that stringifies to nothing
// still has to tell the operator something.
func errorText(err error) string {
	if text := strings.TrimSpace(err.Error()); text != "" {
		return text
	}
	return "(the handler reported an error with no message)"
}
