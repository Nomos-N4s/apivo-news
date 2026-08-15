package db_test

// These tests assert that the DATABASE, not the translation module, is what
// keeps the spend record true (FR-006, 0005):
//
//	every translation moves the ledger, by trigger
//	one paid translation per retrieved item and target locale
//	an unpriced-attempt count is never negative on either side
//	the month counter is atomic across real concurrent sessions
//
// The budget itself - the per-article ceiling and the monthly cap - is
// deliberately not here: it is configuration enforced in application code,
// because a CHECK on the cost would refuse to record money that was already
// spent. See the 0005 migration comments.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// currentMonthLedger creates-or-locks this month's ledger row and reads it
// back under that lock. Taking the lock BEFORE the write under test is what
// makes the delta exact: the suite runs against a shared database, and a
// plain read could be overtaken by another session's committed spend
// between the reading and the writing.
func currentMonthLedger(t *testing.T, tx pgx.Tx) (spent int64, attempts int) {
	t.Helper()
	err := tx.QueryRow(context.Background(),
		`insert into translation_spend (month, spent_microusd) values (date_trunc('month', now())::date, 0)
		 on conflict (month) do update set spent_microusd = translation_spend.spent_microusd
		 returning spent_microusd, unmetered_attempts`).Scan(&spent, &attempts)
	if err != nil {
		t.Fatalf("reading the current month ledger row: %v", err)
	}
	return spent, attempts
}

// execInSavepoint runs one statement expected to fail inside a nested
// transaction, so the raised error does not abort the enclosing test
// transaction (25P02) and later cases can still run.
func execInSavepoint(t *testing.T, tx pgx.Tx, stmt string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	nested, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("begin savepoint: %v", err)
	}
	_, execErr := nested.Exec(ctx, stmt, args...)
	if err := nested.Rollback(ctx); err != nil {
		t.Fatalf("rollback savepoint: %v", err)
	}
	return execErr
}

// TestEveryTranslationInsertMovesTheLedger asserts the rule that makes the
// monthly total trustworthy: the ledger is moved by the database in the
// same transaction as the insert, so "a translation whose cost is not in
// the ledger" is unrepresentable. Nothing here is application discipline -
// the test writes the translation row directly, with no Go writer involved.
func TestEveryTranslationInsertMovesTheLedger(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	spentBefore, attemptsBefore := currentMonthLedger(t, tx)

	// A second locale for the same item: legal, and separately paid for.
	if _, err := tx.Exec(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd, unmetered_attempts)
		 values ($1, 'el', 'test-model-1', 'prompt-v1', 'Επικεφαλίδα', 'Απόσπασμα', 4321, 2)`,
		f.sourceItemID); err != nil {
		t.Fatalf("insert translation: %v", err)
	}

	var spentAfter int64
	var attemptsAfter int
	if err := tx.QueryRow(ctx,
		`select spent_microusd, unmetered_attempts from translation_spend
		 where month = date_trunc('month', now())::date`).Scan(&spentAfter, &attemptsAfter); err != nil {
		t.Fatalf("reading the ledger after the insert: %v", err)
	}
	if got := spentAfter - spentBefore; got != 4321 {
		t.Errorf("ledger moved by %d micro-USD, want 4321 (the cost of the row just inserted)", got)
	}
	// The lower-bound flag travels with the cost: without it the ledger
	// would report a total it cannot stand behind and never say so.
	if got := attemptsAfter - attemptsBefore; got != 2 {
		t.Errorf("ledger unmetered_attempts moved by %d, want 2: the recorded cost is a lower bound and the ledger must carry that", got)
	}
}

// TestASecondTranslationOfTheSameItemAndLocaleIsRejected asserts the
// uniqueness that stops a re-run, a retry after a partial failure, or a
// second worker from buying the same translation twice - and from putting
// two approvable origins for one item and locale in the editorial queue.
func TestASecondTranslationOfTheSameItemAndLocaleIsRejected(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx) // seeds a 'de' translation of f.sourceItemID

	// Positive control: another locale for the same item is a different
	// translation and stays legal.
	if _, err := tx.Exec(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'el', 'test-model-1', 'prompt-v1', 'Επικεφαλίδα', 'Απόσπασμα', 100)`,
		f.sourceItemID); err != nil {
		t.Fatalf("a second locale for the same item must be allowed: %v", err)
	}

	// Positive control: the same locale for a different item is likewise
	// a different translation.
	var otherItemID string
	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, raw_body)
		 values ($1, $2, $3) returning id`,
		f.sourceID, "https://example.test/articles/other-"+randomSuffix(t), "άλλο σώμα "+f.rawBody).
		Scan(&otherItemID); err != nil {
		t.Fatalf("seed second source_item: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'de', 'test-model-1', 'prompt-v1', 'Zweite', 'Auszug', 100)`,
		otherItemID); err != nil {
		t.Fatalf("the same locale for a different item must be allowed: %v", err)
	}

	// The rule: the pair is unique, whatever the model, prompt or price.
	err := execInSavepoint(t,
		tx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'de', 'test-model-2', 'prompt-v2', 'Nochmal', 'Auszug', 9999)`,
		f.sourceItemID)
	wantPgCode(t, err, codeUniqueViolation)
}

// TestNegativeUnmeteredAttemptsAreRejected asserts both halves of the
// lower-bound counter are floored at zero. A negative count would let the
// ledger claim more confidence in its total than it has earned - the exact
// inverse of what the column exists to report.
func TestNegativeUnmeteredAttemptsAreRejected(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	f := seed(t, tx)

	tests := []struct {
		name string
		stmt string
		args []any
	}{
		{
			name: "on a translation",
			stmt: `insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd, unmetered_attempts)
			       values ($1, 'el', 'm', 'p', 'headline', 'extract', 0, -1)`,
			args: []any{f.sourceItemID},
		},
		{
			name: "on the monthly ledger",
			stmt: `insert into translation_spend (month, spent_microusd, unmetered_attempts)
			       values (date '2099-01-01', 0, -1)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := execInSavepoint(t, tx, tt.stmt, tt.args...)
			wantPgCode(t, err, codeCheckViolation)
		})
	}
}

// TestTwoConcurrentUpsertsBothLand asserts the one thing the database
// contributes that application code cannot: an atomic counter. The
// single-statement upsert takes the month row's lock and returns the
// POST-increment total, so two workers crossing the month together each
// see a different total and neither loses the other's money -
// SELECT-then-INSERT would have both read the same number and written back
// over each other.
//
// The race needs two real sessions and real commits; row locks are
// invisible inside one rolled-back transaction. A far-future month keys the
// test's own ledger row so it never collides with the suite's real spend.
func TestTwoConcurrentUpsertsBothLand(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise schema invariants")
	}
	ctx := context.Background()

	// Closed by cleanup rather than by defer: deferred calls run when the
	// test function returns, which is BEFORE its cleanups, and the cleanup
	// below still needs a connection to tidy the test month with. Cleanups
	// run last-registered-first, so it gets one.
	connA, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session A: %v", err)
	}
	t.Cleanup(func() { _ = connA.Close(ctx) })
	connB, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session B: %v", err)
	}
	t.Cleanup(func() { _ = connB.Close(ctx) })

	const month = "2099-06-01"
	const upsert = `insert into translation_spend (month, spent_microusd, unmetered_attempts)
	                values ($1::date, $2, 0)
	                on conflict (month) do update
	                   set spent_microusd = translation_spend.spent_microusd + excluded.spent_microusd,
	                       unmetered_attempts = translation_spend.unmetered_attempts + excluded.unmetered_attempts
	                returning spent_microusd`

	// This month is the test's alone, so it is safe to clear before and
	// after: unlike translation and article, the ledger is a counter, not
	// a record of a decision.
	clearMonth := func() {
		if _, err := connA.Exec(ctx, `delete from translation_spend where month = $1::date`, month); err != nil {
			t.Fatalf("clearing the test month: %v", err)
		}
	}
	clearMonth()
	t.Cleanup(clearMonth)

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin session A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	var totalA int64
	if err := txA.QueryRow(ctx, upsert, month, 100).Scan(&totalA); err != nil {
		t.Fatalf("session A upsert: %v", err)
	}

	done := make(chan int64, 1)
	fail := make(chan error, 1)
	go func() {
		var totalB int64
		if err := connB.QueryRow(ctx, upsert, month, 250).Scan(&totalB); err != nil {
			fail <- err
			return
		}
		done <- totalB
	}()

	select {
	case totalB := <-done:
		t.Fatalf("session B finished (total %d) while session A held the month row: the counter is not serialising", totalB)
	case err := <-fail:
		t.Fatalf("session B upsert: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Blocked on A's row lock, as required.
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit session A: %v", err)
	}

	var totalB int64
	select {
	case totalB = <-done:
	case err := <-fail:
		t.Fatalf("session B upsert after A committed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("session B still blocked after session A committed")
	}

	if totalA != 100 {
		t.Errorf("session A's post-increment total = %d, want 100", totalA)
	}
	if totalB != 350 {
		t.Errorf("session B's post-increment total = %d, want 350 (its own 250 on top of A's committed 100)", totalB)
	}
	if totalA == totalB {
		t.Error("both sessions saw the same total: the upsert is not returning the post-increment value")
	}

	var final int64
	if err := connA.QueryRow(ctx,
		`select spent_microusd from translation_spend where month = $1::date`, month).Scan(&final); err != nil {
		t.Fatalf("reading the test month back: %v", err)
	}
	if final != 350 {
		t.Errorf("month total = %d, want 350: one session's spend was lost", final)
	}
}
