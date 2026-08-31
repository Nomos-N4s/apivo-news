package earnings_test

// What reaches the outbox when an entry is created and when it moves, and
// what must never (T076).
//
// The row-level cases run against a real database rather than the fake
// outbox the other suites share, because what is being asserted is the
// stored ROW - which column each envelope field landed in, and that the
// payload carries none of them. A fake would be asserting this file's
// opinion of the outbox instead of the outbox.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// outboxTx migrates, connects and opens a transaction that is always rolled
// back, so a run leaves the stream exactly as it found it.
func outboxTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the outbox")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	})
	return ctx, tx
}

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
// Scoped to the subject, because domain_event is append-only and shared:
// every other test that commits an event is visible from this transaction,
// so an unscoped read would count whatever else has run against this
// database rather than what this case appended.
func outboxAbout(ctx context.Context, t *testing.T, tx pgx.Tx, subject uuid.UUID) []storedEvent {
	t.Helper()
	rows, err := tx.Query(ctx, `
		select type, version, producer, subject::text, idempotency_key, payload
		  from domain_event
		 where subject = $1
		 order by occurred_at, id`, subject.String())
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

func announcer(t *testing.T) *earnings.Announcer {
	t.Helper()
	a, err := earnings.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	return a
}

// anEntryValue is the domain value an announcement is made from, distinct
// from the stored row the state machine's suites use.
func anEntryValue(t *testing.T, state earnings.State) earnings.Entry {
	t.Helper()
	amount, err := money.New(3000, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return earnings.Entry{
		ID:        uuid.New(),
		Member:    uuid.New(),
		Brand:     "apivo-de",
		Report:    uuid.New(),
		Click:     uuid.New(),
		State:     state,
		Amount:    amount,
		CreatedAt: time.Date(2026, time.March, 1, 9, 30, 0, 0, time.UTC),
	}
}

// TestCreationLandsInTheStreamAsTheContractDescribesIt is the payload of
// contracts/events.md read back out of the column it was stored in.
func TestCreationLandsInTheStreamAsTheContractDescribesIt(t *testing.T) {
	ctx, tx := outboxTx(t)
	entry := anEntryValue(t, earnings.StateHeld)

	if err := announcer(t).Created(ctx, tx, entry); err != nil {
		t.Fatalf("Created(): %v", err)
	}

	stored := outboxAbout(ctx, t, tx, entry.ID)
	if len(stored) != 1 {
		t.Fatalf("appended %d event(s), want one", len(stored))
	}
	got := stored[0]
	if got.Type != earnings.TypeEntryCreated {
		t.Errorf("type = %q, want %q", got.Type, earnings.TypeEntryCreated)
	}
	if got.Producer != earnings.EventProducer {
		t.Errorf("producer = %q, want %q", got.Producer, earnings.EventProducer)
	}
	if got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
	if got.Subject == nil || *got.Subject != entry.ID.String() {
		t.Errorf("subject = %v, want the entry %s", got.Subject, entry.ID)
	}
	if got.IdempotencyKey == nil || *got.IdempotencyKey != earnings.TypeEntryCreated+":"+entry.ID.String() {
		t.Errorf("idempotency key = %v, want the type and the entry", got.IdempotencyKey)
	}
	wantNoEnvelopeFields(t, got)

	if got.Payload["account_id"] != entry.Member.String() {
		t.Errorf("account_id = %v, want %s", got.Payload["account_id"], entry.Member)
	}
	if got.Payload["state"] != string(earnings.StateHeld) {
		t.Errorf("state = %v, want %s", got.Payload["state"], earnings.StateHeld)
	}
	// C-6: the amount travels as {minor, currency}, never as a bare integer
	// a consumer would have to decide a currency for.
	amount, shaped := got.Payload["amount"].(map[string]any)
	if !shaped {
		t.Fatalf("amount = %#v, want an object carrying minor and currency", got.Payload["amount"])
	}
	if amount["minor"] != float64(3000) || amount["currency"] != "EUR" {
		t.Errorf("amount = %#v, want 3000 EUR", amount)
	}
}

// TestAMoveLandsInTheStreamNamingItsTransfer is C-7 seen from the stream: a
// consumer holding the event can find the posting without reading this
// module's tables, which is what consumer rule 2 asks of the payload.
func TestAMoveLandsInTheStreamNamingItsTransfer(t *testing.T) {
	ctx, tx := outboxTx(t)
	moved := earnings.Transition{
		Entry:    uuid.New(),
		From:     earnings.StatePending,
		To:       earnings.StateConfirmed,
		Transfer: wallet.TransferRef("transfer:entry:cause:confirmed"),
		At:       time.Date(2026, time.March, 2, 8, 0, 0, 0, time.UTC),
	}

	if err := announcer(t).StateChanged(ctx, tx, moved); err != nil {
		t.Fatalf("StateChanged(): %v", err)
	}

	stored := outboxAbout(ctx, t, tx, moved.Entry)
	if len(stored) != 1 {
		t.Fatalf("appended %d event(s), want one", len(stored))
	}
	got := stored[0]
	if got.Type != earnings.TypeEntryStateChanged {
		t.Errorf("type = %q, want %q", got.Type, earnings.TypeEntryStateChanged)
	}
	if got.Subject == nil || *got.Subject != moved.Entry.String() {
		t.Errorf("subject = %v, want the entry %s", got.Subject, moved.Entry)
	}
	// Keyed on the transfer, not the entry: an entry moves many times and
	// keying on it would refuse every move after the first.
	if got.IdempotencyKey == nil || *got.IdempotencyKey != earnings.TypeEntryStateChanged+":"+string(moved.Transfer) {
		t.Errorf("idempotency key = %v, want the type and the transfer", got.IdempotencyKey)
	}
	wantNoEnvelopeFields(t, got)

	if got.Payload["from"] != string(earnings.StatePending) || got.Payload["to"] != string(earnings.StateConfirmed) {
		t.Errorf("from/to = %v/%v, want pending/confirmed", got.Payload["from"], got.Payload["to"])
	}
	if got.Payload["ledger_transfer_ref"] != string(moved.Transfer) {
		t.Errorf("ledger_transfer_ref = %v, want %s", got.Payload["ledger_transfer_ref"], moved.Transfer)
	}
	if got.Payload["at"] != "2026-03-02T08:00:00Z" {
		t.Errorf("at = %v, want the instant the transition row carries", got.Payload["at"])
	}
}

// TestAnEntryMovesMoreThanOnce is the case that would fail if the move were
// keyed on the entry. Both moves are about one entry and both must land.
func TestAnEntryMovesMoreThanOnce(t *testing.T) {
	ctx, tx := outboxTx(t)
	entry := uuid.New()
	a := announcer(t)

	first := earnings.Transition{Entry: entry, From: earnings.StateHeld, To: earnings.StatePending, Transfer: "transfer:one"}
	second := earnings.Transition{Entry: entry, From: earnings.StatePending, To: earnings.StateConfirmed, Transfer: "transfer:two"}
	if err := a.StateChanged(ctx, tx, first); err != nil {
		t.Fatalf("the first move: %v", err)
	}
	if err := a.StateChanged(ctx, tx, second); err != nil {
		t.Fatalf("the second move: %v", err)
	}

	if stored := outboxAbout(ctx, t, tx, entry); len(stored) != 2 {
		t.Fatalf("appended %d event(s) about one entry, want two", len(stored))
	}
}

// TestAnnouncingOneTransferTwiceIsRefused. The same move retried re-posts to
// the same transfer (D8), and the second announcement of it is a duplicate
// the stream must not carry.
func TestAnnouncingOneTransferTwiceIsRefused(t *testing.T) {
	ctx, tx := outboxTx(t)
	moved := earnings.Transition{Entry: uuid.New(), From: earnings.StatePending, To: earnings.StateConfirmed, Transfer: "transfer:once"}
	a := announcer(t)

	if err := a.StateChanged(ctx, tx, moved); err != nil {
		t.Fatalf("StateChanged(): %v", err)
	}
	// The collision aborts the transaction, so nothing may be read from it
	// afterwards - which is exactly why this is a failure and not a no-op.
	err := a.StateChanged(ctx, tx, moved)
	if !errors.Is(err, earnings.ErrNotAnnounced) || !errors.Is(err, events.ErrAlreadyAppended) {
		t.Fatalf("the second announcement = %v, want one wrapping both %v and %v",
			err, earnings.ErrNotAnnounced, events.ErrAlreadyAppended)
	}
}

// TestNothingIsAnnouncedAboutNothing keeps a fact the database never wrote
// out of the stream. The zero values are what an unchecked caller would pass
// after a write it did not look at.
func TestNothingIsAnnouncedAboutNothing(t *testing.T) {
	ctx, tx := outboxTx(t)
	a := announcer(t)

	if err := a.Created(ctx, tx, earnings.Entry{}); !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Errorf("Created(zero) = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
	if err := a.StateChanged(ctx, tx, earnings.Transition{}); !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Errorf("StateChanged(zero) = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
	// D7: no state is recorded without its posting, so a move naming no
	// transfer is a move the schema would have refused.
	moved := earnings.Transition{Entry: uuid.New(), From: earnings.StatePending, To: earnings.StateConfirmed}
	if err := a.StateChanged(ctx, tx, moved); !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Errorf("StateChanged(no transfer) = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
	if stored := outboxAbout(ctx, t, tx, moved.Entry); len(stored) != 0 {
		t.Errorf("appended %d event(s) about a move naming no transfer, want none", len(stored))
	}
}
