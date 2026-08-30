// The tests for events.go: what reaches the outbox, and what must never.
//
// Against a real database rather than a fake writer, because what is being
// asserted is the stored ROW - which column each envelope field landed in,
// and that the payload carries none of them. A fake would be asserting this
// file's opinion of the outbox instead of the outbox.

package networks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// storedEvent is one row of the outbox, read back.
type storedEvent struct {
	Type           string
	Version        int
	Producer       string
	Subject        *string
	IdempotencyKey *string
	Payload        map[string]any
}

// outboxAbout reads the events appended about one subject, oldest first.
//
// Scoped, because domain_event is append-only and shared: every other test
// that commits an event is visible from this transaction, so an unscoped
// read counts whatever else has run against this database rather than what
// this case appended. Filtering on the subject is what makes the exact
// counts below statements about the announcer.
func outboxAbout(ctx context.Context, t *testing.T, tx pgx.Tx, subject uuid.UUID) []storedEvent {
	t.Helper()
	return readOutbox(ctx, t, tx, `subject = $1`, subject.String())
}

// outboxAboutNothing reads the events this module appended with no subject
// at all - which is exactly the defect the refusal test is about, and the
// only shape an event announced about a report the database never stored
// could take. Producer as well as subject, because a null subject is the
// ordinary case for the news module's events and none of those are this
// test's business.
func outboxAboutNothing(ctx context.Context, t *testing.T, tx pgx.Tx) []storedEvent {
	t.Helper()
	return readOutbox(ctx, t, tx, `producer = $1 and subject is null`, networks.EventProducer)
}

func readOutbox(ctx context.Context, t *testing.T, tx pgx.Tx, where string, arg any) []storedEvent {
	t.Helper()
	rows, err := tx.Query(ctx, `
		select type, version, producer, subject::text, idempotency_key, payload
		  from domain_event
		 where `+where+`
		 order by occurred_at, id`, arg)
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	defer rows.Close()

	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		var raw []byte
		if err := rows.Scan(&e.Type, &e.Version, &e.Producer, &e.Subject, &e.IdempotencyKey, &raw); err != nil {
			t.Fatalf("scanning an event: %v", err)
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			t.Fatalf("the payload is not a JSON object: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	return out
}

// envelopeFieldNames is the set a payload may never carry at its top level.
// Spelled out here rather than imported, so this test would notice the
// platform package quietly shortening its own list.
var envelopeFieldNames = []string{
	"event_id", "type", "version", "occurred_at",
	"producer", "subject", "idempotency_key", "payload",
}

func wantNoEnvelopeFields(t *testing.T, e storedEvent) {
	t.Helper()
	for _, field := range envelopeFieldNames {
		if _, defect := e.Payload[field]; defect {
			t.Errorf("%s carries envelope field %q inside its payload; envelope fields live in their own columns", e.Type, field)
		}
	}
}

// TestAnnouncerRefusesAFactAboutNothing keeps a report the database did not
// store from being announced. The zero Recorded is what an unchanged
// re-report yields, so this is the value the poller's loop holds most often;
// announced, it would publish an event whose subject is the nil uuid and
// whose idempotency key every other such event shares.
func TestAnnouncerRefusesAFactAboutNothing(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)

	announcer, err := networks.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	if err := announcer.Ingested(ctx, tx, networks.NetworkID("fixture"), networks.StatusPending, networks.Recorded{}); !errors.Is(err, networks.ErrNotAnnounced) {
		t.Errorf("Ingested(zero) = %v, want one wrapping ErrNotAnnounced", err)
	}
	if err := announcer.Unattributed(ctx, tx, networks.Queued{}); !errors.Is(err, networks.ErrNotAnnounced) {
		t.Errorf("Unattributed(zero) = %v, want one wrapping ErrNotAnnounced", err)
	}
	if events := outboxAboutNothing(ctx, t, tx); len(events) != 0 {
		t.Errorf("%d event(s) were appended about nothing", len(events))
	}
}

// TestTheTwoFactsAboutOneReportDoNotCollide is the case the idempotency key
// is shaped for, and the one a key of the report id alone gets wrong. The
// outbox's unique index is on the key by itself, so two different facts
// about one report keyed only by its id would collide - and the collision is
// SILENT, because an already-appended key is not an error. A report that
// went unattributed would simply never be announced as such.
func TestTheTwoFactsAboutOneReportDoNotCollide(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)

	announcer, err := networks.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	at := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	report := uuid.New()

	if err := announcer.Ingested(ctx, tx, networks.NetworkID("fixture"), networks.StatusPending,
		networks.Recorded{ID: report, RetrievedAt: at}); err != nil {
		t.Fatalf("Ingested(): %v", err)
	}
	if err := announcer.Unattributed(ctx, tx, networks.Queued{ReportID: report, DetectedAt: at}); err != nil {
		t.Fatalf("Unattributed(): %v", err)
	}

	events := outboxAbout(ctx, t, tx, report)
	if len(events) != 2 {
		t.Fatalf("%d event(s) about one report, want both facts: %+v", len(events), events)
	}
	seen := map[string]storedEvent{}
	for _, e := range events {
		seen[e.Type] = e
		if e.Producer != networks.EventProducer || e.Version != 1 {
			t.Errorf("%s was appended as %s v%d, want %s v1", e.Type, e.Producer, e.Version, networks.EventProducer)
		}
		if e.Subject == nil || *e.Subject != report.String() {
			t.Errorf("%s names subject %v, want the report %s", e.Type, e.Subject, report)
		}
		if e.IdempotencyKey == nil || *e.IdempotencyKey != e.Type+":"+report.String() {
			t.Errorf("%s is keyed %v, want its own type and the report", e.Type, e.IdempotencyKey)
		}
		wantNoEnvelopeFields(t, e)
	}

	ingested, told := seen[networks.TypeTransactionIngested]
	if !told {
		t.Fatalf("nothing announced %s", networks.TypeTransactionIngested)
	}
	// Identifiers and the normalised status, and nothing else: consumer
	// rule 5 keeps the data in its owning schema, so no amounts, no click
	// reference and above all no raw network payload.
	if len(ingested.Payload) != 4 {
		t.Errorf("the ingested payload carries %d field(s): %v", len(ingested.Payload), ingested.Payload)
	}
	if ingested.Payload["network_transaction_id"] != report.String() {
		t.Errorf("the ingested payload names report %v, want %s", ingested.Payload["network_transaction_id"], report)
	}
	if ingested.Payload["status"] != string(networks.StatusPending) || ingested.Payload["network_id"] != "fixture" {
		t.Errorf("the ingested payload reads %v", ingested.Payload)
	}
	if ingested.Payload["at"] != at.Format(time.RFC3339Nano) {
		t.Errorf("the ingested payload was stamped %v, want the row's own retrieval instant %s",
			ingested.Payload["at"], at.Format(time.RFC3339Nano))
	}

	unattributed, told := seen[networks.TypeTransactionUnattributed]
	if !told {
		t.Fatalf("nothing announced %s", networks.TypeTransactionUnattributed)
	}
	if len(unattributed.Payload) != 2 {
		t.Errorf("the unattributed payload carries %d field(s), want the two the contract names: %v",
			len(unattributed.Payload), unattributed.Payload)
	}
}

// TestAnnouncingOneFactTwiceIsAFailure holds the answer that is tempting to
// get wrong. A second append of the same key reads like a redelivery to be
// shrugged off - but the outbox reports it as a unique violation, and a
// failed statement ABORTS THE TRANSACTION. A caller that swallowed it would
// carry on through a poll whose every later statement raises 25P02, and
// would commit nothing while reporting success.
//
// So the announcer fails, and the poll fails with it. It should never
// happen: one event of each type is announced per row the database has just
// stored, and an unchanged re-report stores no row.
func TestAnnouncingOneFactTwiceIsAFailure(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)

	announcer, err := networks.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	stored := networks.Recorded{ID: uuid.New(), RetrievedAt: time.Now().UTC()}
	if err := announcer.Ingested(ctx, tx, networks.NetworkID("fixture"), networks.StatusConfirmed, stored); err != nil {
		t.Fatalf("Ingested(): %v", err)
	}

	err = announcer.Ingested(ctx, tx, networks.NetworkID("fixture"), networks.StatusConfirmed, stored)
	if !errors.Is(err, networks.ErrNotAnnounced) {
		t.Fatalf("announcing one fact twice = %v, want one wrapping ErrNotAnnounced", err)
	}
	if !errors.Is(err, events.ErrAlreadyAppended) {
		t.Errorf("the failure is %v, want it to carry the outbox's own reason", err)
	}
	// And the transaction really is unusable afterwards, which is the whole
	// argument for failing rather than shrugging.
	if _, probe := tx.Exec(ctx, `select 1`); probe == nil {
		t.Error("the transaction survived the collision; if it did, swallowing it would have been safe after all")
	}
}
