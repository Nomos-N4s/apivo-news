package postgres_test

// These tests assert that the DATABASE enforces the exit-route ledger's
// invariants (migration 0022_pg_ledger). The adapter is deliberately
// bypassed: every test here writes SQL straight at the schema and requires
// Postgres itself to reject the illegal state with a specific SQLSTATE,
// because the whole point of the exit route is that money correctness does
// not depend on the Go code being the only writer (ADR-0002, C-1).
//
//	C-1  a transfer's postings sum to zero per currency -> the deferred
//	     trigger raises at COMMIT
//	C-5  one recording per idempotency key -> unique violation
//	C-6  integer minor units, format-checked currency, no fractional
//	     money type anywhere in the schema
//	     postings and transfers are financial history -> immutable
//	D9   a member stage account never holds less than nothing -> the
//	     second deferred trigger raises at COMMIT; house accounts are
//	     exempt
//
// The synthetic accounts these tests write are house accounts, and
// deliberately: every one of them is about the zero-sum, uniqueness and
// immutability rules, several drive an account negative to get there, and
// the house kind is the one those assertions have always assumed. The
// member kind has its own test below, which is where being refused for
// going negative is the point rather than the noise.
//
// Like the behaviour suite beside it they run against a real Postgres,
// keyed on DATABASE_URL, and clean up by rolling back or by failing the
// very commit under test.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes the assertions expect.
const (
	codeForeignKeyViolation = "23503"
	codeUniqueViolation     = "23505"
	codeCheckViolation      = "23514"
	codeRaiseException      = "P0001"
)

// beginTx opens a transaction that is always rolled back, keeping tests
// independent and the database clean.
func beginTx(t *testing.T) pgx.Tx {
	t.Helper()
	pool := requirePool(t)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func wantPgCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want SQLSTATE %s, but the database accepted the write", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

// seedSQL writes a balanced transfer straight in SQL - two accounts, one
// transfer, two postings - and returns the ids involved. It exists so the
// illegal-write tests have legal rows to attack.
type seededTransfer struct {
	give, take  string
	transferRef string
}

func seedSQL(t *testing.T, tx pgx.Tx) seededTransfer {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)
	s := seededTransfer{
		give: "sql/give/" + suffix,
		take: "sql/take/" + suffix,
	}
	if _, err := tx.Exec(ctx,
		`insert into ledger.account (id, currency, kind) values ($1, 'EUR', 'house'), ($2, 'EUR', 'house')`,
		s.give, s.take); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
		"sql-"+suffix).Scan(&s.transferRef); err != nil {
		t.Fatalf("seed transfer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
		 values ($1, $2, -100, 'EUR'), ($1, $3, 100, 'EUR')`,
		s.transferRef, s.give, s.take); err != nil {
		t.Fatalf("seed postings: %v", err)
	}
	return s
}

// TestDatabaseRefusesAnUnbalancedTransferAtCommit is C-1 with the port
// bypassed entirely: postings written straight in SQL that do not sum to
// zero must be refused by the deferred trigger when the transaction tries
// to COMMIT - the one moment all of a transfer's postings are on the
// table. The commit fails, so nothing is left behind.
func TestDatabaseRefusesAnUnbalancedTransferAtCommit(t *testing.T) {
	t.Parallel()

	t.Run("a lone posting creates money and is refused", func(t *testing.T) {
		t.Parallel()
		pool := requirePool(t)
		ctx := context.Background()

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		s := seedSQL(t, tx)
		var lone string
		if err := tx.QueryRow(ctx,
			`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
			"sql-unbalanced-"+randomSuffix(t)).Scan(&lone); err != nil {
			t.Fatalf("insert transfer: %v", err)
		}
		// The insert itself succeeds: the trigger is deferred so the
		// postings of a transfer may arrive in any order, and nothing can
		// be judged before COMMIT.
		if _, err := tx.Exec(ctx,
			`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
			 values ($1, $2, 55, 'EUR')`, lone, s.take); err != nil {
			t.Fatalf("an unbalanced posting must be accepted until commit: %v", err)
		}
		wantPgCode(t, tx.Commit(ctx), codeRaiseException)
	})

	t.Run("currencies are judged independently", func(t *testing.T) {
		t.Parallel()
		pool := requirePool(t)
		ctx := context.Background()

		// EUR balances; GBP does not. A trigger that netted across
		// currencies - or only checked the first - would let this commit.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		suffix := randomSuffix(t)
		if _, err := tx.Exec(ctx,
			`insert into ledger.account (id, currency, kind)
			 values ($1, 'EUR', 'house'), ($2, 'EUR', 'house'), ($3, 'GBP', 'house')`,
			"sql/eur-a/"+suffix, "sql/eur-b/"+suffix, "sql/gbp/"+suffix); err != nil {
			t.Fatalf("seed accounts: %v", err)
		}
		var ref string
		if err := tx.QueryRow(ctx,
			`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
			"sql-mixed-"+suffix).Scan(&ref); err != nil {
			t.Fatalf("insert transfer: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
			 values ($1, $2, -70, 'EUR'), ($1, $3, 70, 'EUR'), ($1, $4, 12, 'GBP')`,
			ref, "sql/eur-a/"+suffix, "sql/eur-b/"+suffix, "sql/gbp/"+suffix); err != nil {
			t.Fatalf("staging the postings: %v", err)
		}
		wantPgCode(t, tx.Commit(ctx), codeRaiseException)
	})
}

// TestDatabaseRefusesASecondUseOfAnIdempotencyKey is C-5's database half:
// the unique constraint, not adapter logic, is what makes one key one
// recording - which is also exactly how the adapter resolves the
// concurrent same-key race.
func TestDatabaseRefusesASecondUseOfAnIdempotencyKey(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	key := "sql-dup-" + randomSuffix(t)
	if _, err := tx.Exec(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1)`, key); err != nil {
		t.Fatalf("first use of the key: %v", err)
	}
	_, err := tx.Exec(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1)`, key)
	wantPgCode(t, err, codeUniqueViolation)
}

// TestLedgerRowsAreImmutable asserts that postings, transfers and
// accounts, once written, are beyond UPDATE, DELETE and TRUNCATE: they are
// financial history, and a correction is a reversing transfer. Each
// attempt runs in a savepoint so the raised exception does not abort the
// enclosing rolled-back transaction.
//
// Deliberately NOT parallel, alone in this file. Its TRUNCATE attempts
// take ACCESS EXCLUSIVE on ledger.transfer and then, through the cascade,
// on ledger.posting - the opposite order from the one History's join reads
// those two tables in - and a pair of statements that take one pair of
// table locks in opposite orders deadlocks whenever they overlap. They
// cannot overlap if this test never runs beside another: Go releases the
// parallel tests only once the sequential ones have finished.
func TestLedgerRowsAreImmutable(t *testing.T) {
	tx := beginTx(t)
	ctx := context.Background()
	s := seedSQL(t, tx)

	// Flush the zero-sum trigger's pending events now (the seed balances,
	// so they pass): TRUNCATE refuses to run over pending trigger events
	// with 55006, and this test is about the immutability guard's P0001,
	// not about the queue in front of it.
	if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
		t.Fatalf("flushing deferred constraints: %v", err)
	}

	frozen := []struct {
		name string
		stmt string
		args []any
	}{
		{name: "rewrite a posting's amount", stmt: `update ledger.posting set amount_minor = 1 where transfer_ref = $1`, args: []any{s.transferRef}},
		{name: "delete a posting", stmt: `delete from ledger.posting where transfer_ref = $1`, args: []any{s.transferRef}},
		{name: "rewrite a transfer's annotations", stmt: `update ledger.transfer set reference = 'a friendlier story' where ref = $1`, args: []any{s.transferRef}},
		{name: "backdate a transfer", stmt: `update ledger.transfer set posted_at = now() - interval '1 year' where ref = $1`, args: []any{s.transferRef}},
		{name: "delete a transfer", stmt: `delete from ledger.transfer where ref = $1`, args: []any{s.transferRef}},
		{name: "re-denominate an account", stmt: `update ledger.account set currency = 'GBP' where id = $1`, args: []any{s.give}},
		{name: "delete an account", stmt: `delete from ledger.account where id = $1`, args: []any{s.give}},
		{name: "truncate the postings", stmt: `truncate ledger.posting cascade`},
		{name: "truncate the transfers", stmt: `truncate ledger.transfer cascade`},
		{name: "truncate the accounts", stmt: `truncate ledger.account cascade`},
	}
	for _, tt := range frozen {
		nested, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin savepoint: %v", tt.name, err)
		}
		_, execErr := nested.Exec(ctx, tt.stmt, tt.args...)
		if err := nested.Rollback(ctx); err != nil {
			t.Fatalf("%s: rollback savepoint: %v", tt.name, err)
		}
		if execErr == nil {
			t.Fatalf("%s: want rejection, got success", tt.name)
		}
		wantPgCode(t, execErr, codeRaiseException)
	}
}

// TestDatabaseRejectsMalformedMoney is C-6 as writes: every spelling of
// money the constitution forbids must fail at the database layer with a
// specific SQLSTATE, whoever the writer is.
func TestDatabaseRejectsMalformedMoney(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		write    func(ctx context.Context, tx pgx.Tx, s seededTransfer) error
		wantCode string
	}{
		{
			name: "a posting that moves nothing",
			write: func(ctx context.Context, tx pgx.Tx, s seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
					 values ($1, $2, 0, 'EUR')`, s.transferRef, s.give)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "an account in a lowercase currency",
			write: func(ctx context.Context, tx pgx.Tx, _ seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.account (id, currency, kind) values ('sql/bad-currency', 'eur', 'house')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "a posting in a currency its account does not hold",
			write: func(ctx context.Context, tx pgx.Tx, s seededTransfer) error {
				// The composite foreign key, not a check the adapter
				// happens to run: money of another kind has no
				// representation on this account at all.
				_, err := tx.Exec(ctx,
					`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
					 values ($1, $2, 100, 'GBP')`, s.transferRef, s.give)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "a posting on an account nobody issued",
			write: func(ctx context.Context, tx pgx.Tx, s seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
					 values ($1, 'sql/never-issued', 100, 'EUR')`, s.transferRef)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "a transfer with a blank idempotency key",
			write: func(ctx context.Context, tx pgx.Tx, _ seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.transfer (idempotency_key) values ('   ')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "metadata that is not an object",
			write: func(ctx context.Context, tx pgx.Tx, _ seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.transfer (idempotency_key, metadata) values ($1, '["a"]'::jsonb)`,
					"sql-meta-array")
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "an account with a blank id",
			write: func(ctx context.Context, tx pgx.Tx, _ seededTransfer) error {
				_, err := tx.Exec(ctx,
					`insert into ledger.account (id, currency, kind) values ('  ', 'EUR', 'house')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			s := seedSQL(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, s), tt.wantCode)
		})
	}
}

// TestDatabaseKeepsMemberAccountsSolvent is D9 with the port bypassed: a
// transfer written straight in SQL that leaves a member's stage account
// holding less than nothing must be refused by the deferred trigger at
// COMMIT, and a house account in the same position must not be. Every case
// stages the money the way it really arrives - a house account credits the
// member - so the exemption is load-bearing in the passing cases too: the
// house ends each of them at -fund and commits anyway.
func TestDatabaseKeepsMemberAccountsSolvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fund is credited to the member from the house account, and draw
		// is then taken back out of the member to the house - both inside
		// the one transaction whose COMMIT is under test.
		fund, draw int64
		// wantRefused is true when that COMMIT must raise.
		wantRefused bool
	}{
		{name: "a member drawn inside its balance commits", fund: 1000, draw: 400},
		{name: "a member drawn to exactly nothing commits", fund: 1000, draw: 1000},
		{name: "a member drawn one minor unit past its balance is refused", fund: 1000, draw: 1001, wantRefused: true},
		{name: "a member drawn with nothing like enough to give is refused", fund: 1000, draw: 5000, wantRefused: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := requirePool(t)
			ctx := context.Background()

			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			suffix := randomSuffix(t)
			member, house := "sql/member/"+suffix, "sql/house/"+suffix
			if _, err := tx.Exec(ctx,
				`insert into ledger.account (id, currency, kind)
				 values ($1, 'EUR', 'member'), ($2, 'EUR', 'house')`,
				member, house); err != nil {
				t.Fatalf("seed accounts: %v", err)
			}
			stage := func(key string, onMember int64) {
				t.Helper()
				var ref string
				if err := tx.QueryRow(ctx,
					`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
					key+"-"+suffix).Scan(&ref); err != nil {
					t.Fatalf("staging transfer %s: %v", key, err)
				}
				if _, err := tx.Exec(ctx,
					`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
					 values ($1, $2, $3, 'EUR'), ($1, $4, $5, 'EUR')`,
					ref, member, onMember, house, -onMember); err != nil {
					t.Fatalf("staging the postings of %s: %v", key, err)
				}
			}
			stage("sql-fund", tc.fund)
			stage("sql-draw", -tc.draw)

			err = tx.Commit(ctx)
			if tc.wantRefused {
				wantPgCode(t, err, codeRaiseException)
				return
			}
			if err != nil {
				t.Fatalf("commit = %v, want nil: the member ends at %d, which is not less than nothing", err, tc.fund-tc.draw)
			}
		})
	}

	t.Run("a house account may end a transfer holding less than nothing", func(t *testing.T) {
		t.Parallel()
		pool := requirePool(t)
		ctx := context.Background()

		// No member anywhere in this transfer: one house account gives
		// another money it never had, which is exactly how the first
		// credit into a closed set of accounts has to work.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		suffix := randomSuffix(t)
		give, take := "sql/house-give/"+suffix, "sql/house-take/"+suffix
		if _, err := tx.Exec(ctx,
			`insert into ledger.account (id, currency, kind)
			 values ($1, 'EUR', 'house'), ($2, 'EUR', 'house')`,
			give, take); err != nil {
			t.Fatalf("seed accounts: %v", err)
		}
		var ref string
		if err := tx.QueryRow(ctx,
			`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
			"sql-house-"+suffix).Scan(&ref); err != nil {
			t.Fatalf("staging the transfer: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
			 values ($1, $2, -1000, 'EUR'), ($1, $3, 1000, 'EUR')`,
			ref, give, take); err != nil {
			t.Fatalf("staging the postings: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit = %v, want nil: a house account is where money enters, so it may go negative", err)
		}
	})

	t.Run("an account of a kind nobody defined cannot be written", func(t *testing.T) {
		t.Parallel()

		// The rule only means anything while the column holds one of the
		// two kinds: a third would be an account the solvency check has no
		// opinion about, which is a silent exemption.
		tx := beginTx(t)
		_, err := tx.Exec(context.Background(),
			`insert into ledger.account (id, currency, kind) values ($1, 'EUR', 'staff')`,
			"sql/odd-kind/"+randomSuffix(t))
		wantPgCode(t, err, codeCheckViolation)
	})
}

// TestNoFractionalMoneyTypeExistsInTheLedgerSchema asserts C-6 as the
// invariant map states it: not "this column is a bigint" but "no numeric,
// real, double precision or money column EXISTS here". Asked of the
// catalog, so it keeps answering for every relation a later migration
// adds to the schema.
func TestNoFractionalMoneyTypeExistsInTheLedgerSchema(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)

	rows, err := tx.Query(context.Background(),
		`select c.relname, a.attname, t.typname
		   from pg_attribute a
		   join pg_class c on c.oid = a.attrelid
		   join pg_namespace n on n.oid = c.relnamespace
		   join pg_type t on t.oid = a.atttypid
		  where n.nspname = 'ledger'
		    and c.relkind in ('r', 'p', 'v', 'm')
		    and a.attnum > 0
		    and not a.attisdropped
		    and t.typname in ('numeric', 'float4', 'float8', 'money')
		  order by c.relname, a.attname`)
	if err != nil {
		t.Fatalf("reading pg_attribute: %v", err)
	}
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, column, typeName string
		if err := rows.Scan(&table, &column, &typeName); err != nil {
			t.Fatalf("scanning pg_attribute: %v", err)
		}
		offenders = append(offenders, "ledger."+table+"."+column+" is "+typeName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating pg_attribute: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("C-6 violated: fractional types in the ledger schema: %v", offenders)
	}
}

// TestZeroSumCheckCanBePointedAtThisLedger proves the exit route keeps the
// continuous C-1 check (0016) without a rewrite: pointing
// cashback.ledger_schema at 'ledger' (0020) makes cashback.ledger_zero_sum
// read this schema's balances view. The committed world must always net to
// zero - the deferred trigger guarantees no unbalanced transfer ever
// commits - so the only imbalance the check can ever see is one this test
// stages, uncommitted, in its own transaction; and it must SEE that one,
// or the check is decorative.
func TestZeroSumCheckCanBePointedAtThisLedger(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	// Transaction-local, so the redirect cannot follow the connection
	// back into the pool.
	if _, err := tx.Exec(ctx,
		`select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the C-1 check at the ledger schema: %v", err)
	}

	violations := func() map[string]int64 {
		t.Helper()
		rows, err := tx.Query(ctx,
			`select currency, net_minor from cashback.ledger_zero_sum where net_minor <> 0`)
		if err != nil {
			t.Fatalf("the C-1 zero-sum check failed to run: %v", err)
		}
		defer rows.Close()
		broken := map[string]int64{}
		for rows.Next() {
			var currency string
			var net int64
			if err := rows.Scan(&currency, &net); err != nil {
				t.Fatalf("scanning the zero-sum check: %v", err)
			}
			broken[currency] = net
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating the zero-sum check: %v", err)
		}
		return broken
	}

	if broken := violations(); len(broken) != 0 {
		t.Fatalf("the committed ledger is out of balance: %v; the zero-sum trigger should have made this state impossible", broken)
	}

	// Stage an imbalance the deferred trigger has not yet judged: within
	// this transaction the check must report it, currency and amount.
	suffix := randomSuffix(t)
	account := "sql/standin/" + suffix
	if _, err := tx.Exec(ctx,
		`insert into ledger.account (id, currency, kind) values ($1, 'GBP', 'house')`, account); err != nil {
		t.Fatalf("staging the account: %v", err)
	}
	var ref string
	if err := tx.QueryRow(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
		"sql-zerosum-"+suffix).Scan(&ref); err != nil {
		t.Fatalf("staging the transfer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
		 values ($1, $2, 50, 'GBP')`, ref, account); err != nil {
		t.Fatalf("staging the unbalanced posting: %v", err)
	}

	broken := violations()
	if len(broken) != 1 || broken["GBP"] != 50 {
		t.Fatalf("zero-sum check reported %v, want exactly the staged imbalance (GBP: 50)", broken)
	}
}
