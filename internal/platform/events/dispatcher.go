package events

// The dispatcher half of the package: reading the stream a Writer appends
// to, and turning it into handler calls without ever losing an event.
//
// The design centre is what the event contract does and does not promise.
// Ordering holds per (type, subject) and nowhere else, so the unit of both
// ordering and failure isolation is the Lane: within one lane events are
// delivered in stream order and a failure holds everything behind it;
// across lanes nothing is ordered and nothing is coupled. Failure is
// visible by rule 4 of the contract, so a delivery that spends its retry
// budget halts its lane loudly - through OnHalt and through Tick's error,
// on every tick until it is resolved - and pins the checkpoint behind the
// failed event so that a restart retries it. A silently dropped event is
// a defect; a halted lane is the design refusing to become one.
//
// The second thing that pins the checkpoint is the gap between the order
// the stream is read in and the order rows appear in. domain_event stamps
// occurred_at with now(), which is the appending transaction's START time,
// while the row becomes readable only at its COMMIT. An event appended
// early and committed late therefore arrives BEHIND events appended later
// and committed sooner, so a checkpoint that had moved over those would
// leave it behind the read position forever. pollStream asks the database
// which rows are past that risk - see its comment for the two bounds it
// asks for - and Tick advances the checkpoint over those alone, while
// still delivering the rest. Delivery is at-least-once and re-reading is
// free; a skipped event is unrecoverable, so the checkpoint is the half
// that waits. The Registry's durable per-subscriber checkpoint answers to
// the same rule, through the same poll (subscriber.go).

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// DefaultBatchSize is how many events one Tick reads past the
	// checkpoint. It bounds a tick's work, and with it how far delivery
	// can run ahead of a checkpoint pinned by a halted lane or by an
	// in-flight producer transaction.
	DefaultBatchSize = 100
	// DefaultMaxAttempts is how many times one delivery is attempted
	// before its lane halts.
	DefaultMaxAttempts = 3
	// DefaultRetryDelay is the wait between attempts of one delivery.
	DefaultRetryDelay = 100 * time.Millisecond
)

// ErrStarted reports a registration attempted after the first Tick.
// Registration closes when delivery starts, so a subscriber added late
// fails loudly instead of quietly seeing only the events that happen to
// come after it.
var ErrStarted = errors.New("events: dispatcher has already started delivering; register every handler before the first Tick")

// Querier is the narrow slice of database access the dispatcher needs:
// running the two reads a poll is made of - the batch of events, and the
// bound saying how far the checkpoint may follow it. *pgxpool.Pool
// satisfies it, and so does pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Handler consumes one event. Delivery is at-least-once, so every handler
// must be idempotent: safe to run twice on the same EventID (consumer
// rule 1 of the event contract - record processed ids, treat a redelivery
// as a no-op). A nil return acknowledges the event; an error is retried
// within the delivery's budget and then halts the event's lane. A panic
// is recovered and counts as an error.
type Handler func(ctx context.Context, event Event) error

// Lane is the unit of ordering and of failure isolation: one
// (type, subject) pair. Within a lane events are delivered in stream
// order and a failure holds everything behind it; across lanes there is
// no order and no coupling - exactly the guarantee the event contract
// draws.
type Lane struct {
	// Type is the event type the lane belongs to.
	Type string
	// Subject is the subject shared by the lane's events; uuid.Nil is
	// the lane of a type's subjectless events.
	Subject uuid.UUID
}

// String renders the lane for logs and errors.
func (l Lane) String() string {
	if l.Subject == uuid.Nil {
		return "(" + l.Type + ", no subject)"
	}
	return "(" + l.Type + ", " + l.Subject.String() + ")"
}

// Halt is one lane the dispatcher stopped delivering: a delivery on it
// spent its whole retry budget. The halted event stays in the stream and
// ahead of the checkpoint - a halt is loud back-pressure, never a drop -
// so a process restart retries it. The Registry refines this posture into
// its durable form: a delivery that keeps failing there is parked in the
// dead-letter table instead of being retried on every restart.
type Halt struct {
	// Lane is the lane that halted.
	Lane Lane
	// Event is the delivery that spent the budget.
	Event Event
	// Attempts is how many times it was attempted.
	Attempts int
	// Err is the error the final attempt returned.
	Err error
}

// Checkpoint is a position in the stream, identified the way the stream
// is ordered: occurred_at first, id as the tiebreak. The zero value is
// the start of the stream.
//
// A saved checkpoint is a claim that nothing at or before it will ever
// need reading again, so neither delivery layer saves a position that a
// still-uncommitted append could undercut (see pollStream). Everything
// after it is read again on the next tick, which is what lets an event
// that became visible late still be found.
type Checkpoint struct {
	// OccurredAt is the position's time axis.
	OccurredAt time.Time
	// EventID breaks occurred_at ties.
	EventID uuid.UUID
}

// CheckpointStore persists how far the dispatcher has read. A Load with
// nothing saved reports the start of the stream, which is the safe
// direction: at-least-once redelivers, it never skips. MemoryCheckpoints
// is the in-process implementation; PostgresCheckpoints is the durable
// one.
type CheckpointStore interface {
	// Load returns the last saved checkpoint, or the zero Checkpoint
	// when none was ever saved.
	Load(ctx context.Context) (Checkpoint, error)
	// Save records cp as the position to resume from.
	Save(ctx context.Context, cp Checkpoint) error
}

// MemoryCheckpoints is the in-process CheckpointStore: a checkpoint that
// lives and dies with its process. Losing it is safe by design - the next
// start re-reads the stream from the beginning and the idempotent
// handlers absorb the redelivery - but on a long stream that is a real
// cost, so it suits tests and short-lived processes. PostgresCheckpoints
// is the durable implementation.
type MemoryCheckpoints struct {
	mu sync.Mutex
	cp Checkpoint
}

var _ CheckpointStore = (*MemoryCheckpoints)(nil)

// NewMemoryCheckpoints builds an empty store: its first Load reports the
// start of the stream.
func NewMemoryCheckpoints() *MemoryCheckpoints {
	return &MemoryCheckpoints{}
}

// Load returns the last saved checkpoint, or the zero Checkpoint when
// nothing was ever saved.
func (m *MemoryCheckpoints) Load(_ context.Context) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cp, nil
}

// Save records cp.
func (m *MemoryCheckpoints) Save(_ context.Context, cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cp = cp
	return nil
}

// DispatcherConfig tunes the dispatcher. The zero value is usable: every
// field falls back to its documented default.
type DispatcherConfig struct {
	// BatchSize is how many events one Tick reads past the checkpoint.
	// Zero or negative means DefaultBatchSize.
	BatchSize int
	// MaxAttempts is how many times one delivery is attempted before its
	// lane halts. Zero or negative means DefaultMaxAttempts.
	MaxAttempts int
	// RetryDelay is the wait between attempts of one delivery. Zero or
	// negative means DefaultRetryDelay; retrying with no delay at all is
	// deliberately not offered, because the failure being retried is
	// usually still happening.
	RetryDelay time.Duration
	// OnHalt is called from inside Tick at the moment a lane halts, once
	// per halt. It must not call back into the dispatcher. Nil means
	// halts are reported only through Tick's error.
	OnHalt func(Halt)
}

// withDefaults returns cfg with every unset or out-of-range field
// replaced by its default.
func (c DispatcherConfig) withDefaults() DispatcherConfig {
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

// Dispatcher polls the stream and fans events out to registered handlers.
// Construct it with NewDispatcher, add handlers with Register, then drive
// it by calling Tick on a schedule.
//
// Ordering: events are delivered in stream order within each lane, and
// lanes do not block each other - a failing delivery halts its own lane
// and the rest keep flowing, up to the batch bound. Stream order is the
// order of the rows a poll can read: an event whose transaction commits
// after its lane-mates were already delivered stands earlier in the
// stream than they do and is still delivered late, because the only
// alternative to a late delivery is no delivery at all.
//
// No skips: the checkpoint never moves over a position that an append
// still in flight could land in front of, so an event that becomes
// visible after the poll that would have passed it is read by the next
// poll rather than lost. That is the guarantee handlers rely on; what
// they owe in return is idempotence, because holding the checkpoint back
// means re-reading, and re-reading means redelivery.
//
// Failure: when a delivery spends its retry budget the lane halts, the
// halt is handed to OnHalt and returned from Tick, and the checkpoint
// stops advancing at the failed event, so nothing is ever silently
// dropped - a restart retries from there.
//
// State: the checkpoint is the only state that outlives the process.
// Which lanes are halted, and which events past a pinned checkpoint are
// already delivered, is tracked in memory; after a restart or a failover
// the new instance retries from the checkpoint, which is the
// at-least-once posture working as designed. Durable per-delivery
// tracking is provided by the Registry.
//
// Tick is shaped to be a scheduler job's Run
// (internal/platform/scheduler): the advisory lock there keeps two
// instances from ticking at once, and a Tick serialises against every
// other call on the same dispatcher regardless.
type Dispatcher struct {
	db          Querier
	checkpoints CheckpointStore
	cfg         DispatcherConfig

	mu        sync.Mutex
	handlers  map[string][]Handler
	started   bool
	halted    map[Lane]Halt
	delivered map[uuid.UUID]struct{}
}

// NewDispatcher builds a dispatcher that reads events from db and its
// position from checkpoints. Handlers are registered separately, before
// the first Tick.
func NewDispatcher(db Querier, checkpoints CheckpointStore, cfg DispatcherConfig) *Dispatcher {
	return &Dispatcher{
		db:          db,
		checkpoints: checkpoints,
		cfg:         cfg.withDefaults(),
		handlers:    make(map[string][]Handler),
		halted:      make(map[Lane]Halt),
		delivered:   make(map[uuid.UUID]struct{}),
	}
}

// Register subscribes handler to every event of the named type. Handlers
// for one type run in registration order, and all of them must accept an
// event before it counts as delivered. An event whose type has no handler
// is passed over, exactly as the contract requires consumers to ignore
// unknown types. Registration closes at the first Tick; a late call fails
// with ErrStarted.
func (d *Dispatcher) Register(eventType string, handler Handler) error {
	if strings.TrimSpace(eventType) == "" {
		return errors.New("events: register: a subscription needs an event type")
	}
	if handler == nil {
		return fmt.Errorf("events: register %s: handler must not be nil", eventType)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return fmt.Errorf("events: register %s: %w", eventType, ErrStarted)
	}
	d.handlers[eventType] = append(d.handlers[eventType], handler)
	return nil
}

// Tick is one poll-and-deliver pass: read a batch of events past the
// checkpoint in stream order, deliver each through the handlers
// registered for its type, and save the checkpoint over the contiguous
// prefix that both succeeded and is safe to stop reading. A cancelled ctx
// ends the pass early, keeping whatever the checkpoint already covers.
//
// The returned error is the tick's loud surface: infrastructure
// failures, every lane that halted during this tick, and - again, on
// every later tick - every halted lane still holding events back. It
// never goes quiet while something is undelivered.
func (d *Dispatcher) Tick(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = true

	cp, err := d.checkpoints.Load(ctx)
	if err != nil {
		return fmt.Errorf("events: tick: load checkpoint: %w", err)
	}
	batch, err := pollStream(ctx, d.db, cp, d.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("events: tick: %w", err)
	}

	var (
		faults    []error
		watermark = cp
		advanced  bool
		advancing = true
		reported  = make(map[Lane]bool)
	)
	// pass records that e needs no further delivery: the watermark moves
	// over it while the prefix is still contiguous, and otherwise it is
	// remembered so later ticks do not deliver it again.
	pass := func(e Event) {
		if advancing {
			watermark, advanced = position(e), true
			delete(d.delivered, e.EventID)
		} else {
			d.delivered[e.EventID] = struct{}{}
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
			// lands behind this row in stream order. Saving a checkpoint
			// here would move the read position past a place an event can
			// still appear in. Deliver the row, and leave the checkpoint
			// where the next poll will look at that place again.
			advancing = false
		}
		lane := Lane{Type: e.Type, Subject: e.Subject}
		if _, done := d.delivered[e.EventID]; done {
			pass(e)
			continue
		}
		if halt, isHalted := d.halted[lane]; isHalted {
			// Per-lane ordering: nothing later on a halted lane is
			// delivered before its halted event succeeds.
			advancing = false
			if !reported[lane] {
				reported[lane] = true
				faults = append(faults, fmt.Errorf(
					"events: lane %s is halted at event %s and holds back later events; a restart retries it from the checkpoint: %w",
					lane, halt.Event.EventID, halt.Err))
			}
			continue
		}
		if len(d.handlers[e.Type]) == 0 {
			pass(e)
			continue
		}
		if err := d.deliver(ctx, e); err != nil {
			if ctx.Err() != nil {
				// The failure is the shutdown's, not the lane's: the
				// event stays ahead of the watermark for the next tick.
				break
			}
			halt := Halt{Lane: lane, Event: e, Attempts: d.cfg.MaxAttempts, Err: err}
			d.halted[lane] = halt
			reported[lane] = true
			if d.cfg.OnHalt != nil {
				d.cfg.OnHalt(halt)
			}
			faults = append(faults, fmt.Errorf(
				"events: delivering event %s on lane %s failed after %d attempt(s); the lane is halted: %w",
				e.EventID, lane, halt.Attempts, err))
			advancing = false
			continue
		}
		pass(e)
	}

	if advanced {
		if err := d.checkpoints.Save(ctx, watermark); err != nil {
			faults = append(faults, fmt.Errorf("events: tick: save checkpoint: %w", err))
		}
	}
	return errors.Join(faults...)
}

// polled is one row of a poll: the event, and whether the checkpoint may
// be saved over it. Settledness is a fact about the read that found the
// row, not about the event, so it stays here instead of on the Event a
// handler receives.
type polled struct {
	event   Event
	settled bool
}

// pollStream reads the next batch: every event strictly after cp in
// stream order, up to limit. The order is (occurred_at, id): occurred_at
// is the stream's time axis, and the id breaks its ties so the order is
// total and stable across polls. The tiebreak means nothing and never
// needs to - the only order the stream promises is per lane, and any
// stable total order preserves it. It is the one poll both delivery
// layers share, so the Dispatcher and the Registry cannot disagree about
// what stream order is, nor about which rows a checkpoint may be saved
// over - both of them record a position, so both are exposed to the same
// skip.
//
// Each row is also marked settled or not, because stream order is not the
// order rows become readable in. occurred_at defaults to now(), which is
// the appending transaction's START time, while the row is readable only
// once that transaction COMMITS; a producer that appends early and
// commits late lands BEHIND events appended later and committed sooner.
// A checkpoint saved over those later events would put the late one
// behind the read position permanently - a committed event silently
// dropped, which consumer rule 4 of the event contract calls a defect.
//
// A row is settled when both of these hold, and a checkpoint moves over
// settled rows only:
//
//   - Its occurred_at is before the start of the oldest transaction open
//     in this database. Every event that can still appear is being
//     written by a transaction open right now, and every transaction that
//     has not begun yet will begin later and stamp a later occurred_at,
//     so that start time is the line no unread event can be in front of.
//     It is read first and in its own statement: a bound read afterwards
//     could already have moved past a transaction that was open when the
//     rows were read, which is the one direction that is not safe.
//   - Its own xmin precedes the transaction-id horizon of the snapshot
//     that read it, pg_snapshot_xmin(pg_current_snapshot()), which says
//     the transaction that wrote the row had already finished. Both come
//     out of this one statement, so they describe one instant. This test
//     needs no view and no privilege, which is why it is here as well:
//     a producer that appends and then stays open holds the horizon at
//     its own transaction id, so nothing appended after it can be passed
//     while it runs - even if that producer's session is one the first
//     bound cannot see. The package doc sets out what each covers.
func pollStream(ctx context.Context, db Querier, cp Checkpoint, limit int) ([]polled, error) {
	openSince, err := oldestOpenTransaction(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(ctx,
		`select e.id::text, e.type, e.version, e.occurred_at, e.producer,
		        e.subject::text, e.idempotency_key, e.payload,
		        e.xmin::text::bigint,
		        pg_snapshot_xmin(pg_current_snapshot())::xid::text::bigint
		 from domain_event e
		 where (e.occurred_at, e.id) > ($1, $2::uuid)
		 order by e.occurred_at, e.id
		 limit $3`,
		cp.OccurredAt, cp.EventID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("poll after event %s: %w", cp.EventID, err)
	}
	defer rows.Close()

	var batch []polled
	for rows.Next() {
		var (
			id, eventType, producer string
			version                 int
			occurredAt              time.Time
			subject, key            *string
			payload                 []byte
			rowXID, horizonXID      int64
		)
		if err := rows.Scan(&id, &eventType, &version, &occurredAt, &producer, &subject, &key, &payload,
			&rowXID, &horizonXID); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e, err := buildEvent(id, eventType, version, occurredAt, producer, subject, key, payload)
		if err != nil {
			return nil, err
		}
		batch = append(batch, polled{
			event:   e,
			settled: occurredAt.Before(openSince) && xidPrecedes(xid32(rowXID), xid32(horizonXID)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return batch, nil
}

// oldestOpenTransaction reports when the oldest transaction still open in
// this database started, which is the earliest occurred_at any event that
// has not appeared yet can carry: occurred_at is a transaction's start
// time, so an event still to be appended either belongs to one of these
// transactions or to one that has not started, and one that has not
// started will start later than this call. With no transaction open at
// all the answer is the current instant - nothing is pending, so nothing
// is held back.
//
// Scoped twice, and both scopings are about which transactions could
// possibly append. To this database, because a transaction connected
// elsewhere in the cluster cannot write this table - which also keeps an
// unrelated database's long transaction from holding this stream's
// checkpoint. And to client backends, because an event is appended by an
// application connection: a vacuum worker chewing through this very table
// for half an hour holds no event back, and must not hold the checkpoint
// back either.
//
// The minimum covers every session the reading role is allowed to see the
// transaction state of - its own role's, and every role's if it holds
// pg_read_all_stats - which is why producers append through a connection
// of the application's own role, and why the xmin horizon in pollStream
// stands behind this bound rather than beside it.
func oldestOpenTransaction(ctx context.Context, db Querier) (time.Time, error) {
	rows, err := db.Query(ctx,
		`select coalesce(min(xact_start), clock_timestamp())
		 from pg_stat_activity
		 where datname = current_database()
		   and backend_type = 'client backend'
		   and xact_start is not null`)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the open-transaction horizon: %w", err)
	}
	defer rows.Close()

	var openSince time.Time
	if rows.Next() {
		if err := rows.Scan(&openSince); err != nil {
			return time.Time{}, fmt.Errorf("scan the open-transaction horizon: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, fmt.Errorf("read the open-transaction horizon: %w", err)
	}
	if openSince.IsZero() {
		return time.Time{}, errors.New("the open-transaction horizon came back empty")
	}
	return openSince, nil
}

// firstNormalXID is Postgres's FirstNormalTransactionId. The ids below it
// are the bootstrap and frozen markers rather than transactions: they
// stand for "older than anything that could still be running", which is
// what a row carries once vacuum has frozen it.
const firstNormalXID = 3

// xid32 narrows a transaction id read as bigint back to the 32 bits
// Postgres keeps it in. The width is an artefact of the wire: both values
// the poll reads are 32-bit transaction ids already - one an xmin column,
// one a snapshot horizon cast down to xid - and bigint is simply the
// integer type they can both be read as without losing any of them.
func xid32(v int64) uint32 {
	//nolint:gosec // G115: the mask is the conversion; the source is a 32-bit xid.
	return uint32(v & 0xffffffff)
}

// xidPrecedes reports whether transaction id a comes before b, as
// Postgres's own TransactionIdPrecedes does. The comparison has to be
// modular: transaction ids are a 32-bit counter that wraps, so the
// arithmetic difference of two of them says nothing and only the signed
// distance does - which is why the poll compares ids this way instead of
// as the plain numbers they are printed as. The frozen and bootstrap
// markers are not on the circle at all, so they compare directly and come
// before every real transaction.
func xidPrecedes(a, b uint32) bool {
	if a < firstNormalXID || b < firstNormalXID {
		return a < b
	}
	//nolint:gosec // G115: the wrapped difference is the comparison, not a lost value.
	return int32(a-b) < 0
}

// buildEvent assembles one Event from its scanned columns, turning the
// database's nullable text renderings back into the envelope's Go shapes.
func buildEvent(id, eventType string, version int, occurredAt time.Time, producer string, subject, key *string, payload []byte) (Event, error) {
	eventID, err := uuid.Parse(id)
	if err != nil {
		return Event{}, fmt.Errorf("event id %q: %w", id, err)
	}
	subjectID := uuid.Nil
	if subject != nil {
		if subjectID, err = uuid.Parse(*subject); err != nil {
			return Event{}, fmt.Errorf("event %s: subject %q: %w", id, *subject, err)
		}
	}
	idempotencyKey := ""
	if key != nil {
		idempotencyKey = *key
	}
	return Event{
		EventID:        eventID,
		Type:           eventType,
		Version:        version,
		OccurredAt:     occurredAt,
		Producer:       producer,
		Subject:        subjectID,
		IdempotencyKey: idempotencyKey,
		Payload:        payload,
	}, nil
}

// deliver is one delivery: every handler registered for the event's type,
// in registration order, and all of them again on a retry - handlers are
// idempotent, so re-running one that already succeeded is the contract
// working, not a defect. It returns nil once every handler accepted the
// event, and the last error once the budget is spent or ctx ended.
func (d *Dispatcher) deliver(ctx context.Context, e Event) error {
	var last error
	for attempt := 1; attempt <= d.cfg.MaxAttempts; attempt++ {
		if attempt > 1 && !waitRetry(ctx, d.cfg.RetryDelay) {
			return last
		}
		last = nil
		for _, h := range d.handlers[e.Type] {
			if err := invoke(ctx, h, e); err != nil {
				last = err
				break
			}
		}
		if last == nil {
			return nil
		}
	}
	return last
}

// invoke calls one handler with its panic recovered into an error, so a
// bug in one handler is that delivery's failure - retried, then halting
// its lane - and never the end of the tick or the process.
func invoke(ctx context.Context, h Handler, e Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("events: handler for %s panicked: %v\n%s", e.Type, r, debug.Stack())
		}
	}()
	return h(ctx, e)
}

// waitRetry waits out the between-attempts delay, reporting false when
// ctx ended first - a shutdown must not sit out a retry schedule.
func waitRetry(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// position is the stream position of one event, as a checkpoint.
func position(e Event) Checkpoint {
	return Checkpoint{OccurredAt: e.OccurredAt, EventID: e.EventID}
}
