package store_test

// Why the lock the orphan test takes is retried inside a savepoint.
//
// The retry loop in orphan_test.go shipped once without one, and it could
// never have worked: a statement that fails aborts its transaction, so the
// second attempt did not attempt anything - it answered 25P02, which the loop
// did not recognise, and the case died reporting "current transaction is
// aborted" instead of "the table was busy". The suite looked flaky and the
// message pointed nowhere near the cause.
//
// This file asserts the two facts the fix rests on, against a real database
// and with no timing race: the holder holds until this test lets go.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jackc/pgerrcode"
)

// holdEvidenceLock takes ACCESS EXCLUSIVE on cashback.entry from a connection
// of its own and holds it until the returned function is called.
//
// A second pool, because a lock cannot contend with itself: two transactions
// on one connection are the same transaction as far as the lock manager is
// concerned.
func holdEvidenceLock(ctx context.Context, t *testing.T) func() {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting the holder: %v", err)
	}
	held, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("the holder's transaction: %v", err)
	}
	if _, err := held.Exec(ctx, `lock table cashback.entry in access exclusive mode`); err != nil {
		_ = held.Rollback(ctx)
		pool.Close()
		t.Fatalf("the holder taking the lock: %v", err)
	}
	return func() {
		_ = held.Rollback(ctx)
		pool.Close()
	}
}

// lockTimedOut reports whether err is Postgres refusing to wait any longer.
func lockTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable
}

// transactionAborted reports the 25P02 that every command gets after a failed
// one, until the transaction ends.
func transactionAborted(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.InFailedSQLTransaction
}

// TestAFailedLockAbortsTheTransaction is the bug, stated as a fact about
// Postgres rather than about this suite. Without a savepoint there is nothing
// to retry INTO.
// Deliberately NOT parallel, here and below. Both cases take the same table
// lock, and Go defers parallel tests until the sequential ones are done - so
// running sequentially is what keeps these two from being each other's
// contention, and from racing the parallel suites in this package.
func TestAFailedLockAbortsTheTransaction(t *testing.T) {
	ctx, tx, done := schemaTx(t)
	defer done()

	release := holdEvidenceLock(ctx, t)
	defer release()

	attempt, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() { _ = attempt.Rollback(ctx) }()

	if _, err := attempt.Exec(ctx, `set local lock_timeout = '250ms'`); err != nil {
		t.Fatalf("bounding the wait: %v", err)
	}
	_, err = attempt.Exec(ctx, `lock table cashback.entry in access exclusive mode`)
	if !lockTimedOut(err) {
		t.Fatalf("the lock did not time out: %v", err)
	}

	// The transaction is now unusable, and THAT is what a naive retry ran
	// into: not a second lock failure, but a refusal to run anything at all.
	_, err = attempt.Exec(ctx, `select 1`)
	if !transactionAborted(err) {
		t.Fatalf("after a failed lock, `select 1` answered %v; want 25P02 — "+
			"if Postgres no longer aborts here, the savepoint in "+
			"lockEvidenceExclusively is no longer load-bearing and its comment is wrong", err)
	}
}

// TestASavepointMakesTheRetryPossible is the fix, and the reason the helper
// opens a nested transaction per attempt.
func TestASavepointMakesTheRetryPossible(t *testing.T) {
	ctx, tx, done := schemaTx(t)
	defer done()

	release := holdEvidenceLock(ctx, t)

	if _, err := tx.Exec(ctx, `set local lock_timeout = '250ms'`); err != nil {
		t.Fatalf("bounding the wait: %v", err)
	}

	// One doomed attempt, rolled back to the savepoint.
	failed, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, err := failed.Exec(ctx, `lock table cashback.entry in access exclusive mode`); !lockTimedOut(err) {
		release()
		t.Fatalf("the lock did not time out: %v", err)
	}
	if err := failed.Rollback(ctx); err != nil {
		release()
		t.Fatalf("rolling back the doomed attempt: %v", err)
	}

	// The outer transaction is usable again - which is the whole claim.
	if _, err := tx.Exec(ctx, `select 1`); err != nil {
		release()
		t.Fatalf("after rolling back to the savepoint the transaction is still unusable: %v", err)
	}

	// And a second attempt, once the holder lets go, actually takes the lock
	// and keeps it past the release of its savepoint.
	release()
	// The helper's own bound, not the quarter-second the doomed attempt used:
	// this attempt is meant to SUCCEED, and holding it to a timeout chosen to
	// make failure quick would make the case fail on a busy machine.
	if _, err := tx.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
		t.Fatalf("restoring the wait: %v", err)
	}
	taken, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("the second savepoint: %v", err)
	}
	if _, err := taken.Exec(ctx, `lock table cashback.entry in access exclusive mode`); err != nil {
		t.Fatalf("the retry could not take the lock: %v", err)
	}
	if err := taken.Commit(ctx); err != nil {
		t.Fatalf("releasing the savepoint that took the lock: %v", err)
	}
	if !holdsEvidenceLock(ctx, t, tx) {
		t.Error("releasing the savepoint dropped the lock it took; the DDL after it would run unprotected")
	}
}

// holdsEvidenceLock asks the database whether this transaction holds the lock,
// rather than assuming a released savepoint keeps it.
func holdsEvidenceLock(ctx context.Context, t *testing.T, tx pgx.Tx) bool {
	t.Helper()
	var held bool
	if err := tx.QueryRow(ctx, `
		select exists (
			select 1
			  from pg_locks l
			  join pg_class c on c.oid = l.relation
			  join pg_namespace n on n.oid = c.relnamespace
			 where l.pid = pg_backend_pid()
			   and n.nspname = 'cashback' and c.relname = 'entry'
			   and l.mode = 'AccessExclusiveLock' and l.granted)`).Scan(&held); err != nil {
		t.Fatalf("reading pg_locks: %v", err)
	}
	return held
}

// TestTheRetryOutlastsAContendedLock is the guard on the helper itself, as
// opposed to on the mechanism above.
//
// The lock is held past the first attempt's bound, so the helper MUST fail
// once and come back. Without the savepoint the second attempt answers 25P02,
// which the helper does not recognise, and it fails the case - which is
// exactly how this shipped and exactly what this catches.
func TestTheRetryOutlastsAContendedLock(t *testing.T) {
	ctx, tx, done := schemaTx(t)
	defer done()

	// Short enough that the first attempt gives up while the holder still
	// holds, so the retry is genuinely exercised.
	restore := evidenceLockTimeout
	evidenceLockTimeout = "200ms"
	defer func() { evidenceLockTimeout = restore }()

	release := holdEvidenceLock(ctx, t)
	releaseOnce := make(chan struct{})
	go func() {
		time.Sleep(700 * time.Millisecond)
		release()
		close(releaseOnce)
	}()
	defer func() { <-releaseOnce }()

	// Fails, retries, and takes the locks once the holder lets go. A failure
	// here is the helper, not the database.
	lockEvidenceExclusively(ctx, t, tx)

	if !holdsEvidenceLock(ctx, t, tx) {
		t.Error("the helper returned without holding the lock it exists to take")
	}
}
