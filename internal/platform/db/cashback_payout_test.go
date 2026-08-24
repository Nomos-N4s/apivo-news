package db_test

// These tests assert the two invariants that stand between a member and
// being paid twice, or paid by nobody:
//
//	C-4  a payout row cannot exist without a non-null named human approver
//	C-5  every outbound payout carries a unique idempotency key, derived
//	     deterministically from the withdrawal request
//
// C-5 gets a real concurrency test, not a sequential one. "A retry cannot
// create a second payout" is a claim about two sessions racing, and a
// single rolled-back transaction cannot see a unique index doing its job
// under contention.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCashbackPayoutRejectsIllegalWrites is the C-4 and C-5 rejection
// table, plus the destination and request rules that lead up to them.
func TestCashbackPayoutRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "payout with no approver",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, null, 250, 'EUR', 'manual')`, f.requestID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "payout approved by an account that does not exist",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// C-4 says a NAMED HUMAN. A uuid that names nobody is not
				// one, and the foreign key is what says so.
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, gen_random_uuid(), 250, 'EUR', 'manual')`, f.requestID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "a second payout for one request",
			rule: "C-5",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				const pay = `insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
				             values ('fixture', $1, $2, 250, 'EUR', 'manual')`
				if _, err := tx.Exec(ctx, pay, f.requestID, f.approverID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, pay, f.requestID, f.approverID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "payout carrying a caller-chosen idempotency key",
			rule: "C-5",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A caller-chosen key is exactly how a retry stops being
				// one, so the column refuses to be written at all (D8).
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, idempotency_key, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 'attempt-2', 250, 'EUR', 'manual')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeGeneratedAlways,
		},
		{
			name: "payout for a request that does not exist",
			rule: "C-5",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', gen_random_uuid(), $1, 250, 'EUR', 'manual')`, f.approverID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "payout of more than was requested",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The approval was for an amount. Paying a different one is
				// paying something nobody approved.
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 25000, 'EUR', 'manual')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "payout in a different currency from the request",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'USD', 'manual')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "payout with no brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The payout descriptor, the legal entity and the rail
				// account are all brand-scoped: a payment nobody attributed
				// to a brand is a payment nobody can account for.
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (request_id, approved_by, amount_minor, currency, rail)
					 values ($1, $2, 250, 'EUR', 'manual')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "payout with a blank brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('  ', $1, $2, 250, 'EUR', 'manual')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "payout on no rail at all",
			rule: "FR-052",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'EUR', '  ')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "payout settled at no particular time",
			rule: "FR-053",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail, state)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual', 'settled')`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "payout with a settlement time but not settled",
			rule: "FR-053",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail, state, settled_at)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual', 'failed', now())`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "withdrawal naming an unverified destination",
			rule: "FR-051",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				var unverified string
				if err := tx.QueryRow(ctx,
					`insert into cashback.payout_destination (account_id, kind, details_ref)
					 values ($1, 'sepa', $2) returning id`,
					f.accountID, "vault/unverified/"+f.suffix).Scan(&unverified); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.withdrawal_request
					     (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
					 values ($1, $2, 250, 'EUR', $3)`,
					f.accountID, unverified, "reserve-unverified-"+f.suffix)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "withdrawal to somebody else's destination",
			rule: "FR-051",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A verified destination is verified for its owner, not for
				// whoever names it.
				var thief string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ($1, 'Other Member') returning id`,
					"thief-"+f.suffix+"@example.test").Scan(&thief); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.withdrawal_request
					     (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
					 values ($1, $2, 250, 'EUR', $3)`,
					thief, f.destinationID, "reserve-thief-"+f.suffix)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "withdrawal that reserved nothing",
			rule: "D9",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Without the reservation the double-spend window between
				// request and approval is wide open.
				_, err := tx.Exec(ctx,
					`insert into cashback.withdrawal_request
					     (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
					 values ($1, $2, 250, 'EUR', null)`, f.accountID, f.destinationID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "two withdrawals sharing one reservation",
			rule: "D9",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.withdrawal_request
					     (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
					 values ($1, $2, 100, 'EUR', $3)`,
					f.accountID, f.destinationID, "reserve-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "request approved by nobody",
			rule: "FR-060",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`update cashback.withdrawal_request set state = 'approved' where id = $1`, f.requestID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "request rejected with no reason",
			rule: "FR-060",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`update cashback.withdrawal_request
					    set state = 'rejected', decided_by = $2, decided_at = now()
					  where id = $1`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "request decided before it was made",
			rule: "FR-060",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`update cashback.withdrawal_request
					    set state = 'approved', decided_by = $2, decided_at = requested_at - interval '1 hour'
					  where id = $1`, f.requestID, f.approverID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "destination verified by no stated method",
			rule: "FR-051",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout_destination (account_id, kind, details_ref, verified_at)
					 values ($1, 'sepa', $2, now())`, f.accountID, "vault/half/"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "destination re-pointed after verification",
			rule: "FR-051",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`update cashback.payout_destination set details_ref = 'vault/elsewhere' where id = $1`,
					f.destinationID)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "destination quietly un-verified",
			rule: "FR-051",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`update cashback.payout_destination set verified_at = null, verified_method = null where id = $1`,
					f.destinationID)
				return err
			},
			wantCode: codeRaiseException,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackWithdrawal(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, f), tt.wantCode)
		})
	}
}

// TestPayoutApprovalIsFrozen asserts that C-4 is a rule about the row, not
// only about the INSERT that created it. A payout that could be updated to
// name a different approver, or to pay a different amount on a different
// rail, would make every check that ran at insert time a statement about a
// row that no longer exists - and 0019's operator-role check, which also
// runs on INSERT, would be bypassable the same way.
func TestPayoutApprovalIsFrozen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "reassign the approver", stmt: `update cashback.payout set approved_by = gen_random_uuid() where id = $1`},
		{name: "point at another request", stmt: `update cashback.payout set request_id = gen_random_uuid() where id = $1`},
		{name: "pay a different amount", stmt: `update cashback.payout set amount_minor = 99999 where id = $1`},
		{name: "pay in a different currency", stmt: `update cashback.payout set currency = 'USD' where id = $1`},
		{name: "move it to another rail", stmt: `update cashback.payout set rail = 'sepa' where id = $1`},
		{name: "reattribute it to another brand", stmt: `update cashback.payout set brand_id = 'other' where id = $1`},
		{name: "backdate the submission", stmt: `update cashback.payout set submitted_at = now() - interval '1 year' where id = $1`},
		{name: "delete the payout", stmt: `delete from cashback.payout where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			f := seedCashbackWithdrawal(t, tx)

			var payoutID string
			if err := tx.QueryRow(ctx,
				`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
				 values ('fixture', $1, $2, 250, 'EUR', 'manual') returning id`,
				f.requestID, f.approverID).Scan(&payoutID); err != nil {
				t.Fatalf("a valid payout was rejected: %v", err)
			}

			_, err := tx.Exec(ctx, tt.stmt, payoutID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestSettledPayoutIsTerminal is the other half of the guard, and the half
// with money on it: once a rail says the money left, that cannot be walked
// back into a state something else might act on.
func TestSettledPayoutIsTerminal(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	var payoutID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual') returning id`,
		f.requestID, f.approverID).Scan(&payoutID); err != nil {
		t.Fatalf("a valid payout was rejected: %v", err)
	}

	// The rail outcome is exactly what may still move.
	if _, err := tx.Exec(ctx,
		`update cashback.payout set state = 'settled', settled_at = now(), rail_reference = 'bank-ref-1' where id = $1`,
		payoutID); err != nil {
		t.Fatalf("recording the rail outcome was rejected: %v", err)
	}

	_, err := tx.Exec(ctx, `update cashback.payout set state = 'failed' where id = $1`, payoutID)
	wantPgCode(t, err, codeRaiseException)
}

// TestPayoutIdempotencyKeyIsDerivedFromTheRequest asserts D8 as a property
// of the schema rather than of the caller: the key is a function of the
// request id, so two attempts at the same payout cannot produce two keys.
func TestPayoutIdempotencyKeyIsDerivedFromTheRequest(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	var key string
	err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual') returning idempotency_key`,
		f.requestID, f.approverID).Scan(&key)
	if err != nil {
		t.Fatalf("a valid payout was rejected: %v", err)
	}
	if key == "" {
		t.Fatal("the payout carries no idempotency key")
	}

	var derived string
	if err := tx.QueryRow(ctx, `select 'payout:' || $1::text`, f.requestID).Scan(&derived); err != nil {
		t.Fatalf("deriving the expected key: %v", err)
	}
	if key != derived {
		t.Fatalf("idempotency key is %q, want %q derived from the request: a retry could mint a different one", key, derived)
	}
}

// codeLockNotAvailable is what a statement raises when it waits on a lock
// past its lock_timeout. Used here to prove contention positively, rather
// than inferring it from a statement not having finished yet.
const codeLockNotAvailable = "55P03"

// TestConcurrentDoubleSubmitProducesOnePayout is C-5 under the only
// conditions it matters: two sessions submitting the same payout at the
// same time. Exactly one row exists afterwards and the loser sees a unique
// violation, which is the invariant map's "concurrent double submit -> one
// row, one SQLSTATE 23505".
//
// The contention is PROVED, not timed. An earlier version waited 300ms and
// concluded from the retry not having finished that it must be blocked -
// an inference from absence, flaky on a loaded runner, and unable to tell
// "blocked" from "slow". Session B now sets a lock_timeout and asserts it
// fails with 55P03: a statement can only time out on a lock it was
// actually waiting for, so the wait itself becomes the assertion.
//
// The race needs two real sessions and real commits; a unique index cannot
// be seen doing its job inside one rolled-back transaction. The committed
// rows stay in the test database on purpose: the evidence they rest on -
// click, network_transaction, entry - is protected by immutability
// triggers, so the suite cannot delete them even if it wanted to. Random
// suffixes keep runs independent.
func TestConcurrentDoubleSubmitProducesOnePayout(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise schema invariants")
	}
	ctx := context.Background()

	connA, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session A: %v", err)
	}
	defer func() { _ = connA.Close(ctx) }()
	connB, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session B: %v", err)
	}
	defer func() { _ = connB.Close(ctx) }()

	// Seed and COMMIT, so both sessions can see the request.
	seedTx, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	f := seedCashbackWithdrawal(t, seedTx)
	if err := seedTx.Commit(ctx); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	const pay = `insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
	             values ('fixture', $1, $2, 250, 'EUR', 'manual')`

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin submit A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := txA.Exec(ctx, pay, f.requestID, f.approverID); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// The retry must WAIT on the first submit's uncommitted key. A
	// lock_timeout turns that wait into an assertion: 55P03 can only be
	// raised by a statement that was genuinely blocked.
	if _, err := connB.Exec(ctx, `set lock_timeout = '2s'`); err != nil {
		t.Fatalf("setting lock_timeout on session B: %v", err)
	}
	_, err = connB.Exec(ctx, pay, f.requestID, f.approverID)
	wantPgCode(t, err, codeLockNotAvailable)

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit submit A: %v", err)
	}

	// With the first payout committed there is nothing left to wait for,
	// and the retry fails on the key itself.
	_, err = connB.Exec(ctx, pay, f.requestID, f.approverID)
	wantPgCode(t, err, codeUniqueViolation)

	var payouts int
	if err := connA.QueryRow(ctx,
		`select count(*) from cashback.payout where request_id = $1`, f.requestID).Scan(&payouts); err != nil {
		t.Fatalf("counting payouts: %v", err)
	}
	if payouts != 1 {
		t.Fatalf("%d payouts for one request after a concurrent double submit, want exactly 1", payouts)
	}
}

// TestValidPayoutChainIsAccepted is the positive control: a verified
// destination, a reserved request, an approval, a settlement. If this
// failed, the rejection table above would be proving nothing.
func TestValidPayoutChainIsAccepted(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	if _, err := tx.Exec(ctx,
		`update cashback.withdrawal_request
		    set state = 'approved', decided_by = $2, decided_at = now(), decision_reason = 'balance confirmed'
		  where id = $1`, f.requestID, f.approverID); err != nil {
		t.Fatalf("approving the request: %v", err)
	}

	var payoutID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail, rail_reference)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual', 'bank-ref-1') returning id`,
		f.requestID, f.approverID).Scan(&payoutID); err != nil {
		t.Fatalf("a valid payout was rejected: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`update cashback.payout set state = 'settled', settled_at = now() where id = $1`, payoutID); err != nil {
		t.Fatalf("settling the payout: %v", err)
	}

	var approver, state string
	if err := tx.QueryRow(ctx,
		`select a.display_name, p.state
		   from cashback.payout p
		   join account a on a.id = p.approved_by
		  where p.id = $1`, payoutID).Scan(&approver, &state); err != nil {
		t.Fatalf("reading the settled payout: %v", err)
	}
	if approver == "" {
		t.Fatal("the settled payout names no approver")
	}
	if state != "settled" {
		t.Fatalf("payout state is %q, want settled", state)
	}
}
