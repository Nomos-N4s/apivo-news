package editorial_test

// The concurrency story behind publication's role check (T020).
//
// Publication is the one editorial write no database trigger can guard:
// nothing on the article row records who released it, so the schema cannot
// see the actor. The store therefore establishes the authority itself, and
// does it the way migration 0002 does - a locking read (FOR SHARE) of the
// actor's account row inside the publishing transaction. Without that lock
// both sides of a demotion race could pass their own check against the
// prior state and both commit, recording a reader as having released an
// article, irreversibly.
//
// The race needs two real sessions and real commits; row locks are
// invisible inside a single rolled-back transaction. The committed rows
// stay in the test database on purpose: article and source_item are
// immutable by design, and random suffixes keep runs independent.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// blockDetectionWindow is how long a statement must stay blocked before the
// test believes it really is waiting on a lock rather than merely slow.
const blockDetectionWindow = 300 * time.Millisecond

// raceWorld is a committed fixture for one race: an unpublished article, the
// editor about to publish it, and - deliberately - a different editor as its
// approver.
//
// The separation matters. account_role_guard refuses to demote an editor
// whose approvals or withdrawals are on record, so if the publisher were
// also the approver the demotion would raise for that reason and the test
// would prove nothing about publication. The publisher has no editorial
// decisions recorded against them, which is exactly the account the old
// code could be raced with.
type raceWorld struct {
	publisherID uuid.UUID
	articleID   uuid.UUID
}

// seedAttempts bounds how many times the seed is rebuilt after Postgres
// picks it as a deadlock victim. Small on purpose: one more go clears a
// lost coin toss, while a seed that deadlocks four times running is a
// finding rather than a flake and must be allowed to fail the test.
const seedAttempts = 4

// seedRaceWorld builds one race world, rebuilding it if the seed loses a
// deadlock.
//
// The seed is not what this test asserts, but it writes its rows for real
// against the database the whole suite shares, and `go test ./...` runs
// every other package alongside this one. Its own row locks are harmless:
// each is on a row this attempt created moments earlier - the FOR SHARE
// article_insert_guard takes on the approver's account row (0002), the
// key-share the foreign keys take on its own source_item. The two rows it
// shares with the rest of the suite - language 'el', which its source
// insert references, and place 'munich', which its article_place insert
// does - it holds in key-share, and key-share conflicts with key-share.
// Nothing in the tree ever updates or deletes a language or a place row,
// so no transaction ever waits on one, and a lock nobody waits on is no
// edge in any cycle.
//
// What can actually cycle is a table lock. TestImmutableTablesRejectTruncate
// in internal/platform/db runs `truncate article cascade` and `truncate
// source_item cascade` inside an open transaction, which takes ACCESS
// EXCLUSIVE on article and, through the cascade, on article_place. That
// test declines t.Parallel() for exactly this reason - but declining it
// serialises the subtests in its own package, not this one. Meanwhile the
// foreign key allows this transaction no order but article first, its
// place row second, so it can be holding article and asking for
// article_place at the moment the truncate holds article_place and asks
// for article. Postgres resolves that the way it must: it aborts one side
// with 40P01 and leaves the retry to the application.
//
// A seed that dies on that abort reports a suite-wide lock cycle as a
// failure of the very serialisation this test exists to prove, which is
// the loudest possible way of saying the wrong thing. So it retries - and
// only it. The race below, the two blocked statements and every assertion
// about them, runs exactly once against the world this returns, so
// nothing here can soften what the test proves.
func seedRaceWorld(ctx context.Context, t *testing.T, conn *pgx.Conn) raceWorld {
	t.Helper()
	for attempt := 1; ; attempt++ {
		world, err := trySeedRaceWorld(ctx, t, conn)
		if err == nil {
			return world
		}
		var pgErr *pgconn.PgError
		deadlock := errors.As(err, &pgErr) && pgErr.Code == pgerrcode.DeadlockDetected
		if !deadlock || attempt == seedAttempts {
			t.Fatalf("seeding the race world (attempt %d of %d): %v%s",
				attempt, seedAttempts, err, pgDetail(err))
		}
		// A seed that loses a deadlock and then succeeds says nothing in
		// CI: `go test` without -v discards a passing test's output whole,
		// t.Log buffer and stderr alike. This line is for a local run, and
		// for the failing case, where the testing package flushes the
		// earlier attempts alongside the fatal one. The record that
		// survives a green run is Postgres's own deadlock report, which
		// the workflow prints from the service container after the suite.
		t.Logf("seed attempt %d of %d lost a deadlock, rebuilding: %v%s",
			attempt, seedAttempts, err, pgDetail(err))
	}
}

// pgDetail renders Postgres's DETAIL line, which the error's own text drops.
// For a deadlock it names both sides of the cycle and the lock each was
// waiting on - by backend pid, not by statement, since the statements stay
// in the server log - and that is still the most a failing run carries
// away about a cycle nobody can reproduce on demand. So it belongs on the
// retry log and on the final failure alike: the last attempt is the report
// nobody gets to ask a follow-up question about.
func pgDetail(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Detail == "" {
		return ""
	}
	return " (detail: " + pgErr.Detail + ")"
}

// trySeedRaceWorld is one attempt at the seed. It returns its failures
// instead of ending the test so the caller can tell a deadlock - the one
// error worth another go - from everything else.
func trySeedRaceWorld(ctx context.Context, t *testing.T, conn *pgx.Conn) (raceWorld, error) {
	t.Helper()
	// A fresh suffix per attempt. A lost attempt may already have
	// committed some of its rows; re-seeding from nothing leaves them
	// behind rather than colliding with them, which is what this test
	// does with its rows anyway.
	suffix := randomSuffix(t)

	editor := func(name string) (string, error) {
		var id string
		if err := conn.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
			name+"-"+suffix+"@example.test", "Publication Race "+name+" "+suffix).Scan(&id); err != nil {
			return "", fmt.Errorf("seed %s: %w", name, err)
		}
		return id, nil
	}
	approverID, err := editor("approver")
	if err != nil {
		return raceWorld{}, err
	}
	publisherID, err := editor("publisher")
	if err != nil {
		return raceWorld{}, err
	}

	var sourceID string
	if err := conn.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted (race test)') returning id`,
		"Publication Race Feed "+suffix, "https://example.test/publication-race/"+suffix).Scan(&sourceID); err != nil {
		return raceWorld{}, fmt.Errorf("seed source: %w", err)
	}
	var itemID string
	if err := conn.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, raw_body)
		 values ($1, $2, $3, $4) returning id`,
		sourceID, "https://example.test/publication-race/"+suffix+"/item",
		"Τίτλος "+suffix, "Σώμα "+suffix).Scan(&itemID); err != nil {
		return raceWorld{}, fmt.Errorf("seed source_item: %w", err)
	}
	// Approved, never published: exactly the state the publication endpoint
	// acts on. The article and its place row are one transaction: this
	// seed commits for real, and the 0006 constraint trigger checks at
	// COMMIT that the article names at least one place.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return raceWorld{}, fmt.Errorf("begin article seed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var articleID string
	if err := tx.QueryRow(ctx,
		`insert into article (source_item_id, approved_by, attribution_block)
		 values ($1, $2, $3) returning id`,
		itemID, approverID, "Πηγή: Publication Race Feed "+suffix).Scan(&articleID); err != nil {
		return raceWorld{}, fmt.Errorf("seed article: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into article_place (article_id, place_id)
		 select $1, id from place where slug = 'munich'`, articleID); err != nil {
		return raceWorld{}, fmt.Errorf("seed article place: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return raceWorld{}, fmt.Errorf("commit article seed: %w", err)
	}
	return raceWorld{
		publisherID: uuid.MustParse(publisherID),
		articleID:   uuid.MustParse(articleID),
	}, nil
}

func TestPublicationRaceIsSerialized(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the publication race")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
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

	published := func(t *testing.T, articleID uuid.UUID) bool {
		t.Helper()
		var at *time.Time
		if err := connA.QueryRow(ctx, `select published_at from article where id = $1`, articleID).Scan(&at); err != nil {
			t.Fatalf("reading published_at: %v", err)
		}
		return at != nil
	}

	t.Run("a demotion waits for an in-flight publication", func(t *testing.T) {
		world := seedRaceWorld(ctx, t, connA)

		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatalf("begin publication tx: %v", err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		// The store takes FOR SHARE on the publisher's account row and holds
		// it until this transaction ends.
		if _, err := editorial.NewPGStore(txA).Publish(ctx, world.articleID, world.publisherID); err != nil {
			t.Fatalf("publish: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := connB.Exec(ctx, `update account set role = 'reader' where id = $1`, world.publisherID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the demotion finished while the publication was uncommitted (err=%v): the role read did not serialize", err)
		case <-time.After(blockDetectionWindow):
			// Blocked on the share lock, as required.
		}

		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("commit publication: %v", err)
		}
		select {
		case err := <-done:
			// The publisher approved nothing, so account_role_guard has no
			// reason to refuse: the demotion simply lands after the
			// publication it waited for, which is the whole point - the two
			// happened in a defined order rather than both against the past.
			if err != nil {
				t.Fatalf("demotion after the committed publication: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the demotion is still blocked after the publication committed")
		}
		if !published(t, world.articleID) {
			t.Error("the article is not published although its transaction committed")
		}
	})

	t.Run("a publication waits for an in-flight demotion, then is refused", func(t *testing.T) {
		world := seedRaceWorld(ctx, t, connA)

		txB, err := connB.Begin(ctx)
		if err != nil {
			t.Fatalf("begin demotion tx: %v", err)
		}
		defer func() { _ = txB.Rollback(ctx) }()
		if _, err := txB.Exec(ctx, `update account set role = 'reader' where id = $1`, world.publisherID); err != nil {
			t.Fatalf("demotion: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := editorial.NewPGStore(connA).Publish(ctx, world.articleID, world.publisherID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the publication finished while the demotion was uncommitted (err=%v): the role read did not serialize", err)
		case <-time.After(blockDetectionWindow):
			// Blocked on the demotion's row lock, as required.
		}

		if err := txB.Commit(ctx); err != nil {
			t.Fatalf("commit demotion: %v", err)
		}
		select {
		case err := <-done:
			// Resuming, the locking read sees the committed demotion rather
			// than the snapshot it started with, and refuses.
			if !errors.Is(err, editorial.ErrNotEditor) {
				t.Fatalf("publish after the committed demotion = %v, want ErrNotEditor", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the publication is still blocked after the demotion committed")
		}
		if published(t, world.articleID) {
			t.Error("a demoted account published an article; publication must not outlive the role that authorised it")
		}
	})
}
