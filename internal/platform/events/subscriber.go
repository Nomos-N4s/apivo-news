package events

// The durable half of delivery: named subscribers whose progress,
// completed deliveries and unresolved failures all live in Postgres
// (migration 0021), so a restart resumes instead of replaying the stream
// through every handler, and a failure outlives the process that saw it.
//
// The Registry keeps the Dispatcher's model - the (type, subject) lane is
// the unit of both ordering and failure isolation - and swaps its
// in-memory failure posture for the durable one the event contract
// demands. Three tables carry the state:
//
//   subscriber_checkpoint  where each subscriber has read to. Positions
//                          are per subscriber on purpose: one slow or
//                          blocked subscriber never stalls another. A
//                          position claims that nothing at or before it
//                          will ever need reading again, and a durable
//                          one makes that claim past every restart, so it
//                          is held to the same settledness rule as the
//                          dispatcher's (see drainStream).
//   event_delivery         the delivery idempotency key: one row per
//                          (subscriber, event) that completed, so a
//                          redelivery is a recorded no-op.
//   event_dead_letter      deliveries that spent their retry budget,
//                          parked for an operator (deadletter.go).
//
// # The honest exactly-once boundary
//
// The completion row is written after the handler returns, as its own
// statement. A crash between the two - handler succeeded, tracking write
// lost - therefore redelivers the event, and nothing in this package can
// close that window: the handler's side effects and the completion row
// live in different transactions unless the handler itself joins them.
// That is why consumer rule 1 still binds every handler to be idempotent
// on EventID. What the delivery key adds is that the routine redeliveries
// - a lost checkpoint, a replay, a restart mid-batch - are absorbed here,
// before the handler runs at all, instead of by every handler alone.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is the narrow slice of database access a plain write needs:
// running one statement. pgx.Tx satisfies it, and so does *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DB is the database access the durable subscriber layer needs: polling
// the stream, reading its own tracking tables, and writing checkpoints,
// completion rows and dead letters. *pgxpool.Pool satisfies it, and so
// does pgx.Tx.
type DB interface {
	Querier
	RowQuerier
	Execer
}

// PostgresCheckpoints is the durable CheckpointStore: one subscriber's
// position in the subscriber_checkpoint table. Losing a position is still
// safe - the subscriber re-reads the stream from the start and the
// delivery table absorbs the replay - but no longer routine: a restart
// resumes where the last save left off.
type PostgresCheckpoints struct {
	db         DB
	subscriber string
}

var _ CheckpointStore = (*PostgresCheckpoints)(nil)

// NewPostgresCheckpoints builds the durable store for one named
// subscriber. The name keys the stored position, so it must be non-blank
// and stable across releases: renaming a subscriber abandons its position
// and its delivery records, and re-reads the stream under the new name.
func NewPostgresCheckpoints(db DB, subscriber string) (*PostgresCheckpoints, error) {
	if strings.TrimSpace(subscriber) == "" {
		return nil, errors.New("events: a durable checkpoint store needs a subscriber name")
	}
	return &PostgresCheckpoints{db: db, subscriber: subscriber}, nil
}

// Load returns the saved position, or the zero Checkpoint - the start of
// the stream - when this subscriber never saved one.
func (p *PostgresCheckpoints) Load(ctx context.Context) (Checkpoint, error) {
	var (
		occurredAt time.Time
		id         string
	)
	err := p.db.QueryRow(ctx,
		`select occurred_at, event_id::text from subscriber_checkpoint where subscriber = $1`,
		p.subscriber,
	).Scan(&occurredAt, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, nil
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("events: load checkpoint for %q: %w", p.subscriber, err)
	}
	eventID, err := uuid.Parse(id)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("events: checkpoint for %q holds malformed event id %q: %w", p.subscriber, id, err)
	}
	return Checkpoint{OccurredAt: occurredAt, EventID: eventID}, nil
}

// Save records cp as the position to resume from, replacing any earlier
// one.
func (p *PostgresCheckpoints) Save(ctx context.Context, cp Checkpoint) error {
	_, err := p.db.Exec(ctx,
		`insert into subscriber_checkpoint (subscriber, occurred_at, event_id)
		 values ($1, $2, $3)
		 on conflict (subscriber) do update
		 set occurred_at = excluded.occurred_at, event_id = excluded.event_id, updated_at = now()`,
		p.subscriber, cp.OccurredAt, cp.EventID.String())
	if err != nil {
		return fmt.Errorf("events: save checkpoint for %q: %w", p.subscriber, err)
	}
	return nil
}

// RegistryConfig tunes the registry. The zero value is usable: every
// field falls back to the same documented default the dispatcher uses.
type RegistryConfig struct {
	// BatchSize is how many events one subscriber reads past its
	// checkpoint per Tick. Zero or negative means DefaultBatchSize. It
	// also bounds how far other lanes can run ahead of a checkpoint
	// pinned by events held behind a parked lane.
	BatchSize int
	// MaxAttempts is one delivery's retry budget - and again the budget
	// of each requeued redelivery. Zero or negative means
	// DefaultMaxAttempts.
	MaxAttempts int
	// RetryDelay is the wait between attempts of one delivery. Zero or
	// negative means DefaultRetryDelay; retrying with no delay at all is
	// deliberately not offered, because the failure being retried is
	// usually still happening.
	RetryDelay time.Duration
	// OnDeadLetter is called from inside Tick at the moment a delivery is
	// parked - on first parking and again each time a requeued
	// redelivery fails its way back into the queue. It must not call back
	// into the registry. Nil means parking is reported only through
	// Tick's error and the dead-letter table itself.
	OnDeadLetter func(DeadLetter)
}

// withDefaults returns cfg with every unset or out-of-range field
// replaced by its default.
func (c RegistryConfig) withDefaults() RegistryConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = DefaultRetryDelay
	}
	return c
}

// Registry delivers the stream durably to named subscribers. Construct it
// with NewRegistry, add subscribers with Subscribe, then drive it by
// calling Tick on a schedule - it is shaped to be a scheduler job's Run,
// exactly as the Dispatcher is, and the advisory lock there is what keeps
// two instances from delivering at once.
//
// Ordering: within each (type, subject) lane a subscriber sees events in
// stream order; across lanes and across subscribers nothing is ordered
// and nothing is coupled. Each subscriber has its own checkpoint, its own
// delivery records and its own dead letters, so one subscriber's failure
// or slowness never holds another's delivery back. Stream order is the
// order of the rows a poll can read, exactly as it is for the Dispatcher:
// an event whose transaction commits after its lane-mates were already
// delivered stands earlier in the stream than they do and is still
// delivered late, because the only alternative to a late delivery is no
// delivery at all.
//
// No skips: a subscriber's checkpoint is durable, so a position saved too
// far forward drops an event for good rather than for one process
// lifetime. It is never saved over a row that an append still in flight
// could land in front of - the same rule, read from the same poll, that
// holds the dispatcher's checkpoint back (see drainStream, and pollStream
// in dispatcher.go).
//
// Failure: a delivery that spends its retry budget is parked in the
// dead-letter table and its lane blocks for that subscriber until an
// operator requeues it - the durable refinement of the Dispatcher's
// halt, spelled out in deadletter.go.
type Registry struct {
	db  DB
	cfg RegistryConfig

	mu          sync.Mutex
	subscribers []*subscriber
	names       map[string]bool
	started     bool
}

// subscriber is one registered consumer: its name, the set of types it
// handles, its handler and its durable position.
type subscriber struct {
	name        string
	types       map[string]bool
	handler     Handler
	checkpoints *PostgresCheckpoints
}

// NewRegistry builds a registry that reads events and its own tracking
// state from db. Subscribers are added separately, before the first Tick.
func NewRegistry(db DB, cfg RegistryConfig) *Registry {
	return &Registry{
		db:    db,
		cfg:   cfg.withDefaults(),
		names: make(map[string]bool),
	}
}

// Subscribe adds a named subscriber: handler receives every event whose
// type is in types, in per-lane stream order, tracked durably under name.
// The name keys the subscriber's checkpoint, delivery records and dead
// letters, so it must be unique here and stable across releases. Two
// subscribers may share an event type - each receives and records its
// deliveries independently. Registration closes at the first Tick; a late
// call fails with ErrStarted, mirroring the dispatcher's lifecycle.
func (r *Registry) Subscribe(name string, types []string, handler Handler) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("events: subscribe: a subscriber needs a name")
	}
	if handler == nil {
		return fmt.Errorf("events: subscribe %s: handler must not be nil", name)
	}
	if len(types) == 0 {
		return fmt.Errorf("events: subscribe %s: a subscriber needs at least one event type", name)
	}
	typeSet := make(map[string]bool, len(types))
	for _, eventType := range types {
		if strings.TrimSpace(eventType) == "" {
			return fmt.Errorf("events: subscribe %s: an event type must not be blank", name)
		}
		if typeSet[eventType] {
			return fmt.Errorf("events: subscribe %s: event type %s is listed twice", name, eventType)
		}
		typeSet[eventType] = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("events: subscribe %s: %w", name, ErrStarted)
	}
	if r.names[name] {
		return fmt.Errorf("events: subscribe %s: the name is already taken, and a subscriber's durable state is keyed on it", name)
	}
	r.names[name] = true
	r.subscribers = append(r.subscribers, &subscriber{
		name:    name,
		types:   typeSet,
		handler: handler,
		// The name was just validated non-blank, so this cannot fail the
		// way the exported constructor can.
		checkpoints: &PostgresCheckpoints{db: r.db, subscriber: name},
	})
	return nil
}

// Tick is one delivery pass over every subscriber, in registration order.
// Each subscriber's pass first re-attempts its requeued dead letters - in
// stream order, so a resumed lane replays its head before anything held
// behind it - and then delivers a batch past its durable checkpoint. A
// failing subscriber's faults are collected, never allowed to stop
// another subscriber's pass; a cancelled ctx ends the whole pass early,
// keeping whatever the checkpoints already cover.
//
// The returned error is the tick's loud surface: infrastructure failures,
// every delivery parked during this tick, and - again, on every later
// tick - every parked lane still holding later events back. Unlike the
// dispatcher, a parked delivery with nothing behind it goes quiet here,
// because the dead-letter table and the operator queue over it are the
// durable loud surface consumer rule 4 asks for.
func (r *Registry) Tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = true

	var faults []error
	for _, s := range r.subscribers {
		if ctx.Err() != nil {
			break
		}
		if err := r.tick(ctx, s); err != nil {
			faults = append(faults, err)
		}
	}
	return errors.Join(faults...)
}

// tick is one subscriber's pass. The blocked lanes are read before the
// requeued rows on purpose: an operator requeue landing between the two
// queries is then seen by at least one of them, so a held event can never
// overtake its lane's parked head.
func (r *Registry) tick(ctx context.Context, s *subscriber) error {
	blocked, err := blockedLanes(ctx, r.db, s.name)
	if err != nil {
		return fmt.Errorf("events: subscriber %q: %w", s.name, err)
	}
	faults := r.redeliverRequeued(ctx, s, blocked)
	if ctx.Err() == nil {
		faults = append(faults, r.drainStream(ctx, s, blocked)...)
	}
	return errors.Join(faults...)
}

// redeliverRequeued attempts every dead letter an operator marked for
// redelivery, in stream order, before the main poll: a requeued event is
// by construction the head of its lane, so delivering it first is what
// lets the events held behind it flow again in order. A redelivery gets a
// fresh retry budget; spending it parks the row again with the attempts
// accumulated, and the lane stays blocked. blocked is updated in place
// with each outcome so the main poll that follows sees the lanes as they
// now are.
func (r *Registry) redeliverRequeued(ctx context.Context, s *subscriber, blocked map[Lane]bool) []error {
	requeued, err := requeuedDeadLetters(ctx, r.db, s.name)
	if err != nil {
		return []error{fmt.Errorf("events: subscriber %q: %w", s.name, err)}
	}
	var faults []error
	for _, dl := range requeued {
		if ctx.Err() != nil {
			break
		}
		lane := Lane{Type: dl.event.Type, Subject: dl.event.Subject}
		recorded, err := deliveryRecorded(ctx, r.db, s.name, dl.event.EventID)
		if err != nil {
			// Its true state is unknown, so its lane stays held.
			blocked[lane] = true
			faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s: %w", s.name, dl.event.EventID, err))
			continue
		}
		if recorded {
			// A crash between recording a delivery and clearing its dead
			// letter left only the queue entry behind; clear it now.
			if err := resolveDeadLetter(ctx, r.db, s.name, dl.event.EventID); err != nil {
				faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s: %w", s.name, dl.event.EventID, err))
			}
			delete(blocked, lane)
			continue
		}
		attempts, deliverErr := attemptDelivery(ctx, s.handler, dl.event, r.cfg.MaxAttempts, r.cfg.RetryDelay)
		if deliverErr != nil {
			if ctx.Err() != nil {
				// The failure is the shutdown's, not the delivery's: the
				// row stays requeued for the next tick.
				break
			}
			blocked[lane] = true
			parkedAt, parkErr := reparkDeadLetter(ctx, r.db, s.name, dl.event.EventID, attempts, deliverErr)
			if parkErr != nil {
				faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s failed and could not be parked again: %w",
					s.name, dl.event.EventID, errors.Join(deliverErr, parkErr)))
				continue
			}
			r.reportParked(DeadLetter{
				Subscriber: s.name, Event: dl.event,
				Attempts: dl.attempts + attempts, LastError: errorText(deliverErr), ParkedAt: parkedAt,
			})
			faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s on lane %s failed again after %d more attempt(s); the delivery is parked again: %w",
				s.name, dl.event.EventID, lane, attempts, deliverErr))
			continue
		}
		if err := recordDelivery(ctx, r.db, s.name, dl.event.EventID, dl.attempts+attempts); err != nil {
			// The handler ran; without its completion row the next tick
			// redelivers - the at-least-once window, widened by an
			// infrastructure failure. The lane stays held meanwhile.
			blocked[lane] = true
			faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s was handled but recording the delivery failed; it will be redelivered: %w",
				s.name, dl.event.EventID, err))
			continue
		}
		if err := resolveDeadLetter(ctx, r.db, s.name, dl.event.EventID); err != nil {
			// The completion row is in place, so the recorded check above
			// clears this leftover on the next tick.
			faults = append(faults, fmt.Errorf("events: subscriber %q: requeued event %s was delivered but clearing its dead letter failed: %w",
				s.name, dl.event.EventID, err))
		}
		delete(blocked, lane)
	}
	return faults
}

// drainStream is one subscriber's poll-and-deliver pass past its durable
// checkpoint. The checkpoint advances over the contiguous prefix of
// events that are durably accounted for - delivered, parked, or of a type
// this subscriber does not handle - and pins before the first event held
// behind a parked lane, so a held event is preserved by position rather
// than by a row of its own. Between the pin and the batch bound every
// other lane keeps flowing; past the bound, a parked lane with events
// held behind it back-pressures the whole subscriber. That is deliberate:
// the alternative is unbounded tracking of skipped events, and the
// mounting pressure is what makes an unresolved dead letter impossible to
// leave unresolved.
//
// A second rule pins the checkpoint independently of the tracking tables,
// and the advance takes whichever of the two stops first - the minimum of
// what the tables allow and what the poll allows. pollStream marks every
// row settled or unsettled: an unsettled row is one an append still in
// flight can land in FRONT of, because occurred_at is the appending
// transaction's start time while the row becomes readable only at its
// commit. A checkpoint saved at or past such a row claims its place will
// never be read again, and this checkpoint is durable, so the claim
// outlives the process that made it: the event that lands there
// afterwards is not delivered late, it is never delivered at all. So the
// prefix stops at the first unsettled row whatever the tables say about
// it, exactly as the dispatcher's prefix does.
//
// Composing the two rules needs nothing beyond that minimum, because
// pinning early is always safe here. Whatever the checkpoint has not
// passed is re-read on the next tick and re-judged from the same durable
// rows: a recorded delivery is the no-op it already was, a parked head
// re-blocks its lane, and an event held behind one stays held. Nothing in
// this design needs the checkpoint as far forward as the tables would
// permit - only that it never goes further.
func (r *Registry) drainStream(ctx context.Context, s *subscriber, blocked map[Lane]bool) []error {
	cp, err := s.checkpoints.Load(ctx)
	if err != nil {
		return []error{fmt.Errorf("events: subscriber %q: %w", s.name, err)}
	}
	batch, err := pollStream(ctx, r.db, cp, r.cfg.BatchSize)
	if err != nil {
		return []error{fmt.Errorf("events: subscriber %q: %w", s.name, err)}
	}
	if len(batch) == 0 {
		return nil
	}
	ids := make([]string, len(batch))
	for i, row := range batch {
		ids[i] = row.event.EventID.String()
	}
	delivered, err := recordedDeliveries(ctx, r.db, s.name, ids)
	if err != nil {
		return []error{fmt.Errorf("events: subscriber %q: %w", s.name, err)}
	}
	parked, err := recordedDeadLetters(ctx, r.db, s.name, ids)
	if err != nil {
		return []error{fmt.Errorf("events: subscriber %q: %w", s.name, err)}
	}

	var (
		faults    []error
		watermark = cp
		advanced  bool
		advancing = true
		reported  = make(map[Lane]bool)
	)
	// pass records that e needs nothing more from this pass: the
	// watermark moves over it while the prefix is contiguous. Behind a
	// pin, nothing is remembered in memory - what made the event passable
	// is durable, so the next tick re-reads and re-skips it.
	pass := func(e Event) {
		if advancing {
			watermark, advanced = position(e), true
		}
	}

	for _, row := range batch {
		if ctx.Err() != nil {
			break
		}
		e := row.event
		if !row.settled {
			// The row is committed - we are reading it - but an older
			// transaction is still open, and an open transaction's append
			// lands behind this row in stream order. Saving a durable
			// position here would put that append behind this subscriber's
			// read position forever. The gate comes before the type filter
			// and the tracking tables on purpose: what an unsettled row
			// says is that its POSITION cannot be claimed, whatever the
			// row itself turns out to need.
			advancing = false
		}
		lane := Lane{Type: e.Type, Subject: e.Subject}
		if !s.types[e.Type] {
			pass(e)
			continue
		}
		if delivered[e.EventID] {
			// The delivery idempotency key doing its job: this is the
			// recorded no-op a redelivery becomes.
			pass(e)
			continue
		}
		if parked[e.EventID] {
			// Durably parked: the requeue path owns its redelivery, and
			// the lane holds everything behind it.
			blocked[lane] = true
			pass(e)
			continue
		}
		if blocked[lane] {
			advancing = false
			if !reported[lane] {
				reported[lane] = true
				faults = append(faults, fmt.Errorf(
					"events: subscriber %q: lane %s is parked in the dead-letter table and holds back event %s and everything after it on the lane; an operator requeue resumes it",
					s.name, lane, e.EventID))
			}
			continue
		}
		attempts, deliverErr := attemptDelivery(ctx, s.handler, e, r.cfg.MaxAttempts, r.cfg.RetryDelay)
		if deliverErr != nil {
			if ctx.Err() != nil {
				// The failure is the shutdown's, not the lane's: the event
				// stays ahead of the watermark for the next tick.
				break
			}
			blocked[lane] = true
			reported[lane] = true
			parkedAt, parkErr := parkDeadLetter(ctx, r.db, s.name, e, attempts, deliverErr)
			if parkErr != nil {
				// Recorded nowhere, so the checkpoint pins and the next
				// tick retries the delivery from scratch.
				advancing = false
				faults = append(faults, fmt.Errorf(
					"events: subscriber %q: delivering event %s on lane %s failed after %d attempt(s), and parking it failed too; the checkpoint pins for a retry: %w",
					s.name, e.EventID, lane, attempts, errors.Join(deliverErr, parkErr)))
				continue
			}
			r.reportParked(DeadLetter{
				Subscriber: s.name, Event: e,
				Attempts: attempts, LastError: errorText(deliverErr), ParkedAt: parkedAt,
			})
			faults = append(faults, fmt.Errorf(
				"events: subscriber %q: delivering event %s on lane %s failed after %d attempt(s); the delivery is parked in the dead-letter table: %w",
				s.name, e.EventID, lane, attempts, deliverErr))
			pass(e)
			continue
		}
		if err := recordDelivery(ctx, r.db, s.name, e.EventID, attempts); err != nil {
			// The handler ran; the checkpoint must not pass the event
			// without its completion row, or the redelivery that repairs
			// the record would never come.
			advancing = false
			faults = append(faults, fmt.Errorf(
				"events: subscriber %q: event %s was handled but recording the delivery failed; it will be redelivered: %w",
				s.name, e.EventID, err))
			continue
		}
		pass(e)
	}

	if advanced {
		if err := s.checkpoints.Save(ctx, watermark); err != nil {
			faults = append(faults, fmt.Errorf("events: subscriber %q: %w", s.name, err))
		}
	}
	return faults
}

// reportParked hands one freshly parked delivery to OnDeadLetter, when
// one is configured.
func (r *Registry) reportParked(dl DeadLetter) {
	if r.cfg.OnDeadLetter != nil {
		r.cfg.OnDeadLetter(dl)
	}
}

// attemptDelivery runs one delivery through handler within its retry
// budget, reporting how many attempts were made and the last error - nil
// once the handler accepted the event. A cancelled ctx ends the schedule
// early with whatever the last attempt returned.
func attemptDelivery(ctx context.Context, handler Handler, e Event, maxAttempts int, delay time.Duration) (int, error) {
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 && !waitRetry(ctx, delay) {
			return attempt - 1, last
		}
		if last = invoke(ctx, handler, e); last == nil {
			return attempt, nil
		}
	}
	return maxAttempts, last
}
