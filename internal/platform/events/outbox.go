// Package events is the transactional outbox and dispatcher over the
// append-only public.domain_event stream - the only channel through which
// product domains communicate (constitution, "Products"; ADR-0001;
// specs/002-apivo-cashback-alpha/contracts/events.md).
//
// A Writer appends one event inside the transaction that carries the state
// change the event describes. That placement is the whole guarantee: the
// two commit together or neither exists, so there is no code path that
// publishes an event without its state change, or commits a state change
// without its event.
//
// A Dispatcher polls the stream after a checkpoint and fans each event out
// to the handlers registered for its type, preserving order per
// (type, subject) lane - the only ordering the stream promises - and
// halting a lane loudly when a delivery spends its retry budget, so a
// failure is always visible and an event is never silently dropped.
//
// Delivery is at-least-once. The dispatcher's own tracking is deliberately
// thin: the checkpoint is its only persisted state, a restart redelivers
// anything the checkpoint has not passed, and every handler must be
// idempotent (consumer rule 1 of the event contract). Durable per-delivery
// tracking - a delivery idempotency key per subscriber, and the
// dead-letter parking of deliveries that keep failing - is provided by the
// Registry, which builds on the Handler and CheckpointStore seams the
// dispatcher defines.
//
// # What the checkpoint guarantees, and what it does not
//
// The stream is read along occurred_at, which domain_event stamps with
// now() - the appending transaction's START time - while a row can only
// be read once that transaction has COMMITTED. Read order and commit
// order are therefore not the same order, and an event appended early and
// committed late turns up behind events appended later and committed
// sooner. A checkpoint that ignored that would move over the later ones
// and never look at the earlier one's place again.
//
// So a checkpoint is only ever saved at a position that no append still
// in flight can land in front of. Two bounds hold it there, both asked of
// the database on every poll (see pollStream in dispatcher.go): the start
// time of the oldest transaction open in this database, which is the
// earliest occurred_at any event still to appear can carry; and the
// transaction-id horizon of the poll's own snapshot, which says of each
// row read that the transaction that wrote it had already finished. Both
// delivery layers save a position, so both are read through that one poll
// and both obey its verdict - the Dispatcher for its in-process
// checkpoint, the Registry for each subscriber's durable one.
//
// What that makes impossible: an event appended by a transaction the poll
// can see cannot end up behind a checkpoint, however long that
// transaction stays open. The next poll reads its place again, finds it
// and delivers it. What it costs is redelivery, never loss - rows the
// checkpoint has not passed are read again on every tick - which is the
// handler's side of the bargain and the reason idempotence is not
// optional. It also costs delay: while a transaction sits open the
// checkpoint stands still behind it, and delivery runs no further ahead
// of the checkpoint than one batch.
//
// Two limits are worth stating rather than papering over. The first bound
// sees a session's transaction only if the reading role is allowed to -
// sessions of its own role, or all of them given pg_read_all_stats - so a
// producer connecting as some other role is held only by the second
// bound, which covers it from the moment it has written anything, an
// append included, and not before. The second limit is the prepared
// transaction, which has no session left to be seen at all and is held by
// the transaction-id horizon alone.
//
// Recording deliveries does not close either one. The Registry's delivery
// table says which events a subscriber has already handled, which is what
// makes a redelivery a recorded no-op and a replay free of side effects,
// but it is not how the Registry finds work: that is still the same
// position read, the same (occurred_at, id) tuple compared against a
// stored checkpoint. Closing the two limits structurally would mean
// asking which events have no delivery row instead - an anti-join no
// timestamp can outrun - and the Registry does not ask that. It advances
// by position, so for the durable path exactly as for the in-process one,
// these two bounds are what stand between a slow producer and a lost
// event.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// minTypeSegments is the fewest dot-separated segments an event type may
// have: <producer>.<entity>.<past-tense-verb>. A type may refine the
// entity with further segments; it may not have fewer.
const minTypeSegments = 3

// envelopeFieldNames are the field names of the envelope itself, as the
// event contract spells them. Each lives in its own domain_event column
// and never inside payload; a producer that writes one at the payload's
// top level is a defect, and Append refuses it (contracts/events.md,
// "Where each envelope field lives").
var envelopeFieldNames = []string{
	"event_id", "type", "version", "occurred_at",
	"producer", "subject", "idempotency_key", "payload",
}

// ErrAlreadyAppended reports an append whose (producer, idempotency key)
// pair is already in the stream: the work this event describes committed
// on an earlier attempt. The unique violation that detects it has aborted
// the caller's transaction, so the caller rolls everything back - the
// correct outcome for a retry of work that already happened (migration
// 0018's partial unique index; contracts/events.md, at-least-once).
var ErrAlreadyAppended = errors.New("events: an event with this idempotency key is already in the stream")

// Event is one domain event as consumers receive it: the full envelope of
// contracts/events.md, one field per domain_event column, with the
// payload left raw for the consumer that knows its schema.
type Event struct {
	// EventID is the stream identity of the event (domain_event.id). It
	// is what an idempotent handler records to make redelivery a no-op.
	EventID uuid.UUID
	// Type names the fact that happened, as
	// <producer>.<entity>.<past-tense-verb> - always past tense: an event
	// is a fact, never a request.
	Type string
	// Version is the payload schema version for this type. Consumers read
	// it before the payload, so a producer can change shape without a
	// coordinated deploy.
	Version int
	// OccurredAt is when the event was appended - the stream's time axis,
	// and the order the dispatcher reads it in.
	OccurredAt time.Time
	// Producer is the product domain that emitted the event.
	Producer string
	// Subject is the entity the event is about; uuid.Nil when it has
	// none. Per-subject ordering within a type is the only ordering the
	// stream guarantees.
	Subject uuid.UUID
	// IdempotencyKey is the producer-chosen key that made appending this
	// event a one-time thing; "" when the producer supplied none.
	IdempotencyKey string
	// Payload is the event's own data: a JSON object that never carries
	// an envelope field at its top level.
	Payload json.RawMessage
}

// RowQuerier is the narrow slice of database access Append needs: running
// one insert and reading the row it returns. It is defined here per the
// boundary rules (the consumer names its dependency); pgx.Tx satisfies
// it, and so does *pgxpool.Pool.
//
// Which value a producer passes is the outbox contract itself: the event
// is appended in the same transaction as the state change it describes,
// so a producer passes the pgx.Tx that change is in. Passing a pool
// appends in an implicit single-statement transaction, which is only
// correct for an event that describes no other state change.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Message is one event as its producer supplies it: the caller-chosen
// half of the envelope. The database assigns the rest on append - the id
// and occurred_at - and the Writer stamps the producer it was built for.
type Message struct {
	// Type names the fact that happened, as
	// <producer>.<entity>.<past-tense-verb>, its first segment naming the
	// Writer's producer.
	Type string
	// Version is the payload schema version; zero means 1, the first
	// schema any type has. It is bumped on any breaking payload change.
	Version int
	// Subject is the entity the event is about; uuid.Nil means it has
	// none. Events that share a subject within a type are delivered in
	// stream order; events that do not are ordered by nothing.
	Subject uuid.UUID
	// IdempotencyKey, when set, makes appending this event a second time
	// fail as ErrAlreadyAppended instead of duplicating it. "" means the
	// event has no delivery to deduplicate; a key that is present must
	// not be blank.
	IdempotencyKey string
	// Payload is the event's own data: a JSON object carrying none of
	// the envelope's field names at its top level. An event with nothing
	// to say writes {} explicitly.
	Payload json.RawMessage
}

// Writer appends events for one producer. Building one per product domain
// - NewWriter("cashback") in that domain's wiring - is what lets Append
// check at the boundary that every type published through it names the
// domain it came from.
type Writer struct {
	producer string
}

// NewWriter builds a Writer that appends as producer. The producer is the
// first segment of every type the writer will accept, so it must be
// non-blank and contain neither dots nor whitespace.
func NewWriter(producer string) (*Writer, error) {
	switch {
	case strings.TrimSpace(producer) == "":
		return nil, errors.New("events: a writer needs a producer")
	case strings.Contains(producer, "."):
		return nil, fmt.Errorf("events: producer %q must not contain a dot: it is the first dot-separated segment of every type", producer)
	case strings.ContainsFunc(producer, unicode.IsSpace):
		return nil, fmt.Errorf("events: producer %q must not contain whitespace", producer)
	}
	return &Writer{producer: producer}, nil
}

// Append writes one event into the outbox through db - the transaction
// that carries the state change the event describes. Both commit together
// or neither exists; that placement, not this method, is the atomicity
// guarantee, so the caller owns the transaction and its rollback.
//
// The message is validated before any database work, so a defective
// producer is reported by name rather than as a constraint violation deep
// inside the insert. A valid message is stored with the database
// assigning id and occurred_at, and the completed envelope is returned;
// its Payload is the message's bytes, whose stored jsonb rendering may be
// normalised without changing their content. An idempotency-key collision
// is reported as ErrAlreadyAppended, and has aborted the transaction.
func (w *Writer) Append(ctx context.Context, db RowQuerier, msg Message) (Event, error) {
	if err := w.validate(msg); err != nil {
		return Event{}, fmt.Errorf("events: append: %w", err)
	}
	version := msg.Version
	if version == 0 {
		version = 1
	}
	var (
		id         string
		occurredAt time.Time
	)
	err := db.QueryRow(ctx,
		`insert into domain_event (type, payload, version, producer, subject, idempotency_key)
		 values ($1, $2::jsonb, $3, $4, $5, $6)
		 returning id, occurred_at`,
		msg.Type, string(msg.Payload), version, w.producer,
		nilIfNilUUID(msg.Subject), nilIfEmpty(msg.IdempotencyKey),
	).Scan(&id, &occurredAt)
	if err != nil {
		if isIdempotencyCollision(err) {
			return Event{}, fmt.Errorf("events: append %s: %w", msg.Type, ErrAlreadyAppended)
		}
		return Event{}, fmt.Errorf("events: append %s: %w", msg.Type, err)
	}
	eventID, err := uuid.Parse(id)
	if err != nil {
		// The appended row is fine; only the reported envelope is unusable.
		return Event{}, fmt.Errorf("events: append %s: database returned malformed id %q: %w", msg.Type, id, err)
	}
	return Event{
		EventID:        eventID,
		Type:           msg.Type,
		Version:        version,
		OccurredAt:     occurredAt,
		Producer:       w.producer,
		Subject:        msg.Subject,
		IdempotencyKey: msg.IdempotencyKey,
		Payload:        msg.Payload,
	}, nil
}

// validate checks the whole message at the boundary, so every defect is
// reported in the producer's terms before the stream is touched.
func (w *Writer) validate(msg Message) error {
	if err := validateType(msg.Type, w.producer); err != nil {
		return err
	}
	if msg.Version < 0 {
		return fmt.Errorf("type %s: version must not be negative, got %d", msg.Type, msg.Version)
	}
	if msg.IdempotencyKey != "" && strings.TrimSpace(msg.IdempotencyKey) == "" {
		return fmt.Errorf("type %s: an idempotency key, when present, must not be blank", msg.Type)
	}
	if err := validatePayload(msg.Payload); err != nil {
		return fmt.Errorf("type %s: %w", msg.Type, err)
	}
	return nil
}

// validateType checks the shape the event contract mandates: at least
// three dot-separated segments - <producer>.<entity>.<past-tense-verb> -
// none of them empty, no whitespace anywhere, and the first segment
// naming the producer this writer appends as.
func validateType(eventType, producer string) error {
	if strings.ContainsFunc(eventType, unicode.IsSpace) {
		return fmt.Errorf("type %q must not contain whitespace", eventType)
	}
	segments := strings.Split(eventType, ".")
	if len(segments) < minTypeSegments {
		return fmt.Errorf("type %q has %d dot-separated segment(s); the contract shape is <producer>.<entity>.<past-tense-verb>, at least %d",
			eventType, len(segments), minTypeSegments)
	}
	for i, segment := range segments {
		if segment == "" {
			return fmt.Errorf("type %q: segment %d is empty", eventType, i+1)
		}
	}
	if segments[0] != producer {
		return fmt.Errorf("type %q names producer %q, but this writer appends as %q; a domain publishes only its own facts",
			eventType, segments[0], producer)
	}
	return nil
}

// validatePayload checks that the payload is a JSON object carrying none
// of the envelope's field names at its top level. The envelope columns
// are the single home of routing data; a payload duplicating one would
// let two copies of the same fact drift apart, so a producer writing one
// there is a defect and the append is refused (contracts/events.md).
func validatePayload(payload json.RawMessage) error {
	if len(payload) == 0 {
		return errors.New("payload is missing; an event with nothing to say writes {}")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return fmt.Errorf("payload must be a JSON object: %w", err)
	}
	if top == nil {
		return errors.New("payload is JSON null, not an object; an event with nothing to say writes {}")
	}
	for _, field := range envelopeFieldNames {
		if _, defect := top[field]; defect {
			return fmt.Errorf("payload carries envelope field %q at its top level; envelope fields live in their own columns, never inside payload", field)
		}
	}
	return nil
}

// nilIfNilUUID maps an absent subject to NULL: what the producer did not
// supply is stored as nothing, never as a zero value that looks like data.
func nilIfNilUUID(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

// nilIfEmpty maps an absent idempotency key to NULL, keeping the row out
// of the partial unique index: a writer with no delivery to deduplicate
// is simply not in it.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isIdempotencyCollision recognises the unique violation on
// (producer, idempotency_key): this producer already appended this event.
func isIdempotencyCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "domain_event_producer_idempotency_key_unique"
}
