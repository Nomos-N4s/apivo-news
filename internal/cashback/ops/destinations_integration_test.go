package ops_test

// Verification against the real schema (B6, #548, FR-051 with FR-061).
//
// The unit tests above prove the wire; only this can prove the row, the
// guard that freezes it, and that the fact and its event commit together.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// destinationsTx opens the outer transaction every case runs a savepoint
// inside, and rolls the whole suite back.
func destinationsTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise verification")
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

// aDestination seeds a member and one unverified destination of theirs.
func aDestination(ctx context.Context, t *testing.T, tx pgx.Tx) (member, destination uuid.UUID) {
	t.Helper()
	suffix := uuid.NewString()
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'A Paid Member', 'reader') returning id`,
		"paid-"+suffix+"@example.test").Scan(&member); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.payout_destination (account_id, kind, details_ref)
		values ($1, 'sepa', $2) returning id`,
		member, "openbao:secret/cashback/payout-destinations/"+suffix).Scan(&destination); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}
	return member, destination
}

// anOperatorAccount seeds somebody who may verify.
func anOperatorAccount(ctx context.Context, t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'An Operator', 'operator') returning id`,
		"ops-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	return id
}

func TestVerificationAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := destinationsTx(t)

	each(ctx, t, tx, "a verification names its operator, its method and its instant", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		member, destination := aDestination(ctx, t, tx)
		operator := anOperatorAccount(ctx, t, tx)

		verified, err := store.Verify(ctx, ops.Verification{
			ID: destination, Operator: ops.Operator{ID: operator}, Method: "operator: confirmed on a support call",
		})
		if err != nil {
			t.Fatalf("Verify(): %v", err)
		}
		if verified.AccountID != member {
			t.Errorf("the verification names member %s, want %s", verified.AccountID, member)
		}
		if verified.VerifiedBy != operator {
			t.Errorf("verified_by = %s, want the acting operator %s", verified.VerifiedBy, operator)
		}
		if verified.VerifiedAt.IsZero() {
			t.Error("the verification carries no instant, so nothing says when the proof was recorded")
		}

		// The row, not the value this method returned: FR-061 is about what
		// an auditor can read later.
		var by uuid.UUID
		var method string
		if err := tx.QueryRow(ctx,
			`select verified_by, verified_method from cashback.payout_destination where id = $1`,
			destination).Scan(&by, &method); err != nil {
			t.Fatalf("reading the row back: %v", err)
		}
		if by != operator || method != "operator: confirmed on a support call" {
			t.Errorf("the row records %s/%q, want %s and the method as written", by, method, operator)
		}

		// The event and the fact are one commit. A verification nobody
		// heard about is one no consumer can act on.
		var announced int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event where type = $1 and subject = $2`,
			ops.TypeDestinationVerified, destination.String()).Scan(&announced); err != nil {
			t.Fatalf("counting events: %v", err)
		}
		if announced != 1 {
			t.Errorf("the verification was announced %d time(s), want once", announced)
		}
	})

	each(ctx, t, tx, "a second verification keeps the first and announces nothing", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		_, destination := aDestination(ctx, t, tx)
		first := anOperatorAccount(ctx, t, tx)
		second := anOperatorAccount(ctx, t, tx)

		if _, err := store.Verify(ctx, ops.Verification{
			ID: destination, Operator: ops.Operator{ID: first}, Method: "operator: the first proof",
		}); err != nil {
			t.Fatalf("the first verification: %v", err)
		}
		again, err := store.Verify(ctx, ops.Verification{
			ID: destination, Operator: ops.Operator{ID: second}, Method: "operator: a later look",
		})
		if err != nil {
			t.Fatalf("the second verification: %v", err)
		}

		// One-way and final: the answer names who actually did it, so the
		// second operator can see it was somebody else.
		if again.VerifiedBy != first {
			t.Errorf("the standing verification names %s, want the first operator %s", again.VerifiedBy, first)
		}
		if again.Method != "operator: the first proof" {
			t.Errorf("the method reads %q, want the first operator's", again.Method)
		}
		var announced int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event where type = $1 and subject = $2`,
			ops.TypeDestinationVerified, destination.String()).Scan(&announced); err != nil {
			t.Fatalf("counting events: %v", err)
		}
		if announced != 1 {
			t.Errorf("the fact was announced %d time(s), want once however many times it is asked", announced)
		}
	})

	each(ctx, t, tx, "an id naming nothing is a not-found", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		if _, err := store.Verify(ctx, ops.Verification{
			ID: uuid.New(), Operator: ops.Operator{ID: uuid.New()}, Method: "operator: checked",
		}); !errors.Is(err, ops.ErrNoSuchDestination) {
			t.Fatalf("Verify() = %v, want one wrapping %v", err, ops.ErrNoSuchDestination)
		}
	})

	each(ctx, t, tx, "the queue holds what is unverified and drops what is not", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		_, waiting := aDestination(ctx, t, tx)
		_, done := aDestination(ctx, t, tx)
		operator := anOperatorAccount(ctx, t, tx)
		if _, err := store.Verify(ctx, ops.Verification{
			ID: done, Operator: ops.Operator{ID: operator}, Method: "operator: checked",
		}); err != nil {
			t.Fatalf("verifying the second: %v", err)
		}

		queue, err := store.UnverifiedDestinations(ctx, ops.DestinationAfter{}, 100)
		if err != nil {
			t.Fatalf("UnverifiedDestinations(): %v", err)
		}
		var sawWaiting, sawDone bool
		for _, row := range queue {
			switch row.ID {
			case waiting:
				sawWaiting = true
				if row.AccountEmail == "" {
					t.Error("the queue row carries no way to reach the member")
				}
				if row.DetailsRef == "" {
					t.Error("the queue row carries no reference, so an operator cannot open the details")
				}
			case done:
				sawDone = true
			}
		}
		if !sawWaiting {
			t.Error("a destination nobody has verified is missing from the queue")
		}
		if sawDone {
			t.Error("a verified destination is still queued as work")
		}
	})

	each(ctx, t, tx, "a page of nothing is refused rather than read as an empty queue", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		if _, err := store.UnverifiedDestinations(ctx, ops.DestinationAfter{}, 0); err == nil {
			t.Fatal("a page of nothing was accepted")
		}
	})
}
