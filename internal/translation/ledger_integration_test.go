package translation_test

// Integration tests for the monthly spend ledger, against the real,
// migrated schema (FR-006). What is under test is the pair of things the
// 0005 trigger cannot do for itself: spend that produced no translation
// row, and the once-per-month halt latch.
//
// Every test runs inside a transaction that is rolled back. That matters
// more here than elsewhere: the ledger is keyed by calendar month, so the
// current month's row is shared by everything running against this
// database, and a committed test would move a number the rest of the suite
// reads. The upsert takes that row's lock for the life of the transaction,
// which is also what makes the deltas below exact.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// ledgerPool connects to the migrated test database, skipping when none is
// configured. DB-backed tests are never skipped in CI.
func ledgerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the spend ledger")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ledgerTx opens a transaction that is always rolled back.
func ledgerTx(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := ledgerPool(t).Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// TestTheLedgerAcceptsSpendWithNoTranslationRow asserts the path a trigger
// on translation cannot reach: a call the provider billed for that produced
// nothing to hang the cost on - refused at the ceiling, cut off
// mid-generation, failed after the tokens were generated. translation_spend
// has no foreign key precisely so this money has somewhere to go.
func TestTheLedgerAcceptsSpendWithNoTranslationRow(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	ledger := translation.NewLedger(tx)

	// An empty spend creates-or-locks this month's row and returns its
	// current total, so the delta measured below is this transaction's
	// alone even though the suite shares the month.
	before, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{})
	if err != nil {
		t.Fatalf("reading the month under lock: %v", err)
	}

	after, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{CostMicroUSD: 7_777, UnmeteredAttempts: 3})
	if err != nil {
		t.Fatalf("recording spend with no translation row: %v", err)
	}
	if got := after - before; got != 7_777 {
		t.Errorf("month total moved by %d micro-USD, want 7777: a billed call with no translation row still costs money", got)
	}

	month, err := ledger.ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if month.SpentMicroUSD != after {
		t.Errorf("ThisMonth reports %d micro-USD, want the %d the upsert returned", month.SpentMicroUSD, after)
	}
	if month.UnmeteredAttempts < 3 {
		t.Errorf("ThisMonth reports %d unpriced attempts, want at least the 3 just recorded: without them the total reads more certain than it is", month.UnmeteredAttempts)
	}
	if month.Halted() {
		t.Error("a month that has not reached the cap reports itself halted")
	}
	if month.Month.IsZero() {
		t.Error("ThisMonth did not report which month it read")
	}
}

// TestTheLedgerRejectsImpossibleSpend asserts the arithmetic that would
// quietly shrink the record is refused before it reaches the database: a
// negative cost is not an amount anyone was charged, and it would subtract
// from a total the cap is checked against.
func TestTheLedgerRejectsImpossibleSpend(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	ledger := translation.NewLedger(tx)

	if _, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{CostMicroUSD: -1}); err == nil {
		t.Error("a negative cost was accepted into the ledger")
	}
	if _, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{UnmeteredAttempts: -1}); err == nil {
		t.Error("a negative count of unpriced attempts was accepted into the ledger")
	}
}

// TestTheHaltLatchesOncePerMonth asserts the rule that keeps the halt an
// event rather than a repeated log line: the database decides who crossed
// the cap, and the pipeline.halted event is written once off that single
// winning update. Evaluated in Go and written unconditionally, a halt would
// append one event per tick to a stream that cannot deduplicate them.
//
// A far-future month keys the test's own row, so nothing here touches the
// month the rest of the suite is spending in.
func TestTheHaltLatchesOncePerMonth(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	ledger := translation.NewLedger(tx)

	const monthlyCap = 25_000_000
	month := time.Date(2099, time.May, 1, 0, 0, 0, 0, time.UTC)
	if _, err := tx.Exec(ctx,
		`insert into translation_spend (month, spent_microusd, unmetered_attempts)
		 values ($1::date, $2, 4)`, "2099-05-01", monthlyCap+1_234); err != nil {
		t.Fatalf("seeding a month at the cap: %v", err)
	}

	haltedAt, latched, err := ledger.Halt(ctx, month, monthlyCap)
	if err != nil {
		t.Fatalf("halting the month: %v", err)
	}
	if !latched {
		t.Fatal("a month past the cap did not halt")
	}
	if haltedAt.IsZero() {
		t.Error("the halt reported no time")
	}

	// Second crossing, same month: the latch is closed.
	_, latchedAgain, err := ledger.Halt(ctx, month, monthlyCap)
	if err != nil {
		t.Fatalf("second halt attempt: %v", err)
	}
	if latchedAgain {
		t.Error("the month halted twice: the halt would be announced once per tick, not once per month")
	}

	// The audit record is written by the database off the winning update,
	// in the same transaction - never by application discipline.
	var events int
	var spent int64
	if err := tx.QueryRow(ctx,
		`select count(*), max((payload->>'spent_microusd')::bigint)
		   from domain_event
		  where type = 'pipeline.halted' and payload->>'month' = '2099-05-01'`).
		Scan(&events, &spent); err != nil {
		t.Fatalf("querying pipeline.halted events: %v", err)
	}
	if events != 1 {
		t.Fatalf("pipeline.halted events for the month = %d, want exactly 1", events)
	}
	if spent != monthlyCap+1_234 {
		t.Errorf("pipeline.halted recorded %d micro-USD, want the %d the month had reached", spent, monthlyCap+1_234)
	}
}

// TestAMonthBelowTheCapDoesNotHalt is the negative control: the latch is
// conditional on the money, not on being asked.
func TestAMonthBelowTheCapDoesNotHalt(t *testing.T) {
	t.Parallel()
	tx := ledgerTx(t)
	ctx := context.Background()
	ledger := translation.NewLedger(tx)

	const monthlyCap = 25_000_000
	month := time.Date(2099, time.July, 1, 0, 0, 0, 0, time.UTC)
	if _, err := tx.Exec(ctx,
		`insert into translation_spend (month, spent_microusd) values ($1::date, $2)`,
		"2099-07-01", monthlyCap-1); err != nil {
		t.Fatalf("seeding a month under the cap: %v", err)
	}

	haltedAt, latched, err := ledger.Halt(ctx, month, monthlyCap)
	if err != nil {
		t.Fatalf("halt attempt: %v", err)
	}
	if latched || !haltedAt.IsZero() {
		t.Errorf("a month one micro-USD short of the cap halted (at %v)", haltedAt)
	}

	var events int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event
		  where type = 'pipeline.halted' and payload->>'month' = '2099-07-01'`).Scan(&events); err != nil {
		t.Fatalf("querying pipeline.halted events: %v", err)
	}
	if events != 0 {
		t.Errorf("pipeline.halted events = %d, want 0: nothing halted", events)
	}
}
