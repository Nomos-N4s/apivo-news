package account_test

// The tour store against the real schema: what it reads back, what it
// records, where the cap bites, and how it answers about an account that
// is not there (migration 0009).

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/account"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// storeTx opens a transaction the test rolls back, with one account in it.
func storeTx(t *testing.T) (context.Context, pgx.Tx, uuid.UUID) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the tour store")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	})
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into account (email, display_name, role)
		values ($1, 'Tour Taker', 'reader') returning id`,
		"tours-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the account: %v", err)
	}
	return ctx, tx, id
}

func TestTheStoreRecordsACursorAndReadsItBack(t *testing.T) {
	t.Parallel()
	ctx, tx, id := storeTx(t)
	store := account.NewPGStore(tx)

	tours, err := store.Tours(ctx, id)
	if err != nil || len(tours) != 0 {
		t.Fatalf("a fresh account reads %v (err %v), want an empty document", tours, err)
	}
	stored, err := store.SetTour(ctx, id, "editor", "step-3")
	if err != nil || !stored {
		t.Fatalf("SetTour() = %v, %v; want recorded", stored, err)
	}
	// Moving a cursor already present is always allowed.
	if stored, err := store.SetTour(ctx, id, "editor", "done"); err != nil || !stored {
		t.Fatalf("moving a cursor = %v, %v; want recorded", stored, err)
	}
	tours, err = store.Tours(ctx, id)
	if err != nil || tours["editor"] != "done" || len(tours) != 1 {
		t.Fatalf("Tours() = %v (err %v), want {editor: done}", tours, err)
	}
}

// TestTheCapBoundsHowManyToursNotHowOftenTheyMove. The cap refuses a NEW key
// once the account holds MaxToursPerAccount; a key already present still
// moves, because the cap bounds the document, not the person.
func TestTheCapBoundsHowManyToursNotHowOftenTheyMove(t *testing.T) {
	t.Parallel()
	ctx, tx, id := storeTx(t)
	store := account.NewPGStore(tx)

	for i := range account.MaxToursPerAccount {
		if stored, err := store.SetTour(ctx, id, "tour-"+strconv.Itoa(i), "start"); err != nil || !stored {
			t.Fatalf("tour %d: SetTour() = %v, %v; want recorded", i, stored, err)
		}
	}
	if stored, err := store.SetTour(ctx, id, "one-too-many", "start"); err != nil || stored {
		t.Fatalf("past the cap SetTour() = %v, %v; want refused without error", stored, err)
	}
	if stored, err := store.SetTour(ctx, id, "tour-0", "finished"); err != nil || !stored {
		t.Fatalf("at the cap, moving an existing cursor = %v, %v; want recorded", stored, err)
	}
	tours, err := store.Tours(ctx, id)
	if err != nil || len(tours) != account.MaxToursPerAccount || tours["tour-0"] != "finished" {
		t.Fatalf("Tours() holds %d keys with tour-0=%q (err %v), want %d and finished", len(tours), tours["tour-0"], err, account.MaxToursPerAccount)
	}
}

func TestAnAccountThatIsNotThereIsSaidByName(t *testing.T) {
	t.Parallel()
	ctx, tx, _ := storeTx(t)
	store := account.NewPGStore(tx)
	nobody := uuid.New()

	if _, err := store.Tours(ctx, nobody); !errors.Is(err, account.ErrNoAccount) || !strings.Contains(err.Error(), nobody.String()) {
		t.Errorf("Tours(nobody) = %v, want ErrNoAccount naming %s", err, nobody)
	}
	if _, err := store.SetTour(ctx, nobody, "editor", "step-1"); !errors.Is(err, account.ErrNoAccount) {
		t.Errorf("SetTour(nobody) = %v, want ErrNoAccount", err)
	}
}

// TestADocumentThatIsNotFlatIsReportedNotHalved. The column is constrained
// to an object; what is inside it was written by a client. A nested value
// cannot be read as a cursor, and the store says so rather than handing
// back half a document.
func TestADocumentThatIsNotFlatIsReportedNotHalved(t *testing.T) {
	t.Parallel()
	ctx, tx, id := storeTx(t)
	if _, err := tx.Exec(ctx, `update account set tour_progress = '{"editor": {"step": 3}}'::jsonb where id = $1`, id); err != nil {
		t.Fatalf("writing a nested document: %v", err)
	}
	if tours, err := account.NewPGStore(tx).Tours(ctx, id); err == nil || !strings.Contains(err.Error(), "not a flat object") {
		t.Errorf("Tours() = %v, %v; want a refusal naming the shape", tours, err)
	}
}
