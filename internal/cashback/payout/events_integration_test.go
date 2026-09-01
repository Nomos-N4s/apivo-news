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
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
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

// TestAnApprovalIsAnnouncedWithTheHumanWhoMadeIt. FR-061 wants a named human
// on every operator action, and C-4 puts one in the column. The event
// carries it rather than an id to come back and ask about: a consumer that
// had to resolve the approver through this schema would be making exactly
// the synchronous call-back consumer rule 2 forbids.
func TestAnApprovalIsAnnouncedWithTheHumanWhoMadeIt(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	if _, err := approvals(t, f, stub.New()).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	events := announcementsOf(ctx, t, pool, "cashback.withdrawal.approved", request.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cashback.withdrawal.approved events, want exactly 1", len(events))
	}
	one := events[0]
	if one.Payload["actor"] != operator.String() {
		t.Errorf("actor = %v, want the operator %s", one.Payload["actor"], operator)
	}
	if one.Payload["account_id"] != f.member.String() {
		t.Errorf("account_id = %v, want the member %s", one.Payload["account_id"], f.member)
	}
	if minor, currency := amountIn(t, one.Payload); minor != request.Amount.Minor || currency != string(request.Amount.Currency) {
		t.Errorf("amount = %d %s, want %s", minor, currency, request.Amount)
	}
	// An approval has no reason: the reason field belongs to a refusal, and
	// a consumer branching on its presence must be able to.
	if _, present := one.Payload["reason"]; present {
		t.Errorf("an approval carries a reason: %v", one.Payload["reason"])
	}
}

// TestAnApprovalTheRailRefusedIsStillAnnounced. The decision and the payout
// are committed before the rail is asked (FR-052), so the announcement is
// too. Waiting for the rail would drop the event on exactly the submissions
// that go wrong - which is when somebody most wants to know a decision was
// made.
func TestAnApprovalTheRailRefusedIsStillAnnounced(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)

	if _, err := approvals(t, f, stub.New(stub.WithTimeout())).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("Approve() = %v, want a retryable rail failure", err)
	}

	if events := announcementsOf(ctx, t, pool, "cashback.withdrawal.approved", request.ID); len(events) != 1 {
		t.Errorf("a timed-out submission left %d approval events, want the 1 the decision made", len(events))
	}
}

// TestARefusalIsAnnouncedWithItsReason. The reason is what a member is owed
// (FR-061) and what withdrawal_request_rejection_has_reason makes mandatory,
// so it travels with the fact rather than being left in a column.
func TestARefusalIsAnnouncedWithItsReason(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	const reason = "the destination could not be verified"

	if _, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: request.ID, Operator: operator, Reason: reason,
	}); err != nil {
		t.Fatalf("Reject(): %v", err)
	}

	events := announcementsOf(ctx, t, pool, "cashback.withdrawal.rejected", request.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cashback.withdrawal.rejected events, want exactly 1", len(events))
	}
	one := events[0]
	if one.Payload["reason"] != reason {
		t.Errorf("reason = %v, want %q", one.Payload["reason"], reason)
	}
	if one.Payload["actor"] != operator.String() {
		t.Errorf("actor = %v, want the operator %s", one.Payload["actor"], operator)
	}
	// And nothing claims it was approved.
	if events := announcementsOf(ctx, t, pool, "cashback.withdrawal.approved", request.ID); len(events) != 0 {
		t.Errorf("a refused request left %d approval events, want none", len(events))
	}
}

// TestAPaymentThatWillNeverHappenIsAnnounced. The classification is what
// tells a consumer this from a payment still being retried - the only thing
// this event exists to say - and the subject is the PAYOUT rather than the
// request, because it is a fact about the payment and per-subject ordering
// is the only ordering the stream guarantees.
func TestAPaymentThatWillNeverHappenIsAnnounced(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, request := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	rail := stub.New(stub.WithTimeout())

	if _, err := approvals(t, f, rail).Approve(ctx, payout.Approval{
		Request: request.ID, Operator: operator,
	}); !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("the approval = %v, want a retryable rail failure", err)
	}
	// A retryable failure announces nothing: the payment may still happen,
	// and a consumer told otherwise would write the member off.
	if announced := failuresFor(ctx, t, pool, request.ID); announced != 0 {
		t.Errorf("a timed-out submission announced %d failures, want none", announced)
	}

	rail.FailNext(payout.Terminal(errors.New("the destination account is closed")))
	outcome, err := retries(t, f, rail).Retry(ctx, request.ID)
	if err != nil {
		t.Fatalf("Retry(): %v", err)
	}

	events := announcementsOf(ctx, t, pool, "cashback.payout.failed", outcome.Payout.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cashback.payout.failed events, want exactly 1", len(events))
	}
	one := events[0]
	if one.Payload["classification"] != "terminal" {
		t.Errorf("classification = %v, want terminal", one.Payload["classification"])
	}
	if one.Payload["request_id"] != request.ID.String() {
		t.Errorf("request_id = %v, want %s", one.Payload["request_id"], request.ID)
	}
	if one.Payload["payout_id"] != outcome.Payout.ID.String() {
		t.Errorf("payout_id = %v, want %s", one.Payload["payout_id"], outcome.Payout.ID)
	}
	// The instant is the DECISION's, not the submission's. The payout row
	// carries only when the money was first sent, and an event that used it
	// would date the failure to whenever the payment was attempted - which
	// on a payment retried for days is a lie a consumer cannot detect.
	var decidedAt, submittedAt time.Time
	if err := pool.QueryRow(ctx,
		`select r.decided_at, p.submitted_at
		   from cashback.withdrawal_request r
		   join cashback.payout p on p.request_id = r.id
		  where r.id = $1`,
		pgtype.UUID{Bytes: request.ID, Valid: true}).Scan(&decidedAt, &submittedAt); err != nil {
		t.Fatalf("reading the two instants: %v", err)
	}
	if decidedAt.Equal(submittedAt) {
		t.Fatal("the decision and the submission share an instant; this case cannot tell them apart")
	}
	announcedAt, err := time.Parse(time.RFC3339Nano, one.Payload["at"].(string))
	if err != nil {
		t.Fatalf("the payload's at is not a timestamp: %v (%v)", err, one.Payload["at"])
	}
	if !announcedAt.Equal(decidedAt) {
		t.Errorf("at = %s, want the decision's %s (the submission's is %s)",
			announcedAt, decidedAt, submittedAt)
	}

	// The rail never answered, so there is no reference to carry - and an
	// empty string would say the rail named this payment "".
	if ref, present := one.Payload["rail_reference"]; present {
		t.Errorf("rail_reference = %v on a payment the rail never named, want it absent", ref)
	}
	// The refusal is also the request's decision, and both are announced.
	if refusals := announcementsOf(ctx, t, pool, "cashback.withdrawal.rejected", request.ID); len(refusals) != 0 {
		t.Errorf("a failed payment announced %d rejections; a rejection is a decision BEFORE approval", len(refusals))
	}
}

// failuresFor counts the payout failures announced about one request. Keyed
// on the request rather than the payout because the interesting moment is
// before the failure exists, when there is no payout id to ask about.
func failuresFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, request uuid.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`select count(*) from domain_event
		  where type = 'cashback.payout.failed' and payload->>'request_id' = $1`,
		request.String()).Scan(&count); err != nil {
		t.Fatalf("counting payout failures for %s: %v", request, err)
	}
	return count
}
