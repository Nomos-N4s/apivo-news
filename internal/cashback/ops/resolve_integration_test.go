package ops_test

// Resolution against the real, migrated schema (T112, US6, 0030).
//
// What only the database can prove: that the verdict, the reason, the
// person and the moment land together, with the event, or not at all; that
// two operators deciding the same row end with one verdict and one refusal
// that names the other; and that resolving moves nothing but the row.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// anOpenDifference imports a statement paying a report short and detects
// the one difference that makes, answering its id.
func anOpenDifference(ctx context.Context, t *testing.T, tx pgx.Tx, store *ops.PGStore, p statementParties) uuid.UUID {
	t.Helper()
	reported(ctx, t, tx, p, "A", 499, inAugust, uuid.Nil)
	run := imports(ctx, t, store, p, `{"lines":[{"transaction_id":"A","paid":{"minor":450,"currency":"EUR"}}]}`)
	if _, err := store.DetectDifferences(ctx, run); err != nil {
		t.Fatalf("detecting: %v", err)
	}
	rows := differencesOf(ctx, t, tx, run)
	if len(rows) != 1 {
		t.Fatalf("%d differences, want the one shortfall", len(rows))
	}
	return rows[0].id
}

// resolutionEvents reads the resolution announcements for one difference.
func resolutionEvents(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := tx.Query(ctx, `select payload from domain_event where type = $1 and subject = $2 order by occurred_at`,
		ops.TypeDifferenceResolved, id.String())
	if err != nil {
		t.Fatalf("reading the resolution events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var payload map[string]any
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scanning a resolution event: %v", err)
		}
		out = append(out, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the resolution events: %v", err)
	}
	return out
}

func TestDifferenceResolutionAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaPool(t)
	defer done()
	parties := seedStatementParties(ctx, t, tx)

	each(ctx, t, tx, "a decision lands whole, with its event", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		id := anOpenDifference(ctx, t, tx, store, parties)
		entriesBefore, transitionsBefore := entriesAndTransitions(ctx, t, tx)

		resolved, err := store.ResolveDifference(ctx, ops.Resolution{
			ID: id, Verdict: ops.VerdictAbsorbed, Reason: "  a 49 cent shortfall is not worth the dispute  ", Operator: parties.operator,
		})
		if err != nil {
			t.Fatalf("ResolveDifference(): %v", err)
		}
		switch {
		case resolved.ID != id:
			t.Errorf("resolved %s, want %s", resolved.ID, id)
		case resolved.Kind != ops.AmountMismatch:
			t.Errorf("kind = %s, want amount_mismatch", resolved.Kind)
		case resolved.Verdict != ops.VerdictAbsorbed:
			t.Errorf("verdict = %s, want absorbed", resolved.Verdict)
		case resolved.ResolvedBy != parties.operator.ID:
			t.Errorf("resolved by %s, want %s", resolved.ResolvedBy, parties.operator.ID)
		case resolved.Reason != "a 49 cent shortfall is not worth the dispute":
			t.Errorf("reason = %q, want it trimmed", resolved.Reason)
		case resolved.ResolvedAt.IsZero():
			t.Error("no resolved_at")
		case resolved.Run == uuid.Nil:
			t.Error("no run")
		}

		var (
			by         uuid.UUID
			reason     string
			resolution string
		)
		if err := tx.QueryRow(ctx, `
			select resolved_by, resolved_reason, resolution from cashback.reconciliation_difference
			 where id = $1 and resolved_at is not null`, id).Scan(&by, &reason, &resolution); err != nil {
			t.Fatalf("reading the row back: %v", err)
		}
		if by != parties.operator.ID || resolution != "absorbed" || reason != resolved.Reason {
			t.Errorf("the row holds by=%s resolution=%s reason=%q; want the decision as returned", by, resolution, reason)
		}

		events := resolutionEvents(ctx, t, tx, id)
		if len(events) != 1 {
			t.Fatalf("%d resolution events, want 1", len(events))
		}
		payload := events[0]
		switch {
		case payload["difference_id"] != id.String():
			t.Errorf("the event names difference %v, want %s", payload["difference_id"], id)
		case payload["run_id"] != resolved.Run.String():
			t.Errorf("the event names run %v, want %s", payload["run_id"], resolved.Run)
		case payload["kind"] != "amount_mismatch":
			t.Errorf("the event says kind %v", payload["kind"])
		case payload["resolution"] != "absorbed":
			t.Errorf("the event says resolution %v", payload["resolution"])
		case payload["resolved_by"] != parties.operator.ID.String():
			t.Errorf("the event names resolver %v, want %s (FR-061)", payload["resolved_by"], parties.operator.ID)
		case payload["reason"] != resolved.Reason:
			t.Errorf("the event carries reason %v, want %q (FR-061)", payload["reason"], resolved.Reason)
		}

		if entries, transitions := entriesAndTransitions(ctx, t, tx); entries != entriesBefore || transitions != transitionsBefore {
			t.Errorf("resolving changed entries (%d -> %d) or transitions (%d -> %d); it must move nothing but the row",
				entriesBefore, entries, transitionsBefore, transitions)
		}
	})

	each(ctx, t, tx, "the second operator is told who decided first, and what", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		id := anOpenDifference(ctx, t, tx, store, parties)
		first, err := store.ResolveDifference(ctx, ops.Resolution{
			ID: id, Verdict: ops.VerdictExplained, Reason: "paid in full on the September statement", Operator: parties.operator,
		})
		if err != nil {
			t.Fatalf("the first decision: %v", err)
		}
		var colleague uuid.UUID
		if err := tx.QueryRow(ctx, `insert into public.account (email, display_name, role) values ($1, 'Second Operator', 'operator') returning id`,
			"second-"+suffix(t)+"@example.test").Scan(&colleague); err != nil {
			t.Fatalf("seeding the second operator: %v", err)
		}
		_, err = store.ResolveDifference(ctx, ops.Resolution{
			ID: id, Verdict: ops.VerdictAbsorbed, Reason: "write it off", Operator: ops.Operator{ID: colleague},
		})
		var already ops.AlreadyResolvedError
		if !errors.As(err, &already) {
			t.Fatalf("the second decision = %v, want AlreadyResolvedError", err)
		}
		if already.ID != id || already.By != parties.operator.ID || already.Verdict != first.Verdict || already.Reason != first.Reason || !already.At.Equal(first.ResolvedAt) {
			t.Errorf("AlreadyResolvedError = %+v, want the first decision (%+v)", already, first)
		}
		var resolution string
		if err := tx.QueryRow(ctx, `select resolution from cashback.reconciliation_difference where id = $1`, id).Scan(&resolution); err != nil {
			t.Fatalf("reading the row: %v", err)
		}
		if resolution != string(first.Verdict) {
			t.Errorf("the row now says %s; the first verdict was overwritten", resolution)
		}
		if n := len(resolutionEvents(ctx, t, tx, id)); n != 1 {
			t.Errorf("%d resolution events, want the first only", n)
		}
	})

	each(ctx, t, tx, "an id that names nothing is said to", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		_, err := store.ResolveDifference(ctx, ops.Resolution{
			ID: uuid.New(), Verdict: ops.VerdictExplained, Reason: "no such row", Operator: parties.operator,
		})
		if !errors.Is(err, ops.ErrNoSuchDifference) {
			t.Errorf("ResolveDifference() = %v, want one wrapping ErrNoSuchDifference", err)
		}
	})

	each(ctx, t, tx, "a decision the schema would refuse never reaches it", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		id := anOpenDifference(ctx, t, tx, store, parties)
		_, err := store.ResolveDifference(ctx, ops.Resolution{ID: id, Verdict: "report_stands", Reason: "chasing", Operator: parties.operator})
		if !errors.Is(err, ops.ErrInvalidResolution) {
			t.Fatalf("ResolveDifference() = %v, want one wrapping ErrInvalidResolution", err)
		}
		var open bool
		if err := tx.QueryRow(ctx, `select resolved_at is null from cashback.reconciliation_difference where id = $1`, id).Scan(&open); err != nil {
			t.Fatalf("reading the row: %v", err)
		}
		if !open {
			t.Error("a refused resolution closed the row")
		}
	})

	each(ctx, t, tx, "a decision by somebody who is no account leaves nothing behind", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		id := anOpenDifference(ctx, t, tx, store, parties)
		_, err := store.ResolveDifference(ctx, ops.Resolution{
			ID: id, Verdict: ops.VerdictExplained, Reason: "explained", Operator: ops.Operator{ID: uuid.New()},
		})
		if !errors.Is(err, ops.ErrNotResolved) {
			t.Fatalf("ResolveDifference() = %v, want one wrapping ErrNotResolved", err)
		}
		var open bool
		if err := tx.QueryRow(ctx, `select resolved_at is null from cashback.reconciliation_difference where id = $1`, id).Scan(&open); err != nil {
			t.Fatalf("reading the row: %v", err)
		}
		if !open || len(resolutionEvents(ctx, t, tx, id)) != 0 {
			t.Error("a refused resolution left a closed row or an event behind")
		}
	})
}
