package main

// The whole point of issue #84, asserted end to end: an article approved
// through the real approval path is reachable on the front page of the
// places the approval named. The two halves live in different modules -
// the approval in editorial, the front page in content - and the arch
// test forbids either importing the other, so the one place they may meet
// is here, the composition root, exactly as with the wiring tests.

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	contentstore "github.com/Nomos-N4s/apivo-news/internal/content/store"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// TestAnApprovedArticleAppearsOnTheFrontPageForItsPlace seeds an origin,
// approves it with publish through editorial.PGStore - the same code the
// endpoint runs, places included - and then reads the front page the way
// the reader module does. Everything happens inside one rolled-back
// transaction: the approval's own savepoint commits into it, both stores
// share it, and the database is left clean.
func TestAnApprovedArticleAppearsOnTheFrontPageForItsPlace(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the approval-to-front-page chain")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	suffix := randomHex(t)
	var editorID string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"front-"+suffix+"@example.test", "Front Page Editor "+suffix).Scan(&editorID); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted (front-page test)') returning id`,
		"Front Page Feed "+suffix, "https://example.test/front/"+suffix).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	var itemID string
	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, raw_body)
		 values ($1, $2, $3, $4) returning id`,
		sourceID, "https://example.test/front/"+suffix+"/item",
		"Τίτλος "+suffix, "Πρώτη πρόταση. Δεύτερη πρόταση. "+suffix).Scan(&itemID); err != nil {
		t.Fatalf("seed source_item: %v", err)
	}

	origin := uuid.MustParse(itemID)
	article, err := editorial.NewPGStore(tx).Approve(ctx, editorial.NewApproval{
		SourceItemID: &origin,
		Attribution:  "Πηγή: Front Page Feed " + suffix,
		Publish:      true,
		Places:       []string{"munich", "greece"},
		ApprovedBy:   uuid.MustParse(editorID),
	})
	if err != nil {
		t.Fatalf("approving through the real path: %v", err)
	}

	frontPage := func(t *testing.T, places []string) []contentstore.ListFrontPageRow {
		t.Helper()
		rows, err := contentstore.New(tx).ListFrontPage(ctx, contentstore.ListFrontPageParams{
			Lang:     "el",
			Places:   places,
			RowLimit: 100,
		})
		if err != nil {
			t.Fatalf("listing the front page for %v: %v", places, err)
		}
		return rows
	}
	find := func(rows []contentstore.ListFrontPageRow) *contentstore.ListFrontPageRow {
		want := pgtype.UUID{Bytes: article.ID, Valid: true}
		for i := range rows {
			if rows[i].ID == want {
				return &rows[i]
			}
		}
		return nil
	}

	// Reachable on each place the approval named, exactly once, and the
	// row itself states the places it carries.
	for _, place := range []string{"munich", "greece"} {
		row := find(frontPage(t, []string{place}))
		if row == nil {
			t.Fatalf("the approved article is missing from the %s front page", place)
		}
		if want := []string{"greece", "munich"}; !slices.Equal(row.PlaceSlugs, want) {
			t.Errorf("place_slugs on the %s front page = %v, want %v", place, row.PlaceSlugs, want)
		}
	}
	// Tagged to several requested places, it still appears exactly once.
	both := frontPage(t, []string{"munich", "greece"})
	seen := 0
	for i := range both {
		if both[i].ID == (pgtype.UUID{Bytes: article.ID, Valid: true}) {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the article appears %d times on the munich+greece front page, want exactly once", seen)
	}
	// And absent from a place the approval did not name.
	if find(frontPage(t, []string{"bavaria"})) != nil {
		t.Error("the article appears on the bavaria front page although the approval never named bavaria")
	}
}
