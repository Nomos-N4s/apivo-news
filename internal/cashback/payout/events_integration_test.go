// What actually reaches the outbox, read back from it (T100).
//
// The guards live in events_test.go and need no database. These need one,
// because the claim is not what the announcer would say - it is that the
// event and the state change COMMIT TOGETHER. Only a real transaction can
// be asked that.

package payout_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// announced is one outbox row as this suite reads it.
type announced struct {
	Producer string
	Subject  uuid.UUID
	Payload  map[string]any
}

// announcementsOf reads every event of a type about a subject.
func announcementsOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, eventType string, subject uuid.UUID) []announced {
	t.Helper()
	rows, err := pool.Query(ctx,
		`select producer, subject, payload from domain_event
		  where type = $1 and subject = $2 order by occurred_at`,
		eventType, pgtype.UUID{Bytes: subject, Valid: true})
	if err != nil {
		t.Fatalf("reading %s events about %s: %v", eventType, subject, err)
	}
	defer rows.Close()

	var found []announced
	for rows.Next() {
		var (
			one     announced
			id      pgtype.UUID
			payload []byte
		)
		if err := rows.Scan(&one.Producer, &id, &payload); err != nil {
			t.Fatalf("scanning a %s event: %v", eventType, err)
		}
		one.Subject = uuid.UUID(id.Bytes)
		if err := json.Unmarshal(payload, &one.Payload); err != nil {
			t.Fatalf("unmarshalling a %s payload: %v", eventType, err)
		}
		found = append(found, one)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading %s events: %v", eventType, err)
	}
	return found
}

// amountIn reads the {minor, currency} shape every payload carries (C-6).
func amountIn(t *testing.T, payload map[string]any) (int64, string) {
	t.Helper()
	amount, ok := payload["amount"].(map[string]any)
	if !ok {
		t.Fatalf("the payload carries no amount object: %v", payload["amount"])
	}
	minor, ok := amount["minor"].(float64)
	if !ok {
		t.Fatalf("the amount carries no minor: %v", amount["minor"])
	}
	currency, _ := amount["currency"].(string)
	return int64(minor), currency
}

// TestARequestIsAnnouncedInTheTransactionThatMadeIt is the atomicity claim,
// asked the only way it can be answered: the request committed, so its event
// is there to read - one of them, from this producer, about this request.
func TestARequestIsAnnouncedInTheTransactionThatMadeIt(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	made, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 2500),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}

	events := announcementsOf(ctx, t, pool, "cashback.withdrawal.requested", made.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cashback.withdrawal.requested events, want exactly 1", len(events))
	}
	one := events[0]
	if one.Producer != "cashback" {
		t.Errorf("producer = %q, want cashback", one.Producer)
	}
	if one.Payload["account_id"] != f.member.String() {
		t.Errorf("account_id = %v, want the member %s", one.Payload["account_id"], f.member)
	}
	if one.Payload["request_id"] != made.ID.String() {
		t.Errorf("request_id = %v, want %s", one.Payload["request_id"], made.ID)
	}
	// The RESERVED amount, which is what a payout will pay and not
	// necessarily what was asked for (D9). A consumer taking the asking
	// figure would be tracking money that was never moved.
	minor, currency := amountIn(t, one.Payload)
	if minor != made.Amount.Minor || currency != string(made.Amount.Currency) {
		t.Errorf("amount = %d %s, want the reserved %s", minor, currency, made.Amount)
	}
	// The envelope's own fields never appear inside a payload.
	for _, forbidden := range []string{"type", "producer", "subject", "version", "occurred_at", "id"} {
		if _, present := one.Payload[forbidden]; present {
			t.Errorf("the payload carries the envelope field %q", forbidden)
		}
	}
	// Nothing an operator has not done yet is claimed.
	if _, present := one.Payload["actor"]; present {
		t.Error("a request carries an actor; nobody but the member has acted on it")
	}
	if _, present := one.Payload["reason"]; present {
		t.Error("a request carries a reason; nobody has refused it")
	}
}

// TestARefusedRequestIsAnnouncedToNobody is the other half of atomicity, and
// the half that is easy to get wrong: a request the service refused must
// leave NOTHING in the stream. An event published for a state change that
// rolled back is a fact no table agrees with, and a consumer has no way to
// find that out.
func TestARefusedRequestIsAnnouncedToNobody(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	// A threshold above the balance, so the request is refused after the
	// entries have been read and before anything is written.
	f := aFixture(ctx, t, 50_000, 5000)

	if _, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 2500),
	}); err == nil {
		t.Fatal("the request succeeded; this case needs it refused")
	}

	var announcements int
	if err := pool.QueryRow(ctx,
		`select count(*) from domain_event
		  where type = 'cashback.withdrawal.requested'
		    and payload->>'account_id' = $1`, f.member.String()).Scan(&announcements); err != nil {
		t.Fatalf("counting the member's announcements: %v", err)
	}
	if announcements != 0 {
		t.Errorf("a refused request left %d events in the stream, want none", announcements)
	}
}
