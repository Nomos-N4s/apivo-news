package events_test

// The writer's obligations come straight from the event contract
// (specs/002-apivo-cashback-alpha/contracts/events.md): the outbox row is
// written in the same transaction as the state change it describes, the
// type has the <producer>.<entity>.<past-tense-verb> shape, and envelope
// fields never appear inside payload. The validation tests run without a
// database - Append must refuse a defective message before any database
// work - and the atomicity tests run against a real Postgres.
//
// What is deliberately NOT re-proven here: that the partial unique index
// is scoped per producer, and that the pre-envelope writers keep working
// unmodified. Both are established at the database level by
// internal/platform/db/domain_event_envelope_test.go; these tests build
// on that and check only what the writer adds on top.

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

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// stubRow is the row a stubDB returns: Append scans the assigned id and
// occurred_at out of it, in that order.
type stubRow struct {
	id string
	at time.Time
}

func (r stubRow) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.id
	*(dest[1].(*time.Time)) = r.at
	return nil
}

// stubDB satisfies events.RowQuerier without a database, so the
// validation tests can prove Append rejects a defective message before
// any database work: a refused append must leave calls at zero.
type stubDB struct {
	calls int
	row   stubRow
}

func (s *stubDB) QueryRow(context.Context, string, ...any) pgx.Row {
	s.calls++
	return s.row
}

func TestNewWriterValidatesTheProducer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		producer string
		wantErr  bool
	}{
		{name: "a product domain", producer: "cashback"},
		{name: "empty", producer: "", wantErr: true},
		{name: "blank", producer: "   ", wantErr: true},
		{name: "dotted", producer: "cash.back", wantErr: true},
		{name: "inner space", producer: "cash back", wantErr: true},
		{name: "padded", producer: " cashback", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, err := events.NewWriter(tt.producer)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewWriter(%q): want error, got a writer", tt.producer)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewWriter(%q): %v", tt.producer, err)
			}
			if w == nil {
				t.Fatalf("NewWriter(%q): nil writer without an error", tt.producer)
			}
		})
	}
}

// TestAppendValidatesAtTheBoundary is the type-shape and message
// validation table. Every rejection must happen before the database is
// touched, so a defective producer is reported in its own terms rather
// than as a constraint violation deep inside an insert.
func TestAppendValidatesAtTheBoundary(t *testing.T) {
	t.Parallel()

	valid := events.Message{
		Type:    "cashback.entry.created",
		Payload: json.RawMessage(`{"entry_id": "e-1"}`),
	}

	tests := []struct {
		name    string
		msg     events.Message
		wantErr string // substring of the error; "" means accepted
	}{
		{name: "the minimal valid message", msg: valid},
		{
			name: "a fully specified message",
			msg: events.Message{
				Type:           "cashback.entry.state_changed",
				Version:        2,
				Subject:        uuid.New(),
				IdempotencyKey: "entry.state_changed:e-1:2",
				Payload:        json.RawMessage(`{"from": "pending", "to": "confirmed"}`),
			},
		},
		{
			name: "envelope-like names below the top level",
			msg: events.Message{
				Type:    "cashback.entry.created",
				Payload: json.RawMessage(`{"data": {"type": "inner", "event_id": "inner"}}`),
			},
		},
		{
			name:    "two segments",
			msg:     events.Message{Type: "cashback.created", Payload: valid.Payload},
			wantErr: "segment",
		},
		{
			name:    "an empty type",
			msg:     events.Message{Type: "", Payload: valid.Payload},
			wantErr: "segment",
		},
		{
			name:    "an empty middle segment",
			msg:     events.Message{Type: "cashback..created", Payload: valid.Payload},
			wantErr: "empty",
		},
		{
			name:    "whitespace inside the type",
			msg:     events.Message{Type: "cashback.entry .created", Payload: valid.Payload},
			wantErr: "whitespace",
		},
		{
			name:    "another producer's type",
			msg:     events.Message{Type: "news.article.approved", Payload: valid.Payload},
			wantErr: "appends as",
		},
		{
			name:    "a negative version",
			msg:     events.Message{Type: valid.Type, Version: -1, Payload: valid.Payload},
			wantErr: "negative",
		},
		{
			name:    "a blank idempotency key",
			msg:     events.Message{Type: valid.Type, IdempotencyKey: "   ", Payload: valid.Payload},
			wantErr: "blank",
		},
		{
			name:    "a missing payload",
			msg:     events.Message{Type: valid.Type},
			wantErr: "missing",
		},
		{
			name:    "a payload that is not JSON",
			msg:     events.Message{Type: valid.Type, Payload: json.RawMessage(`{"x":`)},
			wantErr: "JSON object",
		},
		{
			name:    "a payload that is a JSON array",
			msg:     events.Message{Type: valid.Type, Payload: json.RawMessage(`[1, 2]`)},
			wantErr: "JSON object",
		},
		{
			name:    "a payload that is a JSON scalar",
			msg:     events.Message{Type: valid.Type, Payload: json.RawMessage(`42`)},
			wantErr: "JSON object",
		},
		{
			name:    "a payload that is JSON null",
			msg:     events.Message{Type: valid.Type, Payload: json.RawMessage(`null`)},
			wantErr: "null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, err := events.NewWriter("cashback")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			db := &stubDB{row: stubRow{id: uuid.NewString(), at: time.Now()}}

			_, err = w.Append(context.Background(), db, tt.msg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Append rejected a valid message: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Append accepted a defective message; want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("rejected for the wrong reason:\n got: %v\nwant a reason containing: %s", err, tt.wantErr)
			}
			if db.calls != 0 {
				t.Errorf("the database was reached %d time(s) for a message that fails validation; want validation before any database work", db.calls)
			}
		})
	}
}

// TestAppendRefusesEnvelopeFieldsInThePayload is the contract's mandatory
// assertion: envelope fields never appear inside payload, and a producer
// that writes one there is a defect - refused at the boundary, before the
// stream is touched.
func TestAppendRefusesEnvelopeFieldsInThePayload(t *testing.T) {
	t.Parallel()

	envelopeFields := []string{
		"event_id", "type", "version", "occurred_at",
		"producer", "subject", "idempotency_key", "payload",
	}

	for _, field := range envelopeFields {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			w, err := events.NewWriter("cashback")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			db := &stubDB{}

			_, err = w.Append(context.Background(), db, events.Message{
				Type:    "cashback.entry.created",
				Payload: json.RawMessage(fmt.Sprintf(`{"entry_id": "e-1", %q: "smuggled"}`, field)),
			})
			if err == nil {
				t.Fatalf("Append accepted a payload carrying envelope field %q at its top level", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("the error does not name the offending field:\n got: %v\nwant a mention of %q", err, field)
			}
			if db.calls != 0 {
				t.Errorf("the database was reached %d time(s); a payload smuggling %q must be refused before any database work", db.calls, field)
			}
		})
	}
}

// TestAppendReturnsTheCompletedEnvelope checks the writer's half of the
// envelope assembly against a stub: the database-assigned id and
// occurred_at are read back, the version defaults to 1, and the producer
// is the writer's, not the caller's to choose.
func TestAppendReturnsTheCompletedEnvelope(t *testing.T) {
	t.Parallel()

	assignedID := uuid.New()
	assignedAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	subject := uuid.New()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	db := &stubDB{row: stubRow{id: assignedID.String(), at: assignedAt}}

	got, err := w.Append(context.Background(), db, events.Message{
		Type:           "cashback.entry.created",
		Subject:        subject,
		IdempotencyKey: "entry.created:e-1",
		Payload:        json.RawMessage(`{"entry_id": "e-1"}`),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	want := events.Event{
		EventID:        assignedID,
		Type:           "cashback.entry.created",
		Version:        1,
		OccurredAt:     assignedAt,
		Producer:       "cashback",
		Subject:        subject,
		IdempotencyKey: "entry.created:e-1",
		Payload:        json.RawMessage(`{"entry_id": "e-1"}`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Append returned:\n%+v\nwant:\n%+v", got, want)
	}
	if db.calls != 1 {
		t.Errorf("the database was reached %d time(s), want exactly 1", db.calls)
	}
}

// TestOutboxAtomicity is the contract's mandatory assertion: a forced
// failure between the state change and the outbox write leaves neither.
// Both halves live in one transaction, so there is nothing the writer has
// to clean up - the rollback is the guarantee.
func TestOutboxAtomicity(t *testing.T) {
	t.Parallel()

	// One event type per subtest, so the assertions against the suite's
	// shared database can count exactly the rows this test is about.
	appendPlace := func(t *testing.T, tx pgx.Tx) string {
		t.Helper()
		var placeID string
		if err := tx.QueryRow(context.Background(),
			`insert into place (name, country) values ($1, 'DE') returning id`,
			"atomicity-"+randomSuffix(t)).Scan(&placeID); err != nil {
			t.Fatalf("writing the state change: %v", err)
		}
		return placeID
	}
	countAfterRollback := func(t *testing.T, eventType, placeID string) (eventRows, placeRows int) {
		t.Helper()
		ctx := context.Background()
		if err := testPool.QueryRow(ctx,
			`select count(*) from domain_event where type = $1`, eventType).Scan(&eventRows); err != nil {
			t.Fatalf("counting events: %v", err)
		}
		if err := testPool.QueryRow(ctx,
			`select count(*) from place where id = $1`, placeID).Scan(&placeRows); err != nil {
			t.Fatalf("counting the state change: %v", err)
		}
		return eventRows, placeRows
	}

	t.Run("a failed append leaves neither the event nor the state change", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		w, err := events.NewWriter("cashback")
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		placeID := appendPlace(t, tx)
		eventType := "cashback.scratch" + randomSuffix(t) + ".tested"
		// The forced failure between the state change and the outbox
		// write: the producer smuggles an envelope field, so the append
		// is refused and the caller's only correct move is rollback.
		if _, err := w.Append(ctx, tx, events.Message{
			Type:    eventType,
			Payload: json.RawMessage(`{"producer": "defect"}`),
		}); err == nil {
			t.Fatal("Append accepted a defective payload")
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		eventRows, placeRows := countAfterRollback(t, eventType, placeID)
		if eventRows != 0 || placeRows != 0 {
			t.Fatalf("after the rollback: %d event row(s) and %d state row(s) survive; atomicity requires zero of each", eventRows, placeRows)
		}
	})

	t.Run("the state change and its event leave together", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		w, err := events.NewWriter("cashback")
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		placeID := appendPlace(t, tx)
		eventType := "cashback.scratch" + randomSuffix(t) + ".tested"
		if _, err := w.Append(ctx, tx, events.Message{
			Type:    eventType,
			Payload: json.RawMessage(`{"place_id": "` + placeID + `"}`),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}

		// Both are visible inside the transaction...
		var visible int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event where type = $1`, eventType).Scan(&visible); err != nil {
			t.Fatalf("counting inside the transaction: %v", err)
		}
		if visible != 1 {
			t.Fatalf("inside the transaction: %d event row(s), want 1", visible)
		}

		// ...and the rollback takes both away, never one of them.
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		eventRows, placeRows := countAfterRollback(t, eventType, placeID)
		if eventRows != 0 || placeRows != 0 {
			t.Fatalf("after the rollback: %d event row(s) and %d state row(s) survive; atomicity requires zero of each", eventRows, placeRows)
		}
	})
}

// TestAppendWritesTheEnvelopeColumns reads an appended event back and
// checks every envelope field landed in its own column - not inside
// payload - with the defaults the contract gives them.
func TestAppendWritesTheEnvelopeColumns(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	subject := uuid.New()
	key := "entry.created:" + randomSuffix(t)
	appended, err := w.Append(ctx, tx, events.Message{
		Type:           "cashback.entry.created",
		Version:        2,
		Subject:        subject,
		IdempotencyKey: key,
		Payload:        json.RawMessage(`{"entry_id": "e-1", "amount": {"minor": 250, "currency": "EUR"}}`),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if appended.EventID == uuid.Nil || appended.OccurredAt.IsZero() {
		t.Fatalf("Append returned an incomplete envelope: id %s, occurred_at %v", appended.EventID, appended.OccurredAt)
	}

	var (
		eventType, producer string
		version             int
		storedSubject       *string
		storedKey           *string
		payload             []byte
	)
	if err := tx.QueryRow(ctx,
		`select type, version, producer, subject::text, idempotency_key, payload
		 from domain_event where id = $1`, appended.EventID.String()).
		Scan(&eventType, &version, &producer, &storedSubject, &storedKey, &payload); err != nil {
		t.Fatalf("reading the appended event: %v", err)
	}
	if eventType != "cashback.entry.created" || version != 2 || producer != "cashback" {
		t.Errorf("stored (type, version, producer) = (%q, %d, %q), want (cashback.entry.created, 2, cashback)", eventType, version, producer)
	}
	if storedSubject == nil || *storedSubject != subject.String() {
		t.Errorf("stored subject = %v, want %s", storedSubject, subject)
	}
	if storedKey == nil || *storedKey != key {
		t.Errorf("stored idempotency_key = %v, want %q", storedKey, key)
	}
	var storedPayload map[string]any
	if err := json.Unmarshal(payload, &storedPayload); err != nil {
		t.Fatalf("stored payload is not JSON: %v", err)
	}
	if _, leaked := storedPayload["producer"]; leaked {
		t.Error("the stored payload carries a producer field; envelope data leaked into it")
	}
	if storedPayload["entry_id"] != "e-1" {
		t.Errorf("stored payload entry_id = %v, want e-1", storedPayload["entry_id"])
	}
}

// TestAppendDefaultsTheVersion checks the zero value maps to version 1,
// the first schema any type has.
func TestAppendDefaultsTheVersion(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	appended, err := w.Append(ctx, tx, events.Message{
		Type:    "cashback.entry.created",
		Payload: json.RawMessage(`{"entry_id": "e-1"}`),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var version int
	var subject, key *string
	if err := tx.QueryRow(ctx,
		`select version, subject::text, idempotency_key from domain_event where id = $1`,
		appended.EventID.String()).Scan(&version, &subject, &key); err != nil {
		t.Fatalf("reading the appended event: %v", err)
	}
	if version != 1 {
		t.Errorf("stored version = %d, want 1", version)
	}
	if subject != nil || key != nil {
		t.Error("a subject or idempotency key was stored that the producer never supplied")
	}
}

// TestAppendReportsARedeliveryAsAlreadyAppended checks the writer's
// reading of the partial unique index: a second append with the same
// (producer, idempotency key) is the same event arriving again, reported
// as ErrAlreadyAppended rather than as an opaque constraint violation.
// The index itself - its producer scoping included - is proven in
// internal/platform/db/domain_event_envelope_test.go.
func TestAppendReportsARedeliveryAsAlreadyAppended(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	w, err := events.NewWriter("cashback")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	msg := events.Message{
		Type:           "cashback.entry.created",
		IdempotencyKey: "entry.created:" + randomSuffix(t),
		Payload:        json.RawMessage(`{"entry_id": "e-1"}`),
	}
	if _, err := w.Append(ctx, tx, msg); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// In a savepoint - pgx.Tx.Begin nests - because the redelivery is
	// expected to abort whatever transaction it runs in, and this test's
	// outer transaction still has an assertion to run.
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err = w.Append(ctx, nested, msg)
	if !errors.Is(err, events.ErrAlreadyAppended) {
		t.Fatalf("second append with the same key: got %v, want ErrAlreadyAppended", err)
	}
	if err := nested.Rollback(ctx); err != nil {
		t.Fatalf("rolling back the savepoint: %v", err)
	}

	// The stream still holds exactly one copy.
	var count int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event where producer = 'cashback' and idempotency_key = $1`,
		msg.IdempotencyKey).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 1 {
		t.Fatalf("the stream holds %d copies of the event, want 1", count)
	}
}
