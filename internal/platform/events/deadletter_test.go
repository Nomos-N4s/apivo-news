package events_test

// The dead-letter suites pin consumer rule 4 in its durable form: a
// delivery that spends its retry budget is parked where an operator queue
// can surface it, its own (type, subject) lane blocks so the subject's
// history cannot reorder, every other lane keeps flowing, and an operator
// requeue - and nothing less - resumes the lane, in order.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// findDeadLetter returns the queue row for one (subscriber, event), or
// false when the queue does not hold it.
func findDeadLetter(t *testing.T, pool *pgxpool.Pool, subscriber string, eventID uuid.UUID) (events.DeadLetter, bool) {
	t.Helper()
	letters, err := events.ListDeadLetters(context.Background(), pool, subscriber)
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	for _, dl := range letters {
		if dl.Event.EventID == eventID {
			return dl, true
		}
	}
	return events.DeadLetter{}, false
}

// TestDeadLetterParksAfterBudgetAndBlocksItsLane covers the parking
// decision end to end: the head of a poisoned lane is parked after its
// budget - durably, so a restarted process does not retry it - the events
// behind it on the lane are held rather than delivered out of order, and
// every other lane keeps flowing.
func TestDeadLetterParksAfterBudgetAndBlocksItsLane(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.park" + suffix + ".tested"
	subscriberName := "park-" + suffix
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
	var parked []events.DeadLetter
	r := events.NewRegistry(pool, events.RegistryConfig{
		MaxAttempts:  2,
		RetryDelay:   time.Millisecond,
		OnDeadLetter: func(dl events.DeadLetter) { parked = append(parked, dl) },
	})
	if err := r.Subscribe(subscriberName, []string{eventType}, handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The checkpoint passing the parked head is waited for rather than
	// read off the tick that parks it: a position is only ever saved over
	// rows no in-flight append can land in front of, so on a busy stream
	// it follows the parking by a tick or two.
	lastErr := tickUntil(t, r, func() bool {
		return len(goodSeen) == 2 && len(parked) == 1 &&
			checkpointPassed(t, pool, subscriberName, badHead)
	})

	if got := attempts[badHead.EventID]; got != 2 {
		t.Errorf("the parked delivery was attempted %d time(s), want exactly the budget of 2", got)
	}
	if got := attempts[badTail.EventID]; got != 0 {
		t.Errorf("an event behind the parked head was attempted %d time(s); per-lane ordering forbids delivering it at all", got)
	}
	if goodSeen[0] != 1 || goodSeen[1] != 2 {
		t.Errorf("the healthy lane delivered %v, want [1 2] - it must not be blocked by the parked one", goodSeen)
	}

	// The callback carries the whole story: who, what, how hard, and why.
	dl := parked[0]
	if dl.Subscriber != subscriberName || dl.Event.EventID != badHead.EventID {
		t.Errorf("OnDeadLetter reported subscriber %q event %s, want %q %s", dl.Subscriber, dl.Event.EventID, subscriberName, badHead.EventID)
	}
	if dl.Attempts != 2 || !strings.Contains(dl.LastError, "digest") {
		t.Errorf("OnDeadLetter reported %d attempt(s) and error %q; want 2 and the handler's error", dl.Attempts, dl.LastError)
	}
	if dl.ParkedAt.IsZero() || !dl.RequeuedAt.IsZero() {
		t.Errorf("OnDeadLetter reported parked_at %v requeued_at %v; a fresh dead letter is parked, never requeued", dl.ParkedAt, dl.RequeuedAt)
	}

	// The operator queue surfaces it - scoped to the subscriber and in the
	// list across all subscribers alike.
	row, found := findDeadLetter(t, pool, subscriberName, badHead.EventID)
	if !found {
		t.Fatal("the parked delivery is not in the operator queue")
	}
	if row.Attempts != 2 || !strings.Contains(row.LastError, "digest") || !row.RequeuedAt.IsZero() {
		t.Errorf("the queue row reads %+v; want 2 attempts, the handler's error, and no requeue mark", row)
	}
	everyone, err := events.ListDeadLetters(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListDeadLetters across all subscribers: %v", err)
	}
	sighted := false
	for _, letter := range everyone {
		if letter.Subscriber == subscriberName && letter.Event.EventID == badHead.EventID {
			sighted = true
		}
	}
	if !sighted {
		t.Error("the unscoped operator queue does not surface the parked delivery")
	}

	// The tick stays loud while the lane holds events back, and the
	// checkpoint pins before the held event so it is preserved by
	// position.
	if lastErr == nil || !strings.Contains(lastErr.Error(), "parked") {
		t.Errorf("a tick with a parked lane returned %v; the failure must be loud", lastErr)
	}
	if err := r.Tick(ctx); err == nil || !strings.Contains(err.Error(), "holds back") {
		t.Errorf("a later tick returned %v; a lane holding events back must stay loud", err)
	}
	if checkpointPassed(t, pool, subscriberName, badTail) {
		t.Error("the checkpoint moved past an event held behind the parked head; the held delivery would be lost")
	}
	if !checkpointPassed(t, pool, subscriberName, badHead) {
		t.Error("the checkpoint never passed the parked head; a parked delivery is durably recorded and must not pin the stream")
	}

	// The restart: a fresh registry over the same durable state. Nothing
	// runs - the good lane is recorded, the bad head is parked (not
	// retried, unlike an in-memory halt), and the bad tail stays held.
	restartedCalls := 0
	restarted := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 2, RetryDelay: time.Millisecond})
	if err := restarted.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		restartedCalls++
		return nil
	}); err != nil {
		t.Fatalf("Subscribe on the restarted registry: %v", err)
	}
	if err := restarted.Tick(ctx); err == nil || !strings.Contains(err.Error(), "holds back") {
		t.Errorf("the restarted registry's tick returned %v; the parked lane must stay loud across restarts", err)
	}
	if restartedCalls != 0 {
		t.Errorf("the restarted registry ran the handler %d time(s); every event is either recorded or held", restartedCalls)
	}
}

// TestRequeueRedeliversInOrder covers the operator's half of the
// dead-letter design: a requeue while the handler still fails re-parks
// with the attempts accumulated, and a requeue once the handler heals
// delivers the parked head first and only then the events held behind it
// - the lane resumes in stream order.
func TestRequeueRedeliversInOrder(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.requeue" + suffix + ".tested"
	subscriberName := "requeue-" + suffix
	subject := uuid.New()
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 1}`)},
		{Type: eventType, Subject: subject, Payload: json.RawMessage(`{"n": 2}`)},
	})
	head, tail := appended[0], appended[1]
	if streamLess(tail, head) {
		head, tail = tail, head
	}

	healthy := false
	var delivered []uuid.UUID
	handlerCalls := make(map[uuid.UUID]int)
	handler := func(_ context.Context, e events.Event) error {
		handlerCalls[e.EventID]++
		if !healthy {
			return errors.New("still failing")
		}
		delivered = append(delivered, e.EventID)
		return nil
	}
	var parked []events.DeadLetter
	r := events.NewRegistry(pool, events.RegistryConfig{
		MaxAttempts:  2,
		RetryDelay:   time.Millisecond,
		OnDeadLetter: func(dl events.DeadLetter) { parked = append(parked, dl) },
	})
	if err := r.Subscribe(subscriberName, []string{eventType}, handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Park the head.
	_ = tickUntil(t, r, func() bool { return len(parked) == 1 })
	if got := handlerCalls[head.EventID]; got != 2 {
		t.Fatalf("the head was attempted %d time(s) before parking, want 2", got)
	}

	// A requeue while the handler still fails: the redelivery gets a
	// fresh budget, spends it, and the row parks again with the attempts
	// accumulated. Requeue is idempotent, so an operator retrying the
	// call changes nothing.
	if err := events.Requeue(ctx, pool, subscriberName, head.EventID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if err := events.Requeue(ctx, pool, subscriberName, head.EventID); err != nil {
		t.Fatalf("a second Requeue of the same row: %v", err)
	}
	err = r.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "parked again") {
		t.Errorf("the tick after a failing requeue returned %v; the re-parking must be loud", err)
	}
	if got := handlerCalls[head.EventID]; got != 4 {
		t.Errorf("the head has been attempted %d time(s) after the failed requeue, want 4 - the old budget plus a fresh one", got)
	}
	if len(parked) != 2 {
		t.Fatalf("OnDeadLetter fired %d time(s), want 2 - once per parking", len(parked))
	}
	if parked[1].Attempts != 4 {
		t.Errorf("the re-parked delivery reports %d attempt(s), want the accumulated 4", parked[1].Attempts)
	}
	row, found := findDeadLetter(t, pool, subscriberName, head.EventID)
	if !found {
		t.Fatal("the re-parked delivery left the operator queue")
	}
	if row.Attempts != 4 || !row.RequeuedAt.IsZero() {
		t.Errorf("the queue row reads attempts=%d requeued_at=%v; want the accumulated 4 and the requeue mark cleared", row.Attempts, row.RequeuedAt)
	}
	if got := handlerCalls[tail.EventID]; got != 0 {
		t.Errorf("the held event was attempted %d time(s) while its lane head sat parked", got)
	}

	// The handler heals and the operator requeues once more: the lane
	// resumes - head first, then the event held behind it, in order.
	healthy = true
	if err := events.Requeue(ctx, pool, subscriberName, head.EventID); err != nil {
		t.Fatalf("Requeue after the fix: %v", err)
	}
	drainRegistry(t, r, func() bool { return len(delivered) == 2 })

	if delivered[0] != head.EventID || delivered[1] != tail.EventID {
		t.Errorf("the resumed lane delivered %v, want [%s %s] - stream order, head first", delivered, head.EventID, tail.EventID)
	}
	if _, found := findDeadLetter(t, pool, subscriberName, head.EventID); found {
		t.Error("the resolved delivery is still in the operator queue")
	}
	attempts, _, found := deliveryRow(t, pool, subscriberName, head.EventID)
	if !found {
		t.Fatal("the resolved head has no completion row")
	}
	if attempts != 5 {
		t.Errorf("the head's completion row records %d attempt(s), want 5 - four failures and the one that landed", attempts)
	}
	attempts, _, found = deliveryRow(t, pool, subscriberName, tail.EventID)
	if !found {
		t.Fatal("the resumed tail has no completion row")
	}
	if attempts != 1 {
		t.Errorf("the tail's completion row records %d attempt(s), want 1", attempts)
	}
	if err := r.Tick(ctx); err != nil {
		t.Errorf("a tick after the lane resumed returned %v; the queue is clear and nothing is held", err)
	}
}

// TestRequeueHealsACrashBetweenRecordingAndClearing manufactures the one
// crash window inside resolution: the completion row landed, the process
// died before the queue entry was cleared. A requeue of that leftover
// must recognise the recorded delivery and clear the entry without
// running the handler again.
func TestRequeueHealsACrashBetweenRecordingAndClearing(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.heal" + suffix + ".tested"
	subscriberName := "heal-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	calls := 0
	r := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 1, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		calls++
		return errors.New("failing until parked")
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	_ = tickUntil(t, r, func() bool {
		_, found := findDeadLetter(t, pool, subscriberName, event.EventID)
		return found
	})
	callsWhenParked := calls

	// The crash: the completion row exists beside the queue entry -
	// exactly what dying between recording a redelivery and clearing its
	// dead letter leaves behind.
	if _, err := pool.Exec(ctx,
		`insert into event_delivery (subscriber, event_id, attempts) values ($1, $2, $3)`,
		subscriberName, event.EventID.String(), callsWhenParked); err != nil {
		t.Fatalf("planting the completion row: %v", err)
	}
	if err := events.Requeue(ctx, pool, subscriberName, event.EventID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("the healing tick: %v", err)
	}

	if calls != callsWhenParked {
		t.Errorf("the handler ran %d more time(s) for a delivery already recorded; the requeue must be a recorded no-op", calls-callsWhenParked)
	}
	if _, found := findDeadLetter(t, pool, subscriberName, event.EventID); found {
		t.Error("the leftover queue entry survived the requeue; the recorded delivery should have cleared it")
	}
}

// TestDeadLetterOnASubjectlessLane runs the whole park-and-requeue cycle
// on the lane of a type's subjectless events - the null-subject shape has
// its own storage path - with a handler whose error says nothing, which
// the queue must not pass on as an empty explanation.
func TestDeadLetterOnASubjectlessLane(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.nosubject" + suffix + ".tested"
	subscriberName := "nosubject-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	healthy := false
	delivered := 0
	r := events.NewRegistry(pool, events.RegistryConfig{MaxAttempts: 1, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		if !healthy {
			return errors.New("   ")
		}
		delivered++
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	lastErr := tickUntil(t, r, func() bool {
		_, found := findDeadLetter(t, pool, subscriberName, event.EventID)
		return found
	})
	if lastErr == nil || !strings.Contains(lastErr.Error(), "no subject") {
		t.Errorf("the parking tick reported %v; the subjectless lane must be named as such", lastErr)
	}
	row, _ := findDeadLetter(t, pool, subscriberName, event.EventID)
	if row.Event.Subject != uuid.Nil {
		t.Errorf("the queue reports subject %s for a subjectless event", row.Event.Subject)
	}
	if strings.TrimSpace(row.LastError) == "" {
		t.Error("the queue passed a blank error on to the operator; a failure with no message still has to say so")
	}

	healthy = true
	if err := events.Requeue(ctx, pool, subscriberName, event.EventID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	drainRegistry(t, r, func() bool { return delivered == 1 })
	if _, found := findDeadLetter(t, pool, subscriberName, event.EventID); found {
		t.Error("the resolved subjectless delivery is still in the operator queue")
	}
}

// faultyDB wraps the real pool and fails exactly the statements the test
// names, so the crash windows between a handler's success and its
// bookkeeping can be opened on purpose. failSQL is read and written only
// between ticks, on the test's own goroutine.
type faultyDB struct {
	pool    *pgxpool.Pool
	failSQL *string
}

func (f faultyDB) failing(sql string) bool {
	return *f.failSQL != "" && strings.Contains(sql, *f.failSQL)
}

func (f faultyDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.failing(sql) {
		return nil, errDatabaseDown
	}
	return f.pool.Query(ctx, sql, args...)
}

func (f faultyDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.failing(sql) {
		return brokenRow{}
	}
	return f.pool.QueryRow(ctx, sql, args...)
}

func (f faultyDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.failing(sql) {
		return pgconn.CommandTag{}, errDatabaseDown
	}
	return f.pool.Exec(ctx, sql, args...)
}

// TestLostTrackingWriteRedelivers is the honest exactly-once boundary,
// executed: the handler succeeds, the completion row is lost, and the
// event is therefore delivered AGAIN - the second side effect the
// documentation promises this window can produce, and the reason
// consumer rule 1 still binds every handler to idempotence. What the
// design guarantees instead is that nothing is lost: the checkpoint pins,
// the redelivery comes, and exactly one completion row remains.
func TestLostTrackingWriteRedelivers(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.window" + suffix + ".tested"
	subscriberName := "window-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	failSQL := "insert into event_delivery"
	calls := 0
	r := events.NewRegistry(faultyDB{pool: pool, failSQL: &failSQL}, events.RegistryConfig{MaxAttempts: 1, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	lastErr := tickUntil(t, r, func() bool { return calls == 1 })
	if lastErr == nil || !strings.Contains(lastErr.Error(), "recording the delivery failed") {
		t.Errorf("the tick that lost the tracking write reported %v; the loss must be loud", lastErr)
	}
	if _, _, found := deliveryRow(t, pool, subscriberName, event.EventID); found {
		t.Fatal("a completion row exists although its write was made to fail")
	}
	if checkpointPassed(t, pool, subscriberName, event) {
		t.Fatal("the checkpoint passed a handled-but-unrecorded event; the repairing redelivery would never come")
	}

	// The write path heals; the redelivery arrives; the handler runs a
	// second time. That second run IS the window - at-least-once, honest.
	failSQL = ""
	drainRegistry(t, r, func() bool { return calls == 2 })
	attempts, _, found := deliveryRow(t, pool, subscriberName, event.EventID)
	if !found {
		t.Fatal("the redelivery left no completion row")
	}
	if attempts != 1 {
		t.Errorf("the completion row records %d attempt(s), want 1 - the run that completed, not the one that was lost", attempts)
	}
}

// TestRequeueSurvivesLostWritesStepByStep walks a requeued redelivery
// through every bookkeeping failure in turn - the recorded-check, the
// completion write, the queue clearing - and shows each one is loud,
// leaves the row requeued, and is healed by a later tick, ending with one
// completion row and an empty queue.
func TestRequeueSurvivesLostWritesStepByStep(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	suffix := randomSuffix(t)
	eventType := "cashback.stepwise" + suffix + ".tested"
	subscriberName := "stepwise-" + suffix
	appended := appendSeeds(t, pool, w, []events.Message{
		{Type: eventType, Subject: uuid.New(), Payload: json.RawMessage(`{"n": 1}`)},
	})
	event := appended[0]

	failSQL := ""
	healthy := false
	calls := 0
	r := events.NewRegistry(faultyDB{pool: pool, failSQL: &failSQL}, events.RegistryConfig{MaxAttempts: 1, RetryDelay: time.Millisecond})
	if err := r.Subscribe(subscriberName, []string{eventType}, func(context.Context, events.Event) error {
		calls++
		if !healthy {
			return errors.New("failing until parked")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	_ = tickUntil(t, r, func() bool {
		_, found := findDeadLetter(t, pool, subscriberName, event.EventID)
		return found
	})
	healthy = true
	if err := events.Requeue(ctx, pool, subscriberName, event.EventID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	steps := []struct {
		failSQL   string
		report    string
		moreCalls int
	}{
		// The recorded-check cannot run: nothing is attempted, the row
		// stays requeued for the next tick.
		{failSQL: "select exists", report: "requeued event", moreCalls: 0},
		// The handler runs, the completion write is lost: redelivered.
		{failSQL: "insert into event_delivery", report: "recording the delivery failed", moreCalls: 1},
		// The handler runs again, the completion lands, clearing the
		// queue entry fails: the leftover waits for the heal.
		{failSQL: "delete from event_dead_letter", report: "clearing its dead letter failed", moreCalls: 1},
	}
	for _, step := range steps {
		callsBefore := calls
		failSQL = step.failSQL
		err := r.Tick(ctx)
		if err == nil || !strings.Contains(err.Error(), step.report) {
			t.Fatalf("with %q failing the tick reported %v, want it to mention %q", step.failSQL, err, step.report)
		}
		if calls != callsBefore+step.moreCalls {
			t.Fatalf("with %q failing the handler ran %d time(s), want %d", step.failSQL, calls-callsBefore, step.moreCalls)
		}
	}

	// Everything heals: the recorded delivery clears the leftover without
	// another handler run.
	failSQL = ""
	callsBefore := calls
	if err := r.Tick(ctx); err != nil {
		t.Fatalf("the healing tick: %v", err)
	}
	if calls != callsBefore {
		t.Errorf("the healing tick ran the handler %d more time(s); the delivery was already recorded", calls-callsBefore)
	}
	if _, found := findDeadLetter(t, pool, subscriberName, event.EventID); found {
		t.Error("the resolved delivery is still in the operator queue")
	}
	attempts, _, found := deliveryRow(t, pool, subscriberName, event.EventID)
	if !found {
		t.Fatal("no completion row after the lane resolved")
	}
	if attempts != 2 {
		t.Errorf("the completion row records %d attempt(s), want 2 - the parked one plus the run whose write landed", attempts)
	}
}

func TestRequeueUnknownDeliveryFails(t *testing.T) {
	t.Parallel()
	pool := dispatcherDB(t)

	err := events.Requeue(context.Background(), pool, "nobody-"+randomSuffix(t), uuid.New())
	if !errors.Is(err, events.ErrNotParked) {
		t.Fatalf("Requeue of a delivery that was never parked: got %v, want ErrNotParked", err)
	}
}
