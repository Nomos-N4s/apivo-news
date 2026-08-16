package editorial_test

// What the source.updated event may claim was replaced (#118).
//
// UpdateSource reads the pre-image the audit line carries. Under READ
// COMMITTED - the pool's isolation level - an UPDATE that blocks on a
// concurrent committed update re-fetches its target row when it resumes
// (EvalPlanQual), but an ordinary join scan of the same table keeps
// answering from the statement's ORIGINAL snapshot. A pre-image read that
// way is the value from before the race, not the value the write actually
// replaced: the intervening version's reign vanishes from an append-only
// stream, and a patch restating what another editor just set looks like
// an edit and appends an event for it.
//
// A row lock is invisible inside a single rolled-back transaction, so
// this race needs two real sessions and real commits. The source rows are
// deleted afterwards; their events are not, because domain_event refuses
// updates and deletes by trigger - the very property these assertions
// exist to protect.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

func TestSourceUpdateRecordsTheValueItReplaced(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the source edit race")
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

	// seedRacedSource commits one source for real - nothing else exposes a
	// row lock - and deletes it again when the subtest ends. It never
	// yielded an item, so the source_item FK has nothing to refuse.
	seedRacedSource := func(t *testing.T, name, terms string) uuid.UUID {
		t.Helper()
		suffix := randomSuffix(t)
		var id string
		if err := connA.QueryRow(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms)
			 values ($1, $2, 'el', 'GR', $3) returning id`,
			name+" "+suffix, "https://example.test/source-edit-race/"+suffix, terms).Scan(&id); err != nil {
			t.Fatalf("seeding the raced source: %v", err)
		}
		t.Cleanup(func() {
			if _, err := connA.Exec(context.Background(), `delete from source where id = $1`, id); err != nil {
				t.Errorf("cleaning up the raced source %s: %v", id, err)
			}
		})
		return uuid.MustParse(id)
	}

	// blockedEdit starts one edit on session B and reports it as a channel
	// that must not answer until the transaction it collides with commits.
	blockedEdit := func(t *testing.T, id uuid.UUID, patch editorial.SourcePatch) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := editorial.NewPGStore(connB).UpdateSource(context.Background(), id, uuid.New(), patch)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("session B's edit finished while session A's was uncommitted (err=%v): the two writes did not serialize", err)
		case <-time.After(blockDetectionWindow):
			// Waiting on session A's row lock, as required.
		}
		return done
	}

	awaitEdit := func(t *testing.T, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("session B's edit after session A committed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("session B's edit is still blocked after session A committed")
		}
	}

	t.Run("the recorded pre-image is the version the write replaced", func(t *testing.T) {
		id := seedRacedSource(t, "Licence Race Feed", "Terms v1")

		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatalf("begin session A: %v", err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		v2, v3 := "Terms v2", "Terms v3"
		if _, err := editorial.NewPGStore(txA).UpdateSource(ctx, id, uuid.New(),
			editorial.SourcePatch{LicenceTerms: &v2}); err != nil {
			t.Fatalf("session A's edit: %v", err)
		}

		done := blockedEdit(t, id, editorial.SourcePatch{LicenceTerms: &v3})
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("commit session A: %v", err)
		}
		awaitEdit(t, done)

		var replaced string
		if err := connA.QueryRow(ctx,
			`select payload->'licence_terms'->>'old'
			   from domain_event
			  where type = 'source.updated'
			    and payload->>'source_id' = $1
			    and payload->'licence_terms'->>'new' = $2`, id.String(), v3).Scan(&replaced); err != nil {
			t.Fatalf("reading the second edit's audit line: %v", err)
		}
		if replaced != v2 {
			t.Errorf("the event says %q became %q, but the row held %q when the write landed: %q's reign is erased from an append-only stream",
				replaced, v3, v2, v2)
		}

		// The row itself is the last write, and the chain the stream tells
		// must end where the row does.
		var stored string
		if err := connA.QueryRow(ctx, `select licence_terms from source where id = $1`, id).Scan(&stored); err != nil {
			t.Fatalf("reading the raced row back: %v", err)
		}
		if stored != v3 {
			t.Errorf("licence_terms = %q, want %q - the later write must win", stored, v3)
		}
	})

	t.Run("restating what another editor just set appends no event", func(t *testing.T) {
		id := seedRacedSource(t, "Rename Race Feed", "Terms v1")
		renamed := "Renamed by A " + randomSuffix(t)

		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatalf("begin session A: %v", err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		if _, err := editorial.NewPGStore(txA).UpdateSource(ctx, id, uuid.New(),
			editorial.SourcePatch{Name: &renamed}); err != nil {
			t.Fatalf("session A's rename: %v", err)
		}

		// B submits the name A is already committing: against the row it
		// will actually meet, this edit changes nothing.
		done := blockedEdit(t, id, editorial.SourcePatch{Name: &renamed})
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("commit session A: %v", err)
		}
		awaitEdit(t, done)

		var events int
		if err := connA.QueryRow(ctx,
			`select count(*) from domain_event
			  where type = 'source.updated'
			    and payload->>'source_id' = $1
			    and payload->'name'->>'new' = $2`, id.String(), renamed).Scan(&events); err != nil {
			t.Fatalf("counting the rename events: %v", err)
		}
		if events != 1 {
			t.Errorf("source.updated events naming %q = %d, want 1 - session B changed nothing and the stream records edits, not re-statements",
				renamed, events)
		}
	})
}
