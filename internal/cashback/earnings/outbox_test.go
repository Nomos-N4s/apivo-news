package earnings_test

// The stand-in for the transaction this package's events are appended
// through, shared by every suite that moves an entry.
//
// A fake rather than a database, because what these suites assert is that
// the machine announced AT ALL and under which key - which is this package's
// behaviour. What lands in which column is the outbox's behaviour, and
// events_test.go asserts that against the real thing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// announced is one appended event as the fake saw it.
type announced struct {
	Type    string
	Subject string
	Key     string
	Payload map[string]any
}

// fakeOutbox records what was appended, and can refuse.
//
// It also enforces the one rule the real outbox enforces and a naive fake
// would not: a repeated idempotency key fails. Without that, a test could
// announce the same fact twice and the fake would happily agree.
type fakeOutbox struct {
	events []announced
	keys   map[string]bool
	err    error
}

// errOutboxRefused is what a fake outbox asked to fail returns, standing in
// for anything the database might refuse an append with.
var errOutboxRefused = errors.New("the outbox refused the append")

func (o *fakeOutbox) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if o.err != nil {
		return failingRow{err: o.err}
	}
	if len(args) != 6 {
		return failingRow{err: fmt.Errorf("the outbox insert took %d arguments, not 6", len(args))}
	}
	seen := announced{Type: str(args[0]), Subject: deref(args[4]), Key: deref(args[5])}
	if err := json.Unmarshal([]byte(str(args[1])), &seen.Payload); err != nil {
		return failingRow{err: fmt.Errorf("the payload is not a JSON object: %w", err)}
	}
	if o.keys == nil {
		o.keys = map[string]bool{}
	}
	if seen.Key != "" && o.keys[seen.Key] {
		return failingRow{err: fmt.Errorf("%w: idempotency key %q is already in the stream", errOutboxRefused, seen.Key)}
	}
	o.keys[seen.Key] = true
	o.events = append(o.events, seen)
	return appendedRow{}
}

// of answers the events of one type, so a case can name the fact it is
// asserting instead of indexing into a slice whose order it would have to
// keep in its head.
func (o *fakeOutbox) of(t *testing.T, eventType string) []announced {
	t.Helper()
	var out []announced
	for _, e := range o.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// only answers the single event of a type, failing the case where there is
// not exactly one.
func (o *fakeOutbox) only(t *testing.T, eventType string) announced {
	t.Helper()
	found := o.of(t, eventType)
	if len(found) != 1 {
		t.Fatalf("announced %s %d times, want exactly 1: %+v", eventType, len(found), o.events)
	}
	return found[0]
}

func str(arg any) string {
	s, _ := arg.(string)
	return s
}

func deref(arg any) string {
	p, ok := arg.(*string)
	if !ok || p == nil {
		return ""
	}
	return *p
}

// appendedRow answers the id and instant the database would have assigned.
type appendedRow struct{}

func (appendedRow) Scan(dest ...any) error {
	if len(dest) != 2 {
		return fmt.Errorf("the outbox insert scanned %d columns, not 2", len(dest))
	}
	id, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("the outbox insert scanned %T for the id, not *string", dest[0])
	}
	at, ok := dest[1].(*time.Time)
	if !ok {
		return fmt.Errorf("the outbox insert scanned %T for the instant, not *time.Time", dest[1])
	}
	*id = uuid.NewString()
	*at = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	return nil
}

// failingRow carries a refusal out through the pgx.Row the writer scans.
type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }
