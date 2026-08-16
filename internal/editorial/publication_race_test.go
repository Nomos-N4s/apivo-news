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
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

func seedRaceWorld(ctx context.Context, t *testing.T, conn *pgx.Conn) raceWorld {
	t.Helper()
	suffix := randomSuffix(t)

	editor := func(name string) string {
		t.Helper()
		var id string
		if err := conn.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
			name+"-"+suffix+"@example.test", "Publication Race "+name+" "+suffix).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	approverID, publisherID := editor("approver"), editor("publisher")

	var sourceID string
	if err := conn.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted (race test)') returning id`,
		"Publication Race Feed "+suffix, "https://example.test/publication-race/"+suffix).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	var itemID string
	if err := conn.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, raw_body)
		 values ($1, $2, $3, $4) returning id`,
		sourceID, "https://example.test/publication-race/"+suffix+"/item",
		"Τίτλος "+suffix, "Σώμα "+suffix).Scan(&itemID); err != nil {
		t.Fatalf("seed source_item: %v", err)
	}
	// Approved, never published: exactly the state the publication endpoint
	// acts on. The article and its place row are one transaction: this
	// seed commits for real, and the 0006 constraint trigger checks at
	// COMMIT that the article names at least one place.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin article seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var articleID string
	if err := tx.QueryRow(ctx,
		`insert into article (source_item_id, approved_by, attribution_block)
		 values ($1, $2, $3) returning id`,
		itemID, approverID, "Πηγή: Publication Race Feed "+suffix).Scan(&articleID); err != nil {
		t.Fatalf("seed article: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into article_place (article_id, place_id)
		 select $1, id from place where slug = 'munich'`, articleID); err != nil {
		t.Fatalf("seed article place: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit article seed: %v", err)
	}
	return raceWorld{
		publisherID: uuid.MustParse(publisherID),
		articleID:   uuid.MustParse(articleID),
	}
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
