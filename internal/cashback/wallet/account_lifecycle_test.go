package wallet_test

// T126 against the real schema: what happens in cashback when an account
// stops existing upstream.
//
// Driven through the handler the registry calls, over a transaction that is
// rolled back - so every case reads its own writes and the suite leaves
// nothing behind. Delivery is at-least-once, so every case that matters is
// asked twice.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	walletstore "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// closures builds the consumer over one transaction, the way the
// composition root builds it over the pool.
func closures(t *testing.T, tx pgx.Tx) *wallet.AccountClosures {
	t.Helper()
	participations, err := wallet.NewParticipations(tx, walletstore.New(tx), wallet.Terms{})
	if err != nil {
		t.Fatalf("NewParticipations(): %v", err)
	}
	made, err := wallet.NewAccountClosures(slog.New(slog.DiscardHandler), tx, participations, walletstore.New(tx))
	if err != nil {
		t.Fatalf("NewAccountClosures(): %v", err)
	}
	return made
}

// deletedEvent is the identity fact, as the contract writes its payload.
func deletedEvent(t *testing.T, member uuid.UUID, at time.Time) events.Event {
	t.Helper()
	payload, err := json.Marshal(struct {
		AccountID uuid.UUID `json:"account_id"`
		DeletedAt time.Time `json:"deleted_at"`
	}{AccountID: member, DeletedAt: at})
	if err != nil {
		t.Fatalf("building the payload: %v", err)
	}
	return events.Event{
		EventID:    uuid.New(),
		Type:       wallet.TypeAccountDeleted,
		Version:    1,
		OccurredAt: at,
		Producer:   "identity",
		Subject:    member,
		Payload:    payload,
	}
}

// anOptedInMember seeds an account that is in cashback, which is the state
// a deletion has something to close.
func anOptedInMember(ctx context.Context, t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	member := aMember(ctx, t, tx)
	if _, err := tx.Exec(ctx, `
		insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
		values ($1, 'fixture', '1.0.0', 'EUR')`, member); err != nil {
		t.Fatalf("opting the member in: %v", err)
	}
	return member
}

// aWithdrawal seeds one request of the member's in the given state, with a
// verified destination of their own so the schema accepts it.
func aWithdrawal(ctx context.Context, t *testing.T, tx pgx.Tx, member uuid.UUID, state string) uuid.UUID {
	t.Helper()
	id := uuid.NewString()
	var destination uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.payout_destination (account_id, kind, details_ref, verified_at, verified_method)
		values ($1, 'manual', $2, now(), 'operator-checked') returning id`,
		member, "vault:"+id).Scan(&destination); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}

	// A decided request needs its decider and its date; an undecided one
	// must have neither (withdrawal_request_decision_all_or_none).
	var request uuid.UUID
	if state == "awaiting_approval" {
		if err := tx.QueryRow(ctx, `
			insert into cashback.withdrawal_request
			    (account_id, destination_id, amount_minor, currency, state, reserved_transfer_ref)
			values ($1, $2, 2500, 'EUR', 'awaiting_approval', $3) returning id`,
			member, destination, "reserve-"+id).Scan(&request); err != nil {
			t.Fatalf("seeding the request: %v", err)
		}
		return request
	}
	reason := "null"
	if state == "rejected" {
		reason = "'no'"
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.withdrawal_request
		    (account_id, destination_id, amount_minor, currency, state, reserved_transfer_ref,
		     decided_by, decided_at, decision_reason)
		values ($1, $2, 2500, 'EUR', $3, $4, $1, now(), `+reason+`) returning id`,
		member, destination, state, "reserve-"+id).Scan(&request); err != nil {
		t.Fatalf("seeding the %s request: %v", state, err)
	}
	return request
}

// participationOf reads the member's participation straight from the table.
func participationOf(ctx context.Context, t *testing.T, tx pgx.Tx, member uuid.UUID) (status string, leftAt *time.Time) {
	t.Helper()
	if err := tx.QueryRow(ctx,
		`select status, left_at from cashback.participation where account_id = $1`, member).
		Scan(&status, &leftAt); err != nil {
		t.Fatalf("reading the participation: %v", err)
	}
	return status, leftAt
}

// TestADeletedAccountLeavesCashbackAndItsMoneyStandsStill is the contract's
// own sentence, checked: participation closes, nothing financial moves, and
// the withdrawal that was in flight is still exactly where it was - flagged
// for a person, not resolved by a rule.
func TestADeletedAccountLeavesCashbackAndItsMoneyStandsStill(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := anOptedInMember(ctx, t, tx)
	request := aWithdrawal(ctx, t, tx, member, "awaiting_approval")
	at := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)

	if err := closures(t, tx).Handle(ctx, deletedEvent(t, member, at)); err != nil {
		t.Fatalf("Handle(): %v", err)
	}

	status, leftAt := participationOf(ctx, t, tx, member)
	if status != "left" {
		t.Errorf("the participation is %q, want left", status)
	}
	if leftAt == nil {
		t.Error("the participation closed with no date, which the schema forbids")
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, member); n != 1 {
		t.Errorf("the departure was announced %d time(s), want once", n)
	}

	// The flag, about the request rather than the member: a consumer
	// indexing work by withdrawal finds it where it already looks.
	if n := announced(ctx, t, tx, wallet.TypeWithdrawalFlagged, request); n != 1 {
		t.Fatalf("the withdrawal was flagged %d time(s), want once", n)
	}

	// The money did not move. This is the half the contract is emphatic
	// about: no financial row is deleted or altered in response.
	var state string
	if err := tx.QueryRow(ctx,
		`select state from cashback.withdrawal_request where id = $1`, request).Scan(&state); err != nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if state != "awaiting_approval" {
		t.Errorf("the request is now %q; a deletion decided a payment nobody looked at", state)
	}
}

// TestADeletionDeliveredTwiceSaysEverythingOnce. Delivery is at-least-once,
// so the second pass is the ordinary case and not the exception. A member
// told twice they left, or an operator shown one withdrawal twice, is a
// queue nobody trusts.
func TestADeletionDeliveredTwiceSaysEverythingOnce(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := anOptedInMember(ctx, t, tx)
	request := aWithdrawal(ctx, t, tx, member, "approved")
	at := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)

	consumer := closures(t, tx)
	first := deletedEvent(t, member, at)
	if err := consumer.Handle(ctx, first); err != nil {
		t.Fatalf("the first delivery: %v", err)
	}
	// A redelivery is the same fact with the same payload, which is what
	// the registry re-hands a handler after a failure further along.
	if err := consumer.Handle(ctx, first); err != nil {
		t.Fatalf("the second delivery: %v", err)
	}

	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, member); n != 1 {
		t.Errorf("the departure was announced %d time(s), want once", n)
	}
	if n := announced(ctx, t, tx, wallet.TypeWithdrawalFlagged, request); n != 1 {
		t.Errorf("the withdrawal was flagged %d time(s), want once", n)
	}
}

// TestOnlyMoneyStillMovingIsFlagged. A request that is paid, rejected or
// failed is finished - the money is gone, back, or accounted for - and
// putting a finished payment in front of an operator is work invented.
func TestOnlyMoneyStillMovingIsFlagged(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	at := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		state string
		want  int
	}{
		{"awaiting_approval", 1},
		{"approved", 1},
		{"rejected", 0},
		{"paid", 0},
		{"failed", 0},
	} {
		t.Run("a "+tc.state+" request", func(t *testing.T) {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			defer func() { _ = sub.Rollback(ctx) }()

			member := anOptedInMember(ctx, t, sub)
			request := aWithdrawal(ctx, t, sub, member, tc.state)
			if err := closures(t, sub).Handle(ctx, deletedEvent(t, member, at)); err != nil {
				t.Fatalf("Handle(): %v", err)
			}
			if n := announced(ctx, t, sub, wallet.TypeWithdrawalFlagged, request); n != tc.want {
				t.Errorf("a %s request was flagged %d time(s), want %d", tc.state, n, tc.want)
			}
		})
	}
}

// TestAnAccountThatNeverJoinedCashbackIsNotAFailure. Most deleted accounts
// will be readers who never opted in, and a consumer that errored on them
// would park the whole lane and stop the ones that matter.
func TestAnAccountThatNeverJoinedCashbackIsNotAFailure(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	at := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)

	var stranger uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Never In Cashback', 'reader') returning id`,
		"stranger-"+uuid.NewString()+"@example.test").Scan(&stranger); err != nil {
		t.Fatalf("seeding the stranger: %v", err)
	}

	if err := closures(t, tx).Handle(ctx, deletedEvent(t, stranger, at)); err != nil {
		t.Fatalf("Handle() for an account with no participation: %v", err)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, stranger); n != 0 {
		t.Errorf("a departure was announced %d time(s) for somebody who never arrived", n)
	}
}

// TestADeletionThisConsumerCannotActOnIsParkedRatherThanAcknowledged. A
// payload that cannot be read will never become readable, so the retries
// buy nothing - but acknowledging it would leave a member in cashback with
// no record that anything went wrong. The dead-letter table is where a
// person finds it.
func TestADeletionThisConsumerCannotActOnIsParkedRatherThanAcknowledged(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	consumer := closures(t, tx)

	unreadable := deletedEvent(t, uuid.New(), time.Now())
	unreadable.Payload = []byte(`{"account_id":"not a uuid"}`)
	if err := consumer.Handle(ctx, unreadable); !errors.Is(err, wallet.ErrUnreadableEvent) {
		t.Errorf("Handle() for a malformed payload = %v, want one wrapping %v", err, wallet.ErrUnreadableEvent)
	}

	nameless := deletedEvent(t, uuid.Nil, time.Now())
	if err := consumer.Handle(ctx, nameless); !errors.Is(err, wallet.ErrUnreadableEvent) {
		t.Errorf("Handle() for a deletion naming no account = %v, want one wrapping %v", err, wallet.ErrUnreadableEvent)
	}

	// A type this consumer never subscribed to means the wiring disagrees
	// with the handler, which is worth a loud failure rather than a shrug.
	wrong := deletedEvent(t, uuid.New(), time.Now())
	wrong.Type = "identity.account.created"
	if err := consumer.Handle(ctx, wrong); !errors.Is(err, wallet.ErrUnhandledEvent) {
		t.Errorf("Handle() for another type = %v, want one wrapping %v", err, wallet.ErrUnhandledEvent)
	}
}

// TestTheConsumerNeedsItsParts covers the construction refusals: a consumer
// discovering mid-stream that it cannot read or announce has already
// acknowledged deletions it did nothing about.
func TestTheConsumerNeedsItsParts(t *testing.T) {
	t.Parallel()

	if _, err := wallet.NewAccountClosures(nil, nil, nil, nil); !errors.Is(err, wallet.ErrNoAccountClosures) {
		t.Errorf("NewAccountClosures(nil...) = %v, want one wrapping %v", err, wallet.ErrNoAccountClosures)
	}
}
