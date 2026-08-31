package store_test

// What the settled-payout sum counts, and what it must not (T078).
//
// The figure is the one thing GET /wallet returns that no ledger account
// holds, so it is the one thing a wrong query would get wrong silently: the
// four projected totals disagree with the postings the moment they drift,
// and this one has nothing to disagree with. Every case here is therefore
// about a row the sum must LEAVE OUT.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// schemaTx migrates, connects and opens the outer transaction every case
// runs a savepoint inside. The whole suite rolls back, so a run leaves the
// database exactly as it found it - and a payout row can never be deleted.
func schemaTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise these statements")
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

// tag keeps one case's fixtures from colliding with another's.
func tag(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// operator seeds the named human a payout is approved by.
//
// A second account rather than the member, because the schema refuses
// anything else: payout_insert_guard checks the role, so C-4 is not "an
// approver is recorded" but "an approver who may release money is recorded",
// and a fixture that could approve its own payout would be testing against a
// schema nobody deploys.
func operator(ctx context.Context, t *testing.T, tx pgx.Tx) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Paying Operator', 'operator') returning id`,
		"operator-"+tag(t)+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	return id
}

// member seeds an account and a verified destination to be paid at.
func member(ctx context.Context, t *testing.T, tx pgx.Tx) (account, destination pgtype.UUID) {
	t.Helper()
	id := tag(t)
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Paid Member', 'reader') returning id`,
		"paid-"+id+"@example.test").Scan(&account); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.payout_destination (account_id, kind, details_ref, verified_at, verified_method)
		values ($1, 'manual', $2, now(), 'operator-checked') returning id`,
		account, "vault:"+id).Scan(&destination); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}
	return account, destination
}

// paid records a withdrawal that was approved and a payout in the given
// state, and answers nothing: every case asserts through the sum.
func paid(ctx context.Context, t *testing.T, tx pgx.Tx, account, destination pgtype.UUID, minor int64, currency, state string) {
	t.Helper()
	approver := operator(ctx, t, tx)
	var request pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.withdrawal_request
		    (account_id, destination_id, amount_minor, currency, state, reserved_transfer_ref, decided_by, decided_at)
		values ($1, $2, $3, $4, 'approved', $5, $6, now()) returning id`,
		account, destination, minor, currency, "reserve-"+tag(t), approver).Scan(&request); err != nil {
		t.Fatalf("seeding the request: %v", err)
	}
	settled := "now()"
	if state != "settled" {
		settled = "null"
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.payout
		    (brand_id, request_id, approved_by, amount_minor, currency, rail, state, settled_at)
		values ('apivo-de', $1, $2, $3, $4, 'manual', $5, `+settled+`)`,
		request, approver, minor, currency, state); err != nil {
		t.Fatalf("seeding the %s payout: %v", state, err)
	}
}

func sum(ctx context.Context, t *testing.T, tx pgx.Tx, account pgtype.UUID, currency string) int64 {
	t.Helper()
	total, err := store.New(tx).SettledPayoutsFor(ctx, store.SettledPayoutsForParams{
		AccountID: account,
		Currency:  currency,
	})
	if err != nil {
		t.Fatalf("SettledPayoutsFor(): %v", err)
	}
	return total
}

func TestSettledPayoutsForAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)

	each := func(name string, scenario func(t *testing.T, tx pgx.Tx)) {
		t.Run(name, func(t *testing.T) {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			defer func() { _ = sub.Rollback(ctx) }()
			scenario(t, sub)
		})
	}

	// A member who has never been paid reads as zero, not as null and not as
	// an error. It is everybody's wallet before their first withdrawal.
	each("a member who has never been paid is paid nothing", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		if got := sum(ctx, t, tx, account, "EUR"); got != 0 {
			t.Errorf("paid = %d, want 0", got)
		}
	})

	each("settled payouts add up", func(t *testing.T, tx pgx.Tx) {
		account, destination := member(ctx, t, tx)
		paid(ctx, t, tx, account, destination, 2500, "EUR", "settled")
		paid(ctx, t, tx, account, destination, 1000, "EUR", "settled")
		if got := sum(ctx, t, tx, account, "EUR"); got != 3500 {
			t.Errorf("paid = %d, want 3500", got)
		}
	})

	// Money in flight. Counting it would tell a member they had been paid
	// before the rail said so, which is the number they would then chase.
	each("a submitted payout is not paid yet", func(t *testing.T, tx pgx.Tx) {
		account, destination := member(ctx, t, tx)
		paid(ctx, t, tx, account, destination, 2500, "EUR", "submitted")
		if got := sum(ctx, t, tx, account, "EUR"); got != 0 {
			t.Errorf("paid = %d, want 0: the rail has not settled it", got)
		}
	})

	// Money that came back. Counting it would tell a member they had been
	// paid despite the rail saying otherwise.
	each("a failed payout was never paid", func(t *testing.T, tx pgx.Tx) {
		account, destination := member(ctx, t, tx)
		paid(ctx, t, tx, account, destination, 2500, "EUR", "failed")
		if got := sum(ctx, t, tx, account, "EUR"); got != 0 {
			t.Errorf("paid = %d, want 0: the rail refused it", got)
		}
	})

	// Currencies do not net against each other (C-6). Asked for euros, a
	// member paid in zloty has been paid no euros.
	each("another currency is another total", func(t *testing.T, tx pgx.Tx) {
		account, destination := member(ctx, t, tx)
		paid(ctx, t, tx, account, destination, 2500, "EUR", "settled")
		paid(ctx, t, tx, account, destination, 9900, "PLN", "settled")
		if got := sum(ctx, t, tx, account, "EUR"); got != 2500 {
			t.Errorf("paid in EUR = %d, want 2500", got)
		}
		if got := sum(ctx, t, tx, account, "PLN"); got != 9900 {
			t.Errorf("paid in PLN = %d, want 9900", got)
		}
	})

	// The join is what scopes the sum to one member. Without it every
	// member's wallet would show the sum of everybody's payouts.
	each("another member's payouts are theirs", func(t *testing.T, tx pgx.Tx) {
		account, destination := member(ctx, t, tx)
		other, othersDestination := member(ctx, t, tx)
		paid(ctx, t, tx, account, destination, 2500, "EUR", "settled")
		paid(ctx, t, tx, other, othersDestination, 7700, "EUR", "settled")
		if got := sum(ctx, t, tx, account, "EUR"); got != 2500 {
			t.Errorf("paid = %d, want 2500: the other member's 7700 is not this member's", got)
		}
	})
}
