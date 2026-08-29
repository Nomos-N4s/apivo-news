package events_test

// The dispatcher's obligations come from the consumer rules of the event
// contract: per-(type, subject) ordering and nothing more, at-least-once
// delivery absorbed by idempotent handlers, and failure that is loud -
// a lane that halts and pins the checkpoint, never an event that quietly
// disappears.
//
// The integration tests run on the shared scratch database
// (dispatcherDB), where every append commits on its own, because that is
// the dispatcher's real subject matter: committed events whose
// occurred_at advances between appends. Within one appending
// transaction occurred_at would be a single timestamp, so the expected
// order is always computed from the stream order the dispatcher promises
// - (occurred_at, id) - not from the order of Append calls.
//
// One test breaks that pattern on purpose. The checkpoint's hard case is
// a producer transaction that is open while a poll runs, so
// TestDispatcherNeverStepsPastAnOpenProducerTransaction drives two real
// concurrent transactions and holds one of them open across a tick.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// failingCheckpoints satisfies events.CheckpointStore without working, so
// the registration-lifecycle test can start a dispatcher with no database
// at all.
type failingCheckpoints struct{}

func (failingCheckpoints) Load(context.Context) (events.Checkpoint, error) {
	return events.Checkpoint{}, errors.New("no checkpoint store")
}

func (failingCheckpoints) Save(context.Context, events.Checkpoint) error {
	return errors.New("no checkpoint store")
}

func TestMemoryCheckpointsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := events.NewMemoryCheckpoints()

	empty, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load on an empty store: %v", err)
	}
	if !empty.OccurredAt.IsZero() || empty.EventID != uuid.Nil {
		t.Fatalf("an empty store reports %+v, want the start of the stream", empty)
	}

	saved := events.Checkpoint{OccurredAt: time.Now(), EventID: uuid.New()}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.OccurredAt.Equal(saved.OccurredAt) || got.EventID != saved.EventID {
		t.Fatalf("Load returned %+v, want %+v", got, saved)
	}
}

func TestRegisterValidates(t *testing.T) {
	t.Parallel()

	handler := func(context.Context, events.Event) error { return nil }
	tests := []struct {
		name      string
		eventType string
		handler   events.Handler
	}{
		{name: "an empty type", eventType: "", handler: handler},
		{name: "a blank type", eventType: "   ", handler: handler},
		{name: "a nil handler", eventType: "cashback.entry.created", handler: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := events.NewDispatcher(nil, events.NewMemoryCheckpoints(), events.DispatcherConfig{})
			if err := d.Register(tt.eventType, tt.handler); err == nil {
				t.Fatal("Register accepted a subscription that could never run")
			}
		})
	}
}

// TestRegisterClosesAtTheFirstTick pins the registration lifecycle: a
// handler added after delivery has started would quietly see only the
// events that happen to come later, so the late call must fail instead.
func TestRegisterClosesAtTheFirstTick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler := func(context.Context, events.Event) error { return nil }

	d := events.NewDispatcher(nil, failingCheckpoints{}, events.DispatcherConfig{})
	if err := d.Register("cashback.entry.created", handler); err != nil {
		t.Fatalf("Register before the first tick: %v", err)
	}
	if err := d.Tick(ctx); err == nil {
		t.Fatal("Tick with a failing checkpoint store: want an error")
	}
	err := d.Register("cashback.entry.reversed", handler)
	if !errors.Is(err, events.ErrStarted) {
		t.Fatalf("Register after the first tick: got %v, want ErrStarted", err)
	}
}

// streamLess is the stream order the dispatcher promises: occurred_at
// first, then the id bytes - the same tuple comparison Postgres applies
// to (occurred_at, id).
func streamLess(a, b events.Event) bool {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		return a.OccurredAt.Before(b.OccurredAt)
	}
	return bytes.Compare(a.EventID[:], b.EventID[:]) < 0
}

// checkpointBefore reports whether cp stands strictly before the event's
// stream position.
func checkpointBefore(cp events.Checkpoint, e events.Event) bool {
	if !cp.OccurredAt.Equal(e.OccurredAt) {
		return cp.OccurredAt.Before(e.OccurredAt)
	}
	cpID, eID := cp.EventID, e.EventID
	return bytes.Compare(cpID[:], eID[:]) < 0
}

// appendSeeds appends one committed event per seed through the pool and
// returns the completed envelopes.
func appendSeeds(t *testing.T, pool *pgxpool.Pool, w *events.Writer, seeds []events.Message) []events.Event {
	t.Helper()
	appended := make([]events.Event, 0, len(seeds))
	for _, msg := range seeds {
		e, err := w.Append(context.Background(), pool, msg)
		if err != nil {
			t.Fatalf("seeding %s: %v", msg.Type, err)
		}
		appended = append(appended, e)
	}
	return appended
}

// drainUntil ticks the dispatcher until done reports true, failing the
// test if a tick errors or the wait's budget runs out first.
//
// The ticks are paced rather than spun, because a tick may legitimately
// make no progress: the checkpoint waits behind any transaction that was
// already open when an event was appended, and that transaction can
// belong to another suite - or to another database on the same cluster
// entirely. tickWait carries the budget and the reason it is measured in
// seconds rather than in ticks.
func drainUntil(t *testing.T, d *events.Dispatcher, done func() bool) {
	t.Helper()
	w := newTickWait()
	for {
		if done() {
			return
		}
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if done() {
			return
		}
		if !w.next(t) {
			t.Fatalf("the dispatcher did not finish: %s", w.reason())
		}
	}
}

// TestDispatcherDeliversInOrderPerLane covers the ordering half of the
// contract: within each (type, subject) lane events arrive in stream
// order, exactly once per tick pass, while types nobody subscribed to are
// passed over without holding the checkpoint back.
func TestDispatcherDeliversInOrderPerLane(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	typeOne := "cashback.lane" + suffix + ".tested"
	typeTwo := "cashback.other" + suffix + ".tested"
	typeUnhandled := "cashback.nobody" + suffix + ".listened"
	subjectA, subjectB := uuid.New(), uuid.New()

	payload := func(n int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"n": %d}`, n))
	}
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: typeOne, Subject: subjectA, Payload: payload(1)},
		{Type: typeOne, Subject: subjectB, Payload: payload(1)},
		{Type: typeTwo, Subject: subjectA, Payload: payload(1)},
		{Type: typeUnhandled, Subject: subjectA, Payload: payload(1)},
		{Type: typeOne, Subject: subjectA, Payload: payload(2)},
		{Type: typeOne, Subject: subjectB, Payload: payload(2)},
		{Type: typeTwo, Subject: subjectA, Payload: payload(2)},
		{Type: typeOne, Subject: subjectA, Payload: payload(3)},
	})
	const handled = 7 // everything but the typeUnhandled event

	var delivered []events.Event
	record := func(_ context.Context, e events.Event) error {
		delivered = append(delivered, e)
		return nil
	}
	store := events.NewMemoryCheckpoints()
	// A batch size smaller than the seed forces the run across several
	// ticks, so the checkpoint is exercised between polls, not only at
	// the end.
	d := events.NewDispatcher(pool, store, events.DispatcherConfig{BatchSize: 3})
	for _, eventType := range []string{typeOne, typeTwo} {
		if err := d.Register(eventType, record); err != nil {
			t.Fatalf("Register %s: %v", eventType, err)
		}
	}

	// The last event in stream order, needed before the drain rather than
	// after it: the drain waits for the checkpoint to reach it, not only
	// for the deliveries. A position is saved only over rows no append
	// still in flight can land in front of, so on a database several
	// suites are appending to at once it arrives a tick or two behind the
	// handler.
	last := appended[0]
	for _, e := range appended {
		if streamLess(last, e) {
			last = e
		}
	}
	drainUntil(t, d, func() bool {
		if len(delivered) < handled {
			return false
		}
		cp, err := store.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return !checkpointBefore(cp, last)
	})

	// Another tick delivers nothing new: everything is behind the
	// checkpoint now.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick after the drain: %v", err)
	}
	if len(delivered) != handled {
		t.Fatalf("delivered %d events, want exactly %d", len(delivered), handled)
	}
	for _, e := range delivered {
		if e.Type == typeUnhandled {
			t.Fatalf("an event of %s was delivered; nothing subscribed to it", typeUnhandled)
		}
	}

	// Per lane, the delivered order must be the stream order.
	wantByLane := make(map[events.Lane][]uuid.UUID)
	sorted := make([]events.Event, len(appended))
	copy(sorted, appended)
	sort.Slice(sorted, func(i, j int) bool { return streamLess(sorted[i], sorted[j]) })
	for _, e := range sorted {
		if e.Type == typeUnhandled {
			continue
		}
		lane := events.Lane{Type: e.Type, Subject: e.Subject}
		wantByLane[lane] = append(wantByLane[lane], e.EventID)
	}
	gotByLane := make(map[events.Lane][]uuid.UUID)
	for _, e := range delivered {
		lane := events.Lane{Type: e.Type, Subject: e.Subject}
		gotByLane[lane] = append(gotByLane[lane], e.EventID)
	}
	for lane, want := range wantByLane {
		got := gotByLane[lane]
		if len(got) != len(want) {
			t.Fatalf("lane %s: delivered %d events, want %d", lane, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("lane %s: position %d delivered %s, want %s - not stream order", lane, i, got[i], want[i])
			}
		}
	}

	// The checkpoint moved past everything, the unhandled event included -
	// which is what the drain above waited for, re-read here so a later
	// tick cannot have walked it back.
	cp, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpointBefore(cp, last) {
		t.Fatalf("the checkpoint %+v never passed the last event %s", cp, last.EventID)
	}

	// The delivered envelope is the appended one, payload included.
	byID := make(map[uuid.UUID]events.Event, len(appended))
	for _, e := range appended {
		byID[e.EventID] = e
	}
	for _, got := range delivered {
		want, known := byID[got.EventID]
		if !known {
			t.Fatalf("event %s was delivered but never appended", got.EventID)
		}
		if got.Type != want.Type || got.Version != want.Version ||
			got.Producer != want.Producer || got.Subject != want.Subject ||
			got.IdempotencyKey != want.IdempotencyKey || !got.OccurredAt.Equal(want.OccurredAt) {
			t.Fatalf("event %s arrived with envelope %+v, want %+v", got.EventID, got, want)
		}
		var gotPayload, wantPayload map[string]any
		if err := json.Unmarshal(got.Payload, &gotPayload); err != nil {
			t.Fatalf("delivered payload is not JSON: %v", err)
		}
		if err := json.Unmarshal(want.Payload, &wantPayload); err != nil {
			t.Fatalf("appended payload is not JSON: %v", err)
		}
		if gotPayload["n"] != wantPayload["n"] {
			t.Fatalf("event %s arrived with payload %s, want %s", got.EventID, got.Payload, want.Payload)
		}
	}
}

// TestDispatcherNeverStepsPastAnOpenProducerTransaction is the no-skip
// guarantee, driven by the interleaving that breaks a checkpoint trusting
// occurred_at alone.
//
// occurred_at is the appending transaction's start time, and a row is
// readable only once that transaction commits. So an event appended by a
// transaction that stays open stands EARLIER in the stream than
// everything appended and committed while it is open, and appears LATER
// than all of it. A checkpoint that moved over the readable events would
// leave that one behind the read position for good: a committed event
// silently dropped, which consumer rule 4 of the event contract calls a
// defect, and which for a credit event means a subscriber that never
// hears about the money.
//
// The two cases are the same interleaving on either side of the open
// transaction's first write, and the checkpoint has to survive both: an
// append it has already made is one the poll cannot see, and an append it
// has not made yet still lands at the start time it already took.
func TestDispatcherNeverStepsPastAnOpenProducerTransaction(t *testing.T) {
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
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := dispatcherDB(t)
			ctx := context.Background()

			w, err := events.NewWriter("cashback")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			eventType := "cashback.inflight" + randomSuffix(t) + ".tested"
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

			var delivered []uuid.UUID
			record := func(_ context.Context, e events.Event) error {
				delivered = append(delivered, e.EventID)
				return nil
			}
			store := events.NewMemoryCheckpoints()
			d := events.NewDispatcher(pool, store, events.DispatcherConfig{})
			if err := d.Register(eventType, record); err != nil {
				t.Fatalf("Register: %v", err)
			}

			// The tick that sees only the committed event. Delivering it
			// is right - it is committed data - but saving a checkpoint
			// over it is not, while an append can still land in front of
			// it.
			if err := d.Tick(ctx); err != nil {
				t.Fatalf("tick while the producer transaction is open: %v", err)
			}
			if len(delivered) != 1 || delivered[0] != late.EventID {
				t.Fatalf("the tick delivered %v, want only the committed event %s", delivered, late.EventID)
			}
			cp, err := store.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			// Reported, not fatal, so that the run goes on to show what
			// saving it costs: the event that is lost, not just the
			// position that was wrong.
			if !cp.OccurredAt.Before(startedAt) {
				t.Errorf("the checkpoint %+v stands at or past %s, where the open transaction's append lands; nothing will ever read that position again",
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
			// The no-skip assertion itself. Nothing about it is on a
			// horizon: the event is committed and stands after a
			// checkpoint that never moved, so one poll finds it. The
			// budget is only here to end the run when it does not.
			arrived := func() bool { return count(early.EventID) > 0 }
			for w := newTickWait(); !arrived(); {
				if err := d.Tick(ctx); err != nil {
					t.Fatalf("tick after the commit: %v", err)
				}
				if !arrived() && !w.next(t) {
					t.Fatalf("event %s was committed and never delivered (%s): the checkpoint had stepped over the position it landed in, so no poll looks there again (delivered %v)",
						early.EventID, w.reason(), delivered)
				}
			}

			// And the event delivered while the checkpoint waited is not
			// delivered again for the waiting: the ticks in between re-read
			// its position, as they must, and pass over it.
			if got := count(late.EventID); got != 1 {
				t.Errorf("the committed event was delivered %d time(s) while the checkpoint waited, want exactly 1", got)
			}
		})
	}
}

// TestDispatcherRetriesWithinTheBudget covers the recoverable half of
// failure: a delivery that fails and then succeeds inside its budget is
// simply delivered, with no halt and no error.
func TestDispatcherRetriesWithinTheBudget(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	eventType := "cashback.retry" + randomSuffix(t) + ".tested"
	appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})

	attempts, deliveries := 0, 0
	handler := func(context.Context, events.Event) error {
		attempts++
		if attempts == 1 {
			return errors.New("a transient failure")
		}
		deliveries++
		return nil
	}
	var halts []events.Halt
	d := events.NewDispatcher(pool, events.NewMemoryCheckpoints(), events.DispatcherConfig{
		MaxAttempts: 3,
		RetryDelay:  time.Millisecond,
		OnHalt:      func(h events.Halt) { halts = append(halts, h) },
	})
	if err := d.Register(eventType, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}

	drainUntil(t, d, func() bool { return deliveries == 1 })
	if attempts != 2 {
		t.Errorf("the delivery took %d attempts, want 2 - one failure, one success", attempts)
	}
	if len(halts) != 0 {
		t.Errorf("a delivery that recovered inside its budget halted its lane: %+v", halts)
	}
}

// TestDispatcherHaltsALaneWithoutBlockingOthers covers failure rules 3
// and 4 of the contract together: a delivery that spends its retry budget
// halts its own (type, subject) lane - later events on it are held back,
// the halt is loud on every tick, and the checkpoint pins before the
// failed event - while another subject of the same type keeps flowing.
// A fresh dispatcher over the same checkpoint - a process restart - then
// retries the halted delivery and drains the lane in order.
func TestDispatcherHaltsALaneWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	eventType := "cashback.halt" + randomSuffix(t) + ".tested"
	bad, good := uuid.New(), uuid.New()
	payload := func(n int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"n": %d}`, n))
	}
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: bad, Payload: payload(1)},
		{Type: eventType, Subject: good, Payload: payload(1)},
		{Type: eventType, Subject: bad, Payload: payload(2)},
		{Type: eventType, Subject: good, Payload: payload(2)},
	})
	// The two bad-lane events in stream order: the head is the one that
	// must halt the lane, the tail the one that must be held back.
	badHead, badTail := appended[0], appended[2]
	if streamLess(badTail, badHead) {
		badHead, badTail = badTail, badHead
	}

	attempts := make(map[uuid.UUID]int)
	var goodSeen []int
	handler := func(_ context.Context, e events.Event) error {
		attempts[e.EventID]++
		if e.Subject == bad {
			return errors.New("this handler cannot digest the subject")
		}
		var p struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		goodSeen = append(goodSeen, p.N)
		return nil
	}
	var halts []events.Halt
	store := events.NewMemoryCheckpoints()
	d := events.NewDispatcher(pool, store, events.DispatcherConfig{
		MaxAttempts: 2,
		RetryDelay:  time.Millisecond,
		OnHalt:      func(h events.Halt) { halts = append(halts, h) },
	})
	if err := d.Register(eventType, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Tick until the good lane drained; the halting ticks return errors,
	// which is their job, so they are collected rather than fatal.
	var lastErr error
	for i := 0; len(goodSeen) < 2; i++ {
		if i > 200 {
			t.Fatalf("the good lane never drained: delivered %v", goodSeen)
		}
		lastErr = d.Tick(ctx)
	}

	if got := attempts[badHead.EventID]; got != 2 {
		t.Errorf("the halting delivery was attempted %d time(s), want exactly the budget of 2", got)
	}
	if got := attempts[badTail.EventID]; got != 0 {
		t.Errorf("an event behind the halted head was attempted %d time(s); per-lane ordering forbids delivering it at all", got)
	}
	if len(goodSeen) != 2 || goodSeen[0] != 1 || goodSeen[1] != 2 {
		t.Errorf("the healthy lane delivered %v, want [1 2] - it must not be blocked by the halted one", goodSeen)
	}
	if len(halts) != 1 {
		t.Fatalf("OnHalt fired %d time(s), want exactly once", len(halts))
	}
	halt := halts[0]
	if halt.Lane != (events.Lane{Type: eventType, Subject: bad}) {
		t.Errorf("the halt names lane %s, want (%s, %s)", halt.Lane, eventType, bad)
	}
	if halt.Event.EventID != badHead.EventID {
		t.Errorf("the halt names event %s, want the lane head %s", halt.Event.EventID, badHead.EventID)
	}
	if halt.Attempts != 2 || halt.Err == nil || !strings.Contains(halt.Err.Error(), "digest") {
		t.Errorf("the halt reports %d attempt(s) and error %v; want 2 and the handler's error", halt.Attempts, halt.Err)
	}

	// The failure stays loud: this tick and every later one report the
	// halted lane, and nothing on it is retried within this process.
	if lastErr == nil || !strings.Contains(lastErr.Error(), "halted") {
		t.Errorf("a tick with a halted lane returned %v; the halt must be loud", lastErr)
	}
	if err := d.Tick(ctx); err == nil || !strings.Contains(err.Error(), "halted") {
		t.Errorf("a later tick returned %v; a halted lane must stay loud until resolved", err)
	}
	if got := attempts[badHead.EventID]; got != 2 {
		t.Errorf("the halted delivery was retried within the process: %d attempt(s), want the budget of 2", got)
	}

	// The checkpoint pins strictly before the halted event: nothing is
	// dropped, and a restart will pick it up again.
	cp, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !checkpointBefore(cp, badHead) {
		t.Fatalf("the checkpoint %+v moved past the halted event %s; the failed delivery would be lost", cp, badHead.EventID)
	}

	// The restart: a fresh dispatcher, the same checkpoint, a handler
	// that now succeeds. The bad lane drains in order; the good lane's
	// redelivery is the at-least-once posture, absorbed by idempotence.
	var badSeen []int
	seen := make(map[uuid.UUID]bool)
	recovered := func(_ context.Context, e events.Event) error {
		if seen[e.EventID] {
			return nil
		}
		seen[e.EventID] = true
		if e.Subject != bad {
			return nil
		}
		var p struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		badSeen = append(badSeen, p.N)
		return nil
	}
	restarted := events.NewDispatcher(pool, store, events.DispatcherConfig{})
	if err := restarted.Register(eventType, recovered); err != nil {
		t.Fatalf("Register on the restarted dispatcher: %v", err)
	}
	drainUntil(t, restarted, func() bool { return len(badSeen) == 2 })
	if badSeen[0] != 1 || badSeen[1] != 2 {
		t.Errorf("after the restart the halted lane delivered %v, want [1 2]", badSeen)
	}
}

// TestDispatcherRedeliveryAfterCheckpointResetIsANoOp demonstrates the
// at-least-once posture end to end: losing the checkpoint replays the
// stream, and an idempotent handler - one that records processed ids, as
// consumer rule 1 requires - absorbs the replay with identical state.
func TestDispatcherRedeliveryAfterCheckpointResetIsANoOp(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	eventType := "cashback.replay" + randomSuffix(t) + ".tested"
	subject := uuid.New()
	seeds := make([]events.Message, 0, 3)
	for n := 1; n <= 3; n++ {
		seeds = append(seeds, events.Message{
			Type:    eventType,
			Subject: subject,
			Payload: json.RawMessage(fmt.Sprintf(`{"n": %d}`, n)),
		})
	}
	appendSeeds(t, pool, w, seeds)

	// The handler's state: a sum applied once per event id. The processed
	// map is the idempotence; the sum is what must not change on replay.
	processed := make(map[uuid.UUID]int)
	sum := 0
	handler := func(_ context.Context, e events.Event) error {
		processed[e.EventID]++
		if processed[e.EventID] > 1 {
			return nil // a redelivery is a no-op
		}
		var p struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		sum += p.N
		return nil
	}
	deliveredOnce := func() bool {
		if len(processed) != 3 {
			return false
		}
		for _, n := range processed {
			if n < 1 {
				return false
			}
		}
		return true
	}

	first := events.NewDispatcher(pool, events.NewMemoryCheckpoints(), events.DispatcherConfig{})
	if err := first.Register(eventType, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	drainUntil(t, first, deliveredOnce)
	if sum != 6 {
		t.Fatalf("after first delivery sum = %d, want 6", sum)
	}

	// The reset: a fresh checkpoint store is exactly what a restart with
	// MemoryCheckpoints produces. The whole stream replays.
	redelivered := func() bool {
		for _, n := range processed {
			if n < 2 {
				return false
			}
		}
		return true
	}
	replayer := events.NewDispatcher(pool, events.NewMemoryCheckpoints(), events.DispatcherConfig{})
	if err := replayer.Register(eventType, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}
	drainUntil(t, replayer, redelivered)

	if sum != 6 {
		t.Fatalf("replaying the stream changed the state: sum = %d, want still 6", sum)
	}
	for id, n := range processed {
		if n != 2 {
			t.Errorf("event %s was handled %d time(s) across the two runs, want exactly 2", id, n)
		}
	}
}

// TestDispatcherRecoversAHandlerPanic pins the failure containment: a
// panicking handler is a failed delivery - its lane halts and the panic
// value is in the report - never the end of the process.
func TestDispatcherRecoversAHandlerPanic(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	eventType := "cashback.panic" + randomSuffix(t) + ".tested"
	appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})

	handler := func(context.Context, events.Event) error {
		panic("a defect in the handler")
	}
	var halts []events.Halt
	d := events.NewDispatcher(pool, events.NewMemoryCheckpoints(), events.DispatcherConfig{
		MaxAttempts: 1,
		OnHalt:      func(h events.Halt) { halts = append(halts, h) },
	})
	if err := d.Register(eventType, handler); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var tickErr error
	for i := 0; len(halts) == 0; i++ {
		if i > 200 {
			t.Fatal("the panicking delivery never halted its lane")
		}
		tickErr = d.Tick(ctx)
	}
	if tickErr == nil || !strings.Contains(tickErr.Error(), "panicked") {
		t.Errorf("the tick reported %v, want the recovered panic", tickErr)
	}
	if halts[0].Err == nil || !strings.Contains(halts[0].Err.Error(), "a defect in the handler") {
		t.Errorf("the halt reports %v, want the panic value", halts[0].Err)
	}
}
