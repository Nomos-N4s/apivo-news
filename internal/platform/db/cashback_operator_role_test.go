package db_test

// C-4 tightened. 0014 made an UNAPPROVED payout unrepresentable with a NOT
// NULL column; 0019 makes an UNAUTHORISED approver unrepresentable too.
// These tests assert both halves, and the concurrency story behind the
// second one.
//
// The race test is the reason payout_insert_guard reads the approver's role
// FOR SHARE rather than with a plain snapshot read. Without the lock, an
// in-flight payout and a concurrent demotion of its approver can each pass
// their own check against the prior state and both commit - recording a
// reader as having released money. That failure is invisible to any
// sequential test, which is why this one uses two real sessions.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPayoutApproverMustHoldTheOperatorRole is the C-4 rejection table.
func TestPayoutApproverMustHoldTheOperatorRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "payout approved by a reader",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				var reader string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ($1, 'Ordinary Member') returning id`,
					"reader-"+f.suffix+"@example.test").Scan(&reader); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual')`, f.requestID, reader)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "payout approved by an editor",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Editorial authority is not money authority. An editor can
				// publish an article and still may not release a payment.
				var editor string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name, role) values ($1, 'Test Editor', 'editor') returning id`,
					"editor-"+f.suffix+"@example.test").Scan(&editor); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual')`, f.requestID, editor)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "payout with no approver keeps its own SQLSTATE",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The guard deliberately falls through on a null approver so
				// the NOT NULL keeps reporting the failure it is: only the
				// ROLE rule lives in the trigger.
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, null, 250, 'EUR', 'manual')`, f.requestID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "payout approved by nobody at all keeps its own SQLSTATE",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, gen_random_uuid(), 250, 'EUR', 'manual')`, f.requestID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "an unknown role",
			rule: "0019",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into account (email, display_name, role) values ($1, 'Impostor', 'administrator')`,
					"admin-"+f.suffix+"@example.test")
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "renaming the approver after the fact",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The role check runs on INSERT. If approved_by could be
				// updated afterwards, that check would be a statement about
				// a row that no longer exists - so 0014's payout_guard
				// freezing the column is half of this invariant, not a
				// separate tidiness rule.
				var reader string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ($1, 'Ordinary Member') returning id`,
					"rename-"+f.suffix+"@example.test").Scan(&reader); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual')`, f.requestID, f.approverID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`update cashback.payout set approved_by = $2 where request_id = $1`, f.requestID, reader)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "demoting an operator who has released money",
			rule: "C-4",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
					 values ('fixture', $1, $2, 250, 'EUR', 'manual')`, f.requestID, f.approverID); err != nil {
					return err
				}
				// The payout is on record; the authority behind it can no
				// longer be taken away, or the C-4 chain would point at an
				// account whose right to approve is undemonstrable.
				_, err := tx.Exec(ctx,
					`update account set role = 'reader' where id = $1`, f.approverID)
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

// TestOperatorMayReleaseMoneyAndBeDemotedIfTheyNeverDid is the positive
// control on both sides: an operator's payout is accepted, and an operator
// with nothing on record is still an ordinary account whose role can move.
func TestOperatorMayReleaseMoneyAndBeDemotedIfTheyNeverDid(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	var payoutID string
	if err := tx.QueryRow(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual') returning id`,
		f.requestID, f.approverID).Scan(&payoutID); err != nil {
		t.Fatalf("an operator-approved payout was rejected: %v", err)
	}

	var idleOperator string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, 'Idle Operator', 'operator') returning id`,
		"idle-"+f.suffix+"@example.test").Scan(&idleOperator); err != nil {
		t.Fatalf("seeding an operator: %v", err)
	}
	if _, err := tx.Exec(ctx, `update account set role = 'reader' where id = $1`, idleOperator); err != nil {
		t.Fatalf("demoting an operator with nothing on record was rejected: %v", err)
	}

	var approverRole string
	if err := tx.QueryRow(ctx,
		`select a.role from cashback.payout p join account a on a.id = p.approved_by where p.id = $1`,
		payoutID).Scan(&approverRole); err != nil {
		t.Fatalf("reading the payout's approver: %v", err)
	}
	if approverRole != "operator" {
		t.Fatalf("the payout's approver holds %q, want operator", approverRole)
	}
}

// TestOperatorDemotionRaceIsSerialized is the reason for the FOR SHARE
// read. Without it, an in-flight payout and a concurrent demotion of its
// approver could each pass their own check against the prior state and
// both commit, recording a reader as having released money.
//
// The contention is PROVED, not timed. An earlier version waited 300ms and
// concluded from the demotion not having finished that it must be blocked
// - an inference from absence, flaky on a loaded runner, and unable to
// tell "blocked" from "slow". Session B now sets a lock_timeout and must
// fail with 55P03: a statement can only time out on a lock it was
// genuinely waiting for, so the wait is the assertion. That also proves
// something the sleep never could - that the lock being waited on is the
// approver's account row, taken by the payout trigger's FOR SHARE read.
//
// The race needs two real sessions and real commits; row locks are
// invisible inside a single rolled-back transaction. The committed rows
// stay in the test database on purpose: the evidence they rest on is
// protected by immutability triggers, so the suite could not delete them
// even if it wanted to. Random suffixes keep runs independent.
func TestOperatorDemotionRaceIsSerialized(t *testing.T) {
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

	seedTx, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	f := seedCashbackWithdrawal(t, seedTx)
	if err := seedTx.Commit(ctx); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin payout tx: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	// The insert trigger takes FOR SHARE on the approver's account row.
	if _, err := txA.Exec(ctx,
		`insert into cashback.payout (brand_id, request_id, approved_by, amount_minor, currency, rail)
		 values ('fixture', $1, $2, 250, 'EUR', 'manual')`, f.requestID, f.approverID); err != nil {
		t.Fatalf("payout insert: %v", err)
	}

	// The demotion must WAIT on that share lock. lock_timeout turns the
	// wait into an assertion.
	if _, err := connB.Exec(ctx, `set lock_timeout = '2s'`); err != nil {
		t.Fatalf("setting lock_timeout on session B: %v", err)
	}
	_, err = connB.Exec(ctx, `update account set role = 'reader' where id = $1`, f.approverID)
	wantPgCode(t, err, codeLockNotAvailable)

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit payout: %v", err)
	}

	// Nothing left to wait for. account_role_guard now sees the committed
	// payout and refuses the demotion on its merits.
	_, err = connB.Exec(ctx, `update account set role = 'reader' where id = $1`, f.approverID)
	wantPgCode(t, err, codeRaiseException)

	var role string
	if err := connA.QueryRow(ctx, `select role from account where id = $1`, f.approverID).Scan(&role); err != nil {
		t.Fatalf("reading the approver's role: %v", err)
	}
	if role != "operator" {
		t.Fatalf("the approver of a committed payout now holds %q: the authority behind released money is no longer demonstrable", role)
	}
}
