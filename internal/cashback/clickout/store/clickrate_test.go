package store_test

// The two counts the click rule reads, against the real, migrated schema
// (T066, US7 scenario 1).
//
// Separate from the click statements' own tests because the subject is
// different: those ask whether a click can be recorded and found, these ask
// whether the rule can be applied without reading the whole table.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// TestTheClickRuleCountsAndUsesItsIndexes is the other half of the click
// store: the two counts the rule reads (T066, US7 scenario 1).
//
// The plans are asserted, not just the numbers. Both counts run on the
// click-out path - the one request a member is waiting on before a
// redirect - and a sequential scan of every click ever recorded would pass
// every correctness test here while getting slower for the life of the
// product.
func TestTheClickRuleCountsAndUsesItsIndexes(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the click rule")
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

	each(ctx, t, tx, "a member's clicks are counted, with the oldest in the window", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		params := aClick(ctx, t, tx)
		for range 3 {
			params.ClickRef = aClickRef(t)
			if _, err := q.InsertClick(ctx, params); err != nil {
				t.Fatalf("InsertClick(): %v", err)
			}
		}
		// One more for a different member, which must not be counted: the
		// rule is about one account, and counting somebody else's clicks
		// would refuse a member for a stranger's browsing.
		other := aClick(ctx, t, tx)
		if _, err := q.InsertClick(ctx, other); err != nil {
			t.Fatalf("InsertClick() for another member: %v", err)
		}

		row, err := q.CountRecentClicksByAccount(ctx, store.CountRecentClicksByAccountParams{
			AccountID: params.AccountID,
			Since:     pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("CountRecentClicksByAccount(): %v", err)
		}
		if row.Clicks != 3 {
			t.Errorf("counted %d clicks, want 3 - this member's and nobody else's", row.Clicks)
		}
		if !row.Oldest.Valid {
			t.Error("the count carries no oldest click; without it a 429 cannot say when the rule lifts")
		}

		// And a window that starts after them counts none, which is what
		// makes the rule a window rather than a total.
		empty, err := q.CountRecentClicksByAccount(ctx, store.CountRecentClicksByAccountParams{
			AccountID: params.AccountID,
			Since:     pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("CountRecentClicksByAccount(): %v", err)
		}
		if empty.Clicks != 0 || empty.Oldest.Valid {
			t.Errorf("a window with nothing in it counted %+v, want no clicks and no oldest", empty)
		}
	})

	each(ctx, t, tx, "a context's clicks are counted, and a click with none is not a context", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		digest := "ctx-" + suffix(t)
		params := aClick(ctx, t, tx)
		params.ContextDigest = pgtype.Text{String: digest, Valid: true}
		for range 2 {
			params.ClickRef = aClickRef(t)
			if _, err := q.InsertClick(ctx, params); err != nil {
				t.Fatalf("InsertClick(): %v", err)
			}
		}
		// A click with no digest at all: not this context, and not any.
		noContext := aClick(ctx, t, tx)
		if _, err := q.InsertClick(ctx, noContext); err != nil {
			t.Fatalf("InsertClick() with no context: %v", err)
		}

		row, err := q.CountRecentClicksByContext(ctx, store.CountRecentClicksByContextParams{
			ContextDigest: pgtype.Text{String: digest, Valid: true},
			Since:         pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("CountRecentClicksByContext(): %v", err)
		}
		if row.Clicks != 2 {
			t.Errorf("counted %d clicks, want 2 - this context's only", row.Clicks)
		}
	})

	each(ctx, t, tx, "both counts read an index rather than the whole table", func(t *testing.T, tx pgx.Tx, _ *store.Queries) {
		// Planner choices depend on statistics, and an empty table is
		// planned as a sequential scan whatever indexes exist. Disabling
		// the sequential scan asks the question this test means: IS there
		// an index that can answer this, at all.
		if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
			t.Fatalf("disabling sequential scans: %v", err)
		}

		for _, probe := range []struct{ name, sql, index string }{
			{
				name:  "the member half",
				sql:   `select count(*), min(clicked_at) from cashback.click where account_id = $1 and clicked_at > $2`,
				index: "click_account_clicked_at_idx",
			},
			{
				name:  "the context half",
				sql:   `select count(*), min(clicked_at) from cashback.click where context_digest = $1 and clicked_at > $2`,
				index: "click_context_clicked_at_idx",
			},
		} {
			var arg any = uuid.New().String()
			if probe.index == "click_context_clicked_at_idx" {
				arg = "a-digest"
			}
			rows, err := tx.Query(ctx, "explain "+probe.sql, arg, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("%s: explain: %v", probe.name, err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					t.Fatalf("%s: scanning the plan: %v", probe.name, err)
				}
				plan.WriteString(line + "\n")
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("%s: reading the plan: %v", probe.name, err)
			}
			if !strings.Contains(plan.String(), probe.index) {
				t.Errorf("%s does not use %s; plan was:\n%s", probe.name, probe.index, plan.String())
			}
		}
	})
}
