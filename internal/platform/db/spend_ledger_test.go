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
		`insert into translation_spend (month, spent_microusd) values (date_trunc('month', now() at time zone 'utc')::date, 0)
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
		 where month = date_trunc('month', now() at time zone 'utc')::date`).Scan(&spentAfter, &attemptsAfter); err != nil {
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

// TestTwoConcurrentTranslationInsertsBothReachTheLedger asserts the one
// thing the database contributes that application code cannot - an atomic
// month counter - and asserts it through the ONLY path production uses:
// the 0005 trigger. A version of this test that ran its own copy of the
// upsert SQL would keep passing if translation_record_spend() were
// rewritten as SELECT-then-UPDATE, the exact lost-update shape the
// migration exists to prevent. So the two sessions here INSERT real
// translation rows concurrently, and the assertions are that the second
// session serialises on the month row the trigger locked, and that the
// committed month carries both costs.
//
// The race needs two real sessions and real commits; row locks are
// invisible inside one rolled-back transaction. That has a cost this test
// accepts and the rest of the suite avoids: translation rows are immutable
// and undeletable (I-3 tooling), so the two rows and their few thousand
// micro-USD stay in the test database's current month. The suite's other
// ledger tests are unaffected - they measure deltas under the month row's
// lock, never absolute totals.
func TestTwoConcurrentTranslationInsertsBothReachTheLedger(t *testing.T) {
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
	t.Cleanup(func() { _ = connA.Close(ctx) })
	connB, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session B: %v", err)
	}
	t.Cleanup(func() { _ = connB.Close(ctx) })

	// The seeds are committed (autocommit) so session B can see them.
	suffix := randomSuffix(t)
	var sourceID string
	if err := connA.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted per feed terms') returning id`,
		"Concurrent Spend Feed "+suffix, "https://example.test/feed/concurrent-"+suffix).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	seedItem := func(name string) string {
		var itemID string
		if err := connA.QueryRow(ctx,
			`insert into source_item (source_id, source_url, raw_body) values ($1, $2, $3) returning id`,
			sourceID, "https://example.test/articles/concurrent-"+name+"-"+suffix,
			"σώμα "+name+" "+suffix).Scan(&itemID); err != nil {
			t.Fatalf("seed source_item %s: %v", name, err)
		}
		return itemID
	}
	itemA, itemB := seedItem("a"), seedItem("b")

	const costA, costB = 3_117, 4_529
	const insert = `insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
	                values ($1, 'de', 'test-model-1', 'prompt-v1', $2, $3, $4)`
	const monthTotal = `select spent_microusd from translation_spend
	                     where month = date_trunc('month', now() at time zone 'utc')::date`

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin session A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	if _, err := txA.Exec(ctx, insert, itemA, "Überschrift A "+suffix, "Auszug A "+suffix, costA); err != nil {
		t.Fatalf("session A insert: %v", err)
	}

	// Read the month under the lock the trigger's upsert just took: the
	// total is the committed baseline plus this transaction's own cost,
	// and nothing can move it until commit releases the lock.
	var totalAfterA int64
	if err := txA.QueryRow(ctx, monthTotal).Scan(&totalAfterA); err != nil {
		t.Fatalf("reading the month under session A's lock: %v", err)
	}

	done := make(chan struct{})
	fail := make(chan error, 1)
	go func() {
		if _, err := connB.Exec(ctx, insert, itemB, "Überschrift B "+suffix, "Auszug B "+suffix, costB); err != nil {
			fail <- err
			return
		}
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("session B's insert committed while session A held the month row: the trigger is not serialising on the counter")
	case err := <-fail:
		t.Fatalf("session B insert: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Blocked on the month row the trigger locked, as required.
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit session A: %v", err)
	}

	select {
	case <-done:
	case err := <-fail:
		t.Fatalf("session B insert after A committed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("session B still blocked after session A committed")
	}

	var final int64
	if err := connA.QueryRow(ctx, monthTotal).Scan(&final); err != nil {
		t.Fatalf("reading the month back: %v", err)
	}
	if final != totalAfterA+costB {
		t.Errorf("month total = %d, want %d: a lost update - the trigger dropped one session's spend", final, totalAfterA+costB)
	}
}
