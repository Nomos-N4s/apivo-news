package events_test

// The registry's obligations are the durable half of the event contract:
// at-least-once delivery deduplicated by the (subscriber, event) delivery
// idempotency key, per-subscriber progress so one consumer never stalls
// another, and replay that leaves the tracking tables exactly as they
// were.
//
// These suites run on the shared scratch database (dispatcherDB) beside
// the dispatcher's, with every test keeping to its own subscriber names
// and event types (randomSuffix): a subscriber only ever touches its own
// rows, and passes over every type it does not handle.
//
// One test breaks that pattern on purpose. A durable checkpoint's hard
// case is a producer transaction that is open while a poll runs, so
// TestRegistryNeverStepsPastAnOpenProducerTransaction drives two real
// concurrent transactions and holds one of them open across a tick.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// brokenDB satisfies events.DB by failing every call, so the
// infrastructure-failure surface can be pinned without a database.
type brokenDB struct{}

var errDatabaseDown = errors.New("the database is down")

func (brokenDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errDatabaseDown
}

func (brokenDB) QueryRow(context.Context, string, ...any) pgx.Row { return brokenRow{} }

func (brokenDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errDatabaseDown
}

// brokenRow is the row a brokenDB hands out: it fails at Scan, which is
// where pgx surfaces a QueryRow error.
type brokenRow struct{}

func (brokenRow) Scan(...any) error { return errDatabaseDown }

// drainRegistry ticks the registry until done reports true, failing the
// test if a tick errors or the budget is reached first.
//
// The ticks are paced rather than spun, for the reason the checkpoint
// exists: a durable position may only be saved over rows no append still
// in flight can land in front of, so a tick makes no progress at all
// while another connection is mid-append - and on a shared database that
// connection belongs to another suite entirely. Pausing between ticks
// spends the budget waiting for those appends to commit instead of on
// hammering the same query.
func drainRegistry(t *testing.T, r *events.Registry, done func() bool) {
	t.Helper()
	for range 500 {
		if done() {
			return
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the registry did not reach the expected state within 500 ticks")
}

// tickUntil ticks the registry until done reports true, collecting rather
// than failing on tick errors - the parked-lane tests are about ticks
// that error by design - and returns the last one. It paces its ticks for
// the same reason drainRegistry does.
func tickUntil(t *testing.T, r *events.Registry, done func() bool) error {
	t.Helper()
	var lastErr error
	for i := 0; !done(); i++ {
		if i > 500 {
			t.Fatalf("the registry did not reach the expected state within 500 ticks; last error: %v", lastErr)
		}
		if i > 0 {
			time.Sleep(time.Millisecond)
		}
		lastErr = r.Tick(context.Background())
	}
	return lastErr
}

// checkpointPassed reports whether the named subscriber's durable
// checkpoint has moved past the event's stream position.
func checkpointPassed(t *testing.T, pool *pgxpool.Pool, subscriber string, e events.Event) bool {
	t.Helper()
	store, err := events.NewPostgresCheckpoints(pool, subscriber)
	if err != nil {
		t.Fatalf("NewPostgresCheckpoints: %v", err)
	}
	cp, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return !checkpointBefore(cp, e)
}

// resetCheckpoint erases the named subscriber's durable position - the
// lost-checkpoint crash the delivery idempotency key exists to absorb.
func resetCheckpoint(t *testing.T, pool *pgxpool.Pool, subscriber string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`delete from subscriber_checkpoint where subscriber = $1`, subscriber); err != nil {
		t.Fatalf("resetting the checkpoint: %v", err)
	}
}

// deliveryRow reads one subscriber's completion row for one event.
func deliveryRow(t *testing.T, pool *pgxpool.Pool, subscriber string, eventID uuid.UUID) (attempts int, deliveredAt time.Time, found bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`select attempts, delivered_at from event_delivery where subscriber = $1 and event_id = $2`,
		subscriber, eventID.String())
	if err != nil {
		t.Fatalf("reading the delivery row: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&attempts, &deliveredAt); err != nil {
			t.Fatalf("scanning the delivery row: %v", err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the delivery row: %v", err)
	}
	return attempts, deliveredAt, found
}

// deliveryState renders every tracking row of one subscriber - completed
// deliveries and dead letters alike, timestamps included - so two moments
// in time can be compared exactly.
func deliveryState(t *testing.T, pool *pgxpool.Pool, subscriber string) []string {
	t.Helper()
	ctx := context.Background()
	var state []string

	read := func(query, shape string) {
		rows, err := pool.Query(ctx, query, subscriber)
		if err != nil {
			t.Fatalf("reading delivery state: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id, detail string
				attempts   int
				at         time.Time
			)
			if err := rows.Scan(&id, &attempts, &at, &detail); err != nil {
				t.Fatalf("scanning delivery state: %v", err)
			}
			state = append(state, fmt.Sprintf(shape, id, attempts, at.UTC().Format(time.RFC3339Nano), detail))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading delivery state: %v", err)
		}
	}

	read(`select event_id::text, attempts, delivered_at, ''::text from event_delivery where subscriber = $1 order by event_id`,
		"delivered %s attempts=%d at=%s%s")
	read(`select event_id::text, attempts, parked_at, last_error || ' requeued=' || coalesce(requeued_at::text, 'never')
	      from event_dead_letter where subscriber = $1 order by event_id`,
		"parked %s attempts=%d at=%s %s")
	return state
}

func TestPostgresCheckpointsValidateTheSubscriber(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   "} {
		if _, err := events.NewPostgresCheckpoints(nil, name); err == nil {
			t.Errorf("NewPostgresCheckpoints(%q) accepted a name that cannot key a stored position", name)
		}
	}
}

func TestPostgresCheckpointsRoundTrip(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	one, err := events.NewPostgresCheckpoints(pool, "checkpoints-one-"+randomSuffix(t))
	if err != nil {
		t.Fatalf("NewPostgresCheckpoints: %v", err)
	}
	two, err := events.NewPostgresCheckpoints(pool, "checkpoints-two-"+randomSuffix(t))
	if err != nil {
		t.Fatalf("NewPostgresCheckpoints: %v", err)
	}

	empty, err := one.Load(ctx)
	if err != nil {
		t.Fatalf("Load with nothing saved: %v", err)
	}
	if !empty.OccurredAt.IsZero() || empty.EventID != uuid.Nil {
		t.Fatalf("an unsaved subscriber reports %+v, want the start of the stream", empty)
	}

	first := events.Checkpoint{OccurredAt: time.Now().UTC().Truncate(time.Microsecond), EventID: uuid.New()}
	if err := one.Save(ctx, first); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := one.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.OccurredAt.Equal(first.OccurredAt) || got.EventID != first.EventID {
		t.Fatalf("Load returned %+v, want %+v", got, first)
	}

	// Saving again replaces: a checkpoint is a position, not a history.
	second := events.Checkpoint{OccurredAt: first.OccurredAt.Add(time.Second), EventID: uuid.New()}
	if err := one.Save(ctx, second); err != nil {
		t.Fatalf("Save over an existing position: %v", err)
	}
	got, err = one.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.OccurredAt.Equal(second.OccurredAt) || got.EventID != second.EventID {
		t.Fatalf("Load after the second save returned %+v, want %+v", got, second)
	}

	// Positions are per subscriber: one's save is invisible to the other.
	other, err := two.Load(ctx)
	if err != nil {
		t.Fatalf("Load for the other subscriber: %v", err)
	}
	if !other.OccurredAt.IsZero() || other.EventID != uuid.Nil {
		t.Fatalf("another subscriber's position leaked: %+v", other)
	}
}

func TestSubscribeValidates(t *testing.T) {
	t.Parallel()
	handler := func(context.Context, events.Event) error { return nil }
	tests := []struct {
		name       string
		subscriber string
		types      []string
		handler    events.Handler
	}{
		{name: "an empty subscriber name", subscriber: "", types: []string{"cashback.entry.created"}, handler: handler},
		{name: "a blank subscriber name", subscriber: "   ", types: []string{"cashback.entry.created"}, handler: handler},
		{name: "no event types", subscriber: "wallet", types: nil, handler: handler},
		{name: "a blank event type", subscriber: "wallet", types: []string{"cashback.entry.created", " "}, handler: handler},
		{name: "a duplicated event type", subscriber: "wallet", types: []string{"cashback.entry.created", "cashback.entry.created"}, handler: handler},
		{name: "a nil handler", subscriber: "wallet", types: []string{"cashback.entry.created"}, handler: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := events.NewRegistry(nil, events.RegistryConfig{})
			if err := r.Subscribe(tt.subscriber, tt.types, tt.handler); err == nil {
				t.Fatal("Subscribe accepted a subscriber that could never run")
			}
		})
	}
}

func TestSubscribeRefusesADuplicateName(t *testing.T) {
	t.Parallel()
	handler := func(context.Context, events.Event) error { return nil }
	r := events.NewRegistry(nil, events.RegistryConfig{})
	if err := r.Subscribe("wallet", []string{"cashback.entry.created"}, handler); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if err := r.Subscribe("wallet", []string{"cashback.entry.reversed"}, handler); err == nil {
		t.Fatal("Subscribe accepted a second subscriber under the same name; their durable state would collide")
	}
}

// TestSubscribeClosesAtTheFirstTick pins the registration lifecycle to
// the dispatcher's: a subscriber added after delivery has started would
// quietly see only the events that happen to come later, so the late call
// must fail instead.
func TestSubscribeClosesAtTheFirstTick(t *testing.T) {
	t.Parallel()
	handler := func(context.Context, events.Event) error { return nil }

	r := events.NewRegistry(nil, events.RegistryConfig{})
	// No subscribers registered, so the tick has no database work to do -
	// it only closes registration.
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick with no subscribers: %v", err)
	}
	err := r.Subscribe("wallet", []string{"cashback.entry.created"}, handler)
	if !errors.Is(err, events.ErrStarted) {
		t.Fatalf("Subscribe after the first tick: got %v, want ErrStarted", err)
	}
}

// TestInfrastructureFailureIsLoudAndPerSubscriber pins two things at
// once: a database that is down surfaces through Tick's error rather than
// silence, and one subscriber's failure does not end the pass - every
// subscriber is still given its turn and named in the report.
func TestInfrastructureFailureIsLoudAndPerSubscriber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler := func(context.Context, events.Event) error { return nil }

	r := events.NewRegistry(brokenDB{}, events.RegistryConfig{})
	first, second := "broken-one", "broken-two"
	for _, name := range []string{first, second} {
		if err := r.Subscribe(name, []string{"cashback.entry.created"}, handler); err != nil {
			t.Fatalf("Subscribe %s: %v", name, err)
		}
	}
	err := r.Tick(ctx)
	if err == nil {
		t.Fatal("Tick over a failing database returned nil; an infrastructure failure must be loud")
	}
	for _, name := range []string{first, second} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the tick error %v does not name subscriber %q; every subscriber's failure must be reported", err, name)
		}
	}

	// The durable checkpoint store and the operator primitives fail the
	// same loud way.
	store, storeErr := events.NewPostgresCheckpoints(brokenDB{}, "broken-checkpoints")
	if storeErr != nil {
		t.Fatalf("NewPostgresCheckpoints: %v", storeErr)
	}
	if _, err := store.Load(ctx); err == nil {
		t.Error("Load over a failing database returned nil")
	}
	if err := store.Save(ctx, events.Checkpoint{OccurredAt: time.Now(), EventID: uuid.New()}); err == nil {
		t.Error("Save over a failing database returned nil")
	}
	if err := events.Requeue(ctx, brokenDB{}, "broken", uuid.New()); err == nil || errors.Is(err, events.ErrNotParked) {
		t.Errorf("Requeue over a failing database returned %v; a database failure is not ErrNotParked", err)
	}
	if _, err := events.ListDeadLetters(ctx, brokenDB{}, "broken"); err == nil {
		t.Error("ListDeadLetters over a failing database returned nil")
	}
}

// TestShutdownDoesNotParkTheDelivery draws the line between a poisoned
// delivery and a cancelled one: a handler failure during shutdown is the
// shutdown's, so nothing is parked, nothing completes, and the next
// healthy tick simply delivers the event.
func TestShutdownDoesNotParkTheDelivery(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.shutdown" + suffix + ".tested"
	subscriberName := "shutdown-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	var cancelTick context.CancelFunc
	calls, delivered := 0, 0
	r := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 3, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(handlerCtx context.Context, _ events.Event) error {
		calls++
		if handlerCtx.Err() != nil {
			return handlerCtx.Err()
		}
		if calls == 1 {
			// The shutdown arrives mid-delivery: the attempt fails, and
			// the retry schedule must not be sat out.
			cancelTick()
			return errors.New("interrupted by shutdown")
		}
		delivered++
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Tick until the delivery is reached (earlier ticks may only be
	// chewing through other suites' events on the shared stream); the
	// tick that reaches it is shut down from inside the handler. Ticks
	// may error during the shutdown - a cancelled context fails whatever
	// statement it reaches next - so what matters is what was NOT done:
	// no park, no completion, exactly the one interrupted attempt.
	for i := 0; calls == 0; i++ {
		if i > 200 {
			t.Fatal("the delivery was never attempted within 200 ticks")
		}
		tickCtx, cancel := context.WithCancel(context.Background())
		cancelTick = cancel
		_ = r.Tick(tickCtx)
		cancel()
	}
	if calls != 1 {
		t.Fatalf("the interrupted delivery was attempted %d time(s); a shutdown must not sit out the retry schedule", calls)
	}
	if _, found := findDeadLetter(t, pool, subscriberName, event.EventID); found {
		t.Fatal("a delivery that failed because of the shutdown was parked; the failure is the shutdown's, not the lane's")
	}
	if _, _, found := deliveryRow(t, pool, subscriberName, event.EventID); found {
		t.Fatal("a delivery that never succeeded has a completion row")
	}

	// The restart delivers it as if nothing happened.
	drainRegistry(t, r, func() bool { return delivered == 1 })
	if _, _, found := deliveryRow(t, pool, subscriberName, event.EventID); !found {
		t.Fatal("the delivery after the restart left no completion row")
	}
}

// TestRedeliveryIsARecordedNoOp is the delivery idempotency key doing its
// one job: the same event delivered twice - here through the
// lost-checkpoint crash, which replays the stream - reaches the handler
// once, because the second delivery finds the completion row and is a
// recorded no-op. The tracking row itself must come through untouched.
func TestRedeliveryIsARecordedNoOp(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.noop" + suffix + ".tested"
	subscriberName := "noop-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	handled := 0
	r := events.NewRegistry(pool, events.RegistryConfig{})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		handled++
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainRegistry(t, r, func() bool { return checkpointPassed(t, pool, subscriberName, event) })

	if handled != 1 {
		t.Fatalf("the handler ran %d time(s) on first delivery, want 1", handled)
	}
	attempts, deliveredAt, found := deliveryRow(t, pool, subscriberName, event.EventID)
	if !found {
		t.Fatal("no completion row was recorded; without it every redelivery would reach the handler")
	}
	if attempts != 1 {
		t.Fatalf("the completion row records %d attempt(s), want 1", attempts)
	}

	// The crash: the checkpoint is gone, the delivery table is not. The
	// registry re-reads the stream from the start and must pass the event
	// without running the handler again.
	resetCheckpoint(t, pool, subscriberName)
	drainRegistry(t, r, func() bool { return checkpointPassed(t, pool, subscriberName, event) })

	if handled != 1 {
		t.Fatalf("the handler ran %d time(s) across delivery and redelivery, want exactly 1 - the redelivery must be a recorded no-op", handled)
	}
	attemptsAfter, deliveredAtAfter, found := deliveryRow(t, pool, subscriberName, event.EventID)
	if !found {
		t.Fatal("the completion row disappeared during the replay")
	}
	if attemptsAfter != attempts || !deliveredAtAfter.Equal(deliveredAt) {
		t.Fatalf("the replay rewrote the completion row: attempts %d->%d, delivered_at %v->%v",
			attempts, attemptsAfter, deliveredAt, deliveredAtAfter)
	}
}

// TestRegistryNeverStepsPastAnOpenProducerTransaction is the no-skip
// guarantee where it costs the most: a durable checkpoint.
//
// occurred_at is the appending transaction's start time, and a row is
// readable only once that transaction commits, so an event appended by a
// transaction that stays open stands EARLIER in the stream than
// everything appended and committed while it is open, and appears LATER
// than all of it. A subscriber that saved a position over the readable
// events would leave that one behind its read position - and this
// position is a row in a table, so unlike the dispatcher's the mistake
// survives every restart. The event is not delivered late; it is never
// delivered, to this subscriber, ever.
//
// The subscriber starts just behind the open transaction, so what is
// under test is this interleaving rather than the walk down a stream the
// whole package shares. The two cases are the same interleaving on either
// side of the open transaction's first write, which is what tells the two
// bounds of the poll apart: before it the transaction has no id to hold
// the snapshot horizon back, and only its start time stands in front of
// the checkpoint.
func TestRegistryNeverStepsPastAnOpenProducerTransaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// appendsFirst says whether the open transaction appends before
		// the tick that must not step over it, or only afterwards.
		appendsFirst bool
	}{
		{name: "the transaction has already appended", appendsFirst: true},
		{name: "the transaction appends only afterwards", appendsFirst: false},
	}

	for _, tt := range tests {
		// The cases run one after another rather than in parallel: each
		// holds a real transaction open on the database the whole package
		// shares, which holds every other subscriber's checkpoint back for
		// as long as it lasts, and each case proves its own bound just as
		// well on its own.
		t.Run(tt.name, func(t *testing.T) {
			pool := dispatcherDB(t)
			ctx := context.Background()

			w, err := events.NewWriter("cashback")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			suffix := randomSuffix(t)
			eventType := "cashback.durinflight" + suffix + ".tested"
			subscriberName := "durinflight-" + suffix
			subject := uuid.New()

			// The slow producer: a transaction that starts - fixing the
			// occurred_at of everything it will ever append - and stays
			// open, which is what the outbox asks of a producer while the
			// state change its event describes is still being written.
			slow, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the producer transaction: %v", err)
			}
			t.Cleanup(func() { _ = slow.Rollback(ctx) })
			var startedAt time.Time
			if err := slow.QueryRow(ctx, "select now()").Scan(&startedAt); err != nil {
				t.Fatalf("read the producer transaction's start time: %v", err)
			}
			appendSlow := func() events.Event {
				t.Helper()
				e, err := w.Append(ctx, slow, events.Message{
					Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 1}`),
				})
				if err != nil {
					t.Fatalf("appending inside the open transaction: %v", err)
				}
				return e
			}

			var early events.Event
			if tt.appendsFirst {
				early = appendSlow()
			}

			// The quick producer, on its own connection, appending and
			// committing while the slow one is still open. Its event is
			// readable, and stands behind the slow one in the stream.
			time.Sleep(2 * time.Millisecond)
			late := appendSeeds(t, pool, w, []events.Message{
				{Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 2}`)},
			})[0]
			if !startedAt.Before(late.OccurredAt) {
				t.Fatalf("the premise does not hold: the open transaction started at %s, not before the committed event at %s",
					startedAt, late.OccurredAt)
			}

			// The subscriber's saved position, one instant behind the open
			// transaction. Everything this test is about lies after it.
			store, err := events.NewPostgresCheckpoints(pool, subscriberName)
			if err != nil {
				t.Fatalf("NewPostgresCheckpoints: %v", err)
			}
			if err := store.Save(ctx, events.Checkpoint{OccurredAt: startedAt.Add(-time.Microsecond)}); err != nil {
				t.Fatalf("seeding the durable checkpoint: %v", err)
			}

			var delivered []uuid.UUID
			r := events.NewRegistry(pool, events.RegistryConfig{})
			if err := r.Subscribe(subscriberName, []string{eventType}, func(_ context.Context, e events.Event) error {
				delivered = append(delivered, e.EventID)
				return nil
			}); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			// The ticks that see only the committed event. Delivering it
			// is right - it is committed data - but saving a position over
			// it is not, while an append can still land in front of it.
			drainRegistry(t, r, func() bool { return len(delivered) > 0 })
			if len(delivered) != 1 || delivered[0] != late.EventID {
				t.Fatalf("the registry delivered %v, want only the committed event %s", delivered, late.EventID)
			}
			cp, err := store.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			// Reported, not fatal, so that the run goes on to show what
			// saving it costs: the event that is lost, not just the
			// position that was wrong.
			if !cp.OccurredAt.Before(startedAt) {
				t.Errorf("the durable checkpoint %+v stands at or past %s, where the open transaction's append lands; no poll of this subscriber's will read that position again",
					cp, startedAt)
			}

			if !tt.appendsFirst {
				early = appendSlow()
			}
			if !streamLess(early, late) {
				t.Fatalf("the premise does not hold: event %s at %s does not stand before event %s at %s",
					early.EventID, early.OccurredAt, late.EventID, late.OccurredAt)
			}

			// The slow producer commits. Its event is now readable, at a
			// position the poll has already read past once - so the ticks
			// that follow have to find it there.
			if err := slow.Commit(ctx); err != nil {
				t.Fatalf("commit the producer transaction: %v", err)
			}
			count := func(id uuid.UUID) int {
				n := 0
				for _, got := range delivered {
					if got == id {
						n++
					}
				}
				return n
			}
			for i := 0; count(early.EventID) == 0; i++ {
				if i == 500 {
					t.Fatalf("event %s was committed and never delivered: the checkpoint had stepped over the position it landed in, so no poll looks there again (delivered %v)",
						early.EventID, delivered)
				}
				if err := r.Tick(ctx); err != nil {
					t.Fatalf("tick after the commit: %v", err)
				}
				if count(early.EventID) == 0 {
					time.Sleep(time.Millisecond)
				}
			}

			// The event delivered while the checkpoint waited is not
			// delivered again for the waiting: the ticks in between re-read
			// its position, as they must, and its completion row makes each
			// re-read the recorded no-op it is meant to be.
			if got := count(late.EventID); got != 1 {
				t.Errorf("the committed event was delivered %d time(s) while the checkpoint waited, want exactly 1", got)
			}
			for _, e := range []events.Event{early, late} {
				if _, _, found := deliveryRow(t, pool, subscriberName, e.EventID); !found {
					t.Errorf("event %s was delivered and left no completion row", e.EventID)
				}
			}

			// And once nothing is in flight the position catches up: the
			// checkpoint waits, it does not stop.
			drainRegistry(t, r, func() bool { return checkpointPassed(t, pool, subscriberName, late) })
		})
	}
}

// TestSubscribersAdvanceIndependently covers the registry's isolation
// promise: each named subscriber has its own checkpoint, deliveries and
// dead letters, so one subscriber's poisoned lane leaves another's
// delivery of the very same events untouched.
func TestSubscribersAdvanceIndependently(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.pair" + suffix + ".tested"
	healthyName, failingName := "healthy-"+suffix, "failing-"+suffix
	subject := uuid.New()
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 1}`)},
		{Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 2}`)},
	})
	head, tail := appended[0], appended[1]
	if streamLess(tail, head) {
		head, tail = tail, head
	}

	var healthySeen []uuid.UUID
	failingAttempts := make(map[uuid.UUID]int)
	r := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 2, RetryDelay: time.Millisecond})
	if err := r.Subscribe(healthyName, []string{eventType}, func(_ context.Context, e events.Event) error {
		healthySeen = append(healthySeen, e.EventID)
		return nil
	}); err != nil {
		t.Fatalf("Subscribe %s: %v", healthyName, err)
	}
	if err := r.Subscribe(failingName, []string{eventType}, func(_ context.Context, e events.Event) error {
		failingAttempts[e.EventID]++
		return errors.New("this subscriber cannot digest the type")
	}); err != nil {
		t.Fatalf("Subscribe %s: %v", failingName, err)
	}

	// The healthy subscriber's checkpoint is part of what is being waited
	// for, not something to read the moment the deliveries land: it may
	// only be saved over rows no in-flight append can land in front of, so
	// it catches up a tick or two behind the handler on a busy stream.
	lastErr := tickUntil(t, r, func() bool {
		return len(healthySeen) == 2 && failingAttempts[head.EventID] == 2 &&
			checkpointPassed(t, pool, healthyName, tail)
	})

	if healthySeen[0] != head.EventID || healthySeen[1] != tail.EventID {
		t.Errorf("the healthy subscriber delivered %v, want [%s %s] in stream order", healthySeen, head.EventID, tail.EventID)
	}
	if got := failingAttempts[tail.EventID]; got != 0 {
		t.Errorf("the failing subscriber attempted the held event %d time(s); its parked lane forbids that", got)
	}
	if !checkpointPassed(t, pool, healthyName, tail) {
		t.Error("the healthy subscriber's checkpoint never passed the stream; the failing subscriber held it back")
	}
	if checkpointPassed(t, pool, failingName, tail) {
		t.Error("the failing subscriber's checkpoint passed an event held behind its parked lane")
	}
	if _, _, found := deliveryRow(t, pool, failingName, head.EventID); found {
		t.Error("the failing subscriber recorded a completion for a delivery that never succeeded")
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), failingName) {
		t.Errorf("the tick reported %v; the failing subscriber must be named", lastErr)
	}
	if lastErr != nil && strings.Contains(lastErr.Error(), healthyName) {
		t.Errorf("the tick blamed the healthy subscriber too: %v", lastErr)
	}
}

// TestReplayLeavesIdenticalDeliveryState is the event contract's replay
// obligation, scoped to the tracking tables: replaying the full event log
// - a fresh read from the start of the stream - changes neither a
// completion row, nor a dead letter, nor a handler side effect.
func TestReplayLeavesIdenticalDeliveryState(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.replaydur" + suffix + ".tested"
	subscriberName := "replay-" + suffix
	good, bad := uuid.New(), uuid.New()
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: good, Payload: json.RawMessage(`{"n": 1}`)},
		{Type: eventType, Subject: bad, Payload: json.RawMessage(`{"n": 2}`)},
		{Type: eventType, Subject: good, Payload: json.RawMessage(`{"n": 3}`)},
	})
	last := appended[len(appended)-1]
	for _, e := range appended {
		if streamLess(last, e) {
			last = e
		}
	}

	handled := 0
	r := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 2, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(_ context.Context, e events.Event) error {
		handled++
		if e.Subject == bad {
			return errors.New("the bad subject never digests")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First pass: the good lane delivers, the bad lane parks - a mixed
	// durable state, which is the interesting thing to replay.
	_ = tickUntil(t, r, func() bool { return checkpointPassed(t, pool, subscriberName, last) })
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick after the first pass: %v", err)
	}
	before := deliveryState(t, pool, subscriberName)
	handledBefore := handled
	if len(before) == 0 {
		t.Fatal("the first pass left no tracking rows; the replay would compare nothing")
	}

	// The replay: the checkpoint resets to the start of the stream and
	// every event flows past the subscriber again.
	resetCheckpoint(t, pool, subscriberName)
	_ = tickUntil(t, r, func() bool { return checkpointPassed(t, pool, subscriberName, last) })
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick after the replay: %v", err)
	}

	after := deliveryState(t, pool, subscriberName)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("replaying the event log changed the delivery state:\nbefore: %v\nafter:  %v", before, after)
	}
	if handled != handledBefore {
		t.Errorf("replaying the event log ran handlers %d more time(s); every event was already recorded", handled-handledBefore)
	}
}
