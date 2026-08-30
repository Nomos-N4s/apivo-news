package store_test

// Exercises the cursor queries against the real, migrated schema (T055).
//
// Everything here is about a rule that only exists in the database. The
// conditional advance is what makes "read a window, write it, move the
// cursor" safe against a second poller; the schema's own check is what keeps
// the trailing cursor behind the main one; and FOR UPDATE is what stops two
// pollers paying for the same fetch before either finds out. None of the
// three can be read off the Go code that calls them.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// codeLockNotAvailable is what a statement reports when lock_timeout expires
// waiting for a row somebody else holds.
const codeLockNotAvailable = "55P03"

func stamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}

func TestNetworkAccountCursorsAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the cursor queries")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	anchor := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)

	each(ctx, t, tx, "a fresh account has got nowhere yet", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		_, accountID := account(ctx, t, tx)

		got, err := queries.GetNetworkAccountCursors(ctx, accountID)
		if err != nil {
			t.Fatalf("GetNetworkAccountCursors(): %v", err)
		}
		if got.CursorAt.Valid || got.TrailingCursorAt.Valid {
			t.Errorf("a never-polled account reports cursors %v / %v, want both unset", got.CursorAt, got.TrailingCursorAt)
		}
		if !got.Active {
			t.Error("the seeded account is inactive, so the rest of this suite would be judging an account nobody polls")
		}
	})

	each(ctx, t, tx, "the first advance moves the cursor from nothing", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		_, accountID := account(ctx, t, tx)

		// The case `=` gets wrong. null = null is null, not true, so an
		// equality comparison would match no row and every first poll would
		// report a race it had not lost.
		moved, err := queries.AdvanceNetworkAccountCursor(ctx, store.AdvanceNetworkAccountCursorParams{
			ID:          accountID,
			AdvanceTo:   stamp(anchor),
			AdvanceFrom: pgtype.Timestamptz{},
		})
		if err != nil {
			t.Fatalf("the first advance was refused: %v", err)
		}
		if !moved.CursorAt.Time.Equal(anchor) {
			t.Errorf("the cursor is at %s, want %s", moved.CursorAt.Time, anchor)
		}
	})

	each(ctx, t, tx, "an advance from a stale value moves nothing", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		_, accountID := account(ctx, t, tx)

		if _, err := queries.AdvanceNetworkAccountCursor(ctx, store.AdvanceNetworkAccountCursorParams{
			ID: accountID, AdvanceTo: stamp(anchor), AdvanceFrom: pgtype.Timestamptz{},
		}); err != nil {
			t.Fatalf("the first advance: %v", err)
		}

		// A second poller that read the cursor before the first one moved it.
		// Its window has already been read and written by somebody else, and
		// letting this through would move the cursor past a window only the
		// other poller persisted.
		_, err := queries.AdvanceNetworkAccountCursor(ctx, store.AdvanceNetworkAccountCursorParams{
			ID:          accountID,
			AdvanceTo:   stamp(anchor.Add(48 * time.Hour)),
			AdvanceFrom: pgtype.Timestamptz{},
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("an advance from a stale cursor returned %v, want pgx.ErrNoRows", err)
		}

		still, err := queries.GetNetworkAccountCursors(ctx, accountID)
		if err != nil {
			t.Fatalf("GetNetworkAccountCursors(): %v", err)
		}
		if !still.CursorAt.Time.Equal(anchor) {
			t.Errorf("the cursor moved to %s despite the refusal, want %s", still.CursorAt.Time, anchor)
		}
	})

	each(ctx, t, tx, "the trailing cursor may not run ahead of the main one", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		_, accountID := account(ctx, t, tx)

		if _, err := queries.AdvanceNetworkAccountCursor(ctx, store.AdvanceNetworkAccountCursorParams{
			ID: accountID, AdvanceTo: stamp(anchor), AdvanceFrom: pgtype.Timestamptz{},
		}); err != nil {
			t.Fatalf("the first advance: %v", err)
		}

		// Behind is fine: the trailing cursor re-reads what has already been
		// read.
		if _, err := queries.AdvanceNetworkAccountTrailingCursor(ctx, store.AdvanceNetworkAccountTrailingCursorParams{
			ID: accountID, AdvanceTo: stamp(anchor.Add(-72 * time.Hour)), AdvanceFrom: pgtype.Timestamptz{},
		}); err != nil {
			t.Fatalf("a trailing cursor behind the main one was refused: %v", err)
		}

		// Ahead is a restart that advanced the wrong cursor, which is how a
		// window gets skipped. The schema refuses it; nothing in Go does.
		_, err := queries.AdvanceNetworkAccountTrailingCursor(ctx, store.AdvanceNetworkAccountTrailingCursorParams{
			ID:          accountID,
			AdvanceTo:   stamp(anchor.Add(time.Hour)),
			AdvanceFrom: stamp(anchor.Add(-72 * time.Hour)),
		})
		code, constraint := refusal(err)
		if code != codeCheckViolation {
			t.Fatalf("a trailing cursor ahead of the main one was accepted (SQLSTATE %q, err %v)", code, err)
		}
		if constraint != "network_account_trailing_cursor_not_ahead" {
			t.Errorf("it was refused by %q, want network_account_trailing_cursor_not_ahead", constraint)
		}
	})

	each(ctx, t, tx, "a trailing cursor with no main cursor is a re-read of history never read", func(t *testing.T, tx pgx.Tx, queries *store.Queries) {
		_, accountID := account(ctx, t, tx)

		_, err := queries.AdvanceNetworkAccountTrailingCursor(ctx, store.AdvanceNetworkAccountTrailingCursorParams{
			ID: accountID, AdvanceTo: stamp(anchor), AdvanceFrom: pgtype.Timestamptz{},
		})
		code, constraint := refusal(err)
		if code != codeCheckViolation {
			t.Fatalf("a trailing cursor was set on an account that has never been polled (SQLSTATE %q, err %v)", code, err)
		}
		if constraint != "network_account_trailing_cursor_not_ahead" {
			t.Errorf("it was refused by %q, want network_account_trailing_cursor_not_ahead", constraint)
		}
	})
}

// TestGetNetworkAccountCursorsLocksTheRow is the property that stops two
// pollers paying for the same fetch. It needs two real connections, because
// a savepoint inside one transaction cannot contend with itself - which is
// exactly why it is a test of its own rather than a case above.
func TestGetNetworkAccountCursorsLocksTheRow(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the cursor lock")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	// Registered as a cleanup rather than deferred, and FIRST, because
	// cleanups run last-in-first-out: the row deletion below needs the pool
	// still open, and a deferred Close would have run before it.
	t.Cleanup(pool.Close)

	// The account has to be COMMITTED for a second connection to see it at
	// all, so this test cleans up after itself rather than rolling back.
	seed, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	networkID, accountID := account(ctx, t, seed)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("committing the seed: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `delete from cashback.network_account where network_id = $1`, networkID); err != nil {
			t.Errorf("cleaning up the account: %v", err)
		}
		if _, err := pool.Exec(ctx, `delete from cashback.network where id = $1`, networkID); err != nil {
			t.Errorf("cleaning up the network: %v", err)
		}
	})

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := store.New(holder).GetNetworkAccountCursors(ctx, accountID); err != nil {
		t.Fatalf("the first poller's read: %v", err)
	}

	contender, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin contender: %v", err)
	}
	defer func() { _ = contender.Rollback(ctx) }()
	// Without a timeout this would block until the holder finishes, which in
	// a test is until it is killed. With one, the wait becomes an assertion.
	if _, err := contender.Exec(ctx, `set local lock_timeout = '750ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}

	_, err = store.New(contender).GetNetworkAccountCursors(ctx, accountID)
	code, _ := refusal(err)
	if code != codeLockNotAvailable {
		t.Fatalf("a second poller read the account while the first held it (SQLSTATE %q, err %v); both would then fetch the same window and only one could keep it",
			code, err)
	}
}
