package ops_test

// Detection against the real, migrated schema (T111, US6, 0029).
//
// Derive is proved as a table in differences_test.go. What only the database
// can prove is here: that what was derived lands as rows the schema accepts
// and the gate reads; that a repeat records nothing and announces nothing;
// that a difference an operator resolved is not raised at them again; that
// the report compared is the network's latest word; and that none of it
// touches an entry.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// reported stores one confirmed report in euros for the parties' account and
// answers its id. Confirmed and euros only: what the other statuses and a
// second currency mean to detection is the derivation table's to prove, and
// it proves it without a database.
// supersedes, when not Nil, makes it the transaction's newer word.
func reported(ctx context.Context, t *testing.T, tx pgx.Tx, p statementParties, externalID string, commission int64, at time.Time, supersedes uuid.UUID) uuid.UUID {
	t.Helper()
	var supersedesArg any
	if supersedes != uuid.Nil {
		supersedesArg = supersedes
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload, supersedes_id)
		values ($1, $2, $3, null, 'confirmed', 'confirmed', 4999, $4, 'EUR', $5, $6, $7, $8, $9, $10)
		returning id`,
		p.network, p.account, externalID, commission,
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"`+externalID+`","status":"confirmed"}`), supersedesArg,
	).Scan(&id); err != nil {
		t.Fatalf("storing report %s: %v", externalID, err)
	}
	return id
}

// storedDifference is a queue row as the database holds it.
type storedDifference struct {
	id         uuid.UUID
	kind       string
	report     *uuid.UUID
	line       *string
	expected   *int64
	actual     *int64
	currency   string
	resolvedAt *time.Time
}

// differencesOf reads a run's queue rows, oldest first.
func differencesOf(ctx context.Context, t *testing.T, tx pgx.Tx, run uuid.UUID) []storedDifference {
	t.Helper()
	rows, err := tx.Query(ctx, `
		select id, kind, network_transaction_id, statement_transaction_id, expected_minor, actual_minor, currency, resolved_at
		  from cashback.reconciliation_difference
		 where run_id = $1
		 order by detected_at, kind, statement_transaction_id`, run)
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	defer rows.Close()
	var out []storedDifference
	for rows.Next() {
		var d storedDifference
		if err := rows.Scan(&d.id, &d.kind, &d.report, &d.line, &d.expected, &d.actual, &d.currency, &d.resolvedAt); err != nil {
			t.Fatalf("scanning a queue row: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	return out
}

// foundEvents reads the difference_found payloads announced for one run,
// keyed by the difference they announce.
func foundEvents(ctx context.Context, t *testing.T, tx pgx.Tx, run uuid.UUID) map[string]map[string]any {
	t.Helper()
	rows, err := tx.Query(ctx, `select subject, payload from domain_event where type = $1 and payload->>'run_id' = $2`,
		ops.TypeDifferenceFound, run.String())
	if err != nil {
		t.Fatalf("reading the difference events: %v", err)
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var (
			subject uuid.UUID
			payload map[string]any
		)
		if err := rows.Scan(&subject, &payload); err != nil {
			t.Fatalf("scanning a difference event: %v", err)
		}
		if payload["difference_id"] != subject.String() {
			t.Errorf("event subject %s and difference_id %v disagree", subject, payload["difference_id"])
		}
		out[subject.String()] = payload
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the difference events: %v", err)
	}
	return out
}

// entriesAndTransitions counts what detection must never change.
func entriesAndTransitions(ctx context.Context, t *testing.T, tx pgx.Tx) (entries, transitions int) {
	t.Helper()
	if err := tx.QueryRow(ctx, `
		select (select count(*) from cashback.entry), (select count(*) from cashback.entry_transition)`,
	).Scan(&entries, &transitions); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	return entries, transitions
}

// imports writes the statement and answers the run.
func imports(ctx context.Context, t *testing.T, store *ops.PGStore, p statementParties, raw string) uuid.UUID {
	t.Helper()
	run, err := store.ImportStatement(ctx, p.statement(raw))
	if err != nil {
		t.Fatalf("importing the statement: %v", err)
	}
	return run.ID
}

func TestDifferenceDetectionAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaPool(t)
	defer done()
	parties := seedStatementParties(ctx, t, tx)

	each(ctx, t, tx, "an omitted and a shorted transaction are both flagged with their deltas", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		shorted := reported(ctx, t, tx, parties, "B", 499, inAugust, uuid.Nil)
		reported(ctx, t, tx, parties, "C", 300, inAugust, uuid.Nil)
		run := imports(ctx, t, store, parties, `{"lines":[
			{"transaction_id":"B","paid":{"minor":450,"currency":"EUR"}},
			{"transaction_id":"C","paid":{"minor":300,"currency":"EUR"}},
			{"transaction_id":"X","paid":{"minor":120,"currency":"EUR"}}]}`)
		entriesBefore, transitionsBefore := entriesAndTransitions(ctx, t, tx)

		detection, err := store.DetectDifferences(ctx, run)
		if err != nil {
			t.Fatalf("DetectDifferences(): %v", err)
		}
		switch {
		case len(detection.Found) != 3:
			t.Fatalf("found %d, want 3: %s", len(detection.Found), describe(detection.Found))
		case detection.Recorded != 3:
			t.Errorf("recorded %d, want 3", detection.Recorded)
		case detection.DetectedAt.IsZero():
			t.Error("the detection has no time")
		}

		rows := differencesOf(ctx, t, tx, run)
		if len(rows) != 3 {
			t.Fatalf("%d rows in the queue, want 3", len(rows))
		}
		byKind := map[string]storedDifference{}
		for _, r := range rows {
			byKind[r.kind] = r
		}
		omitted := byKind["reported_not_paid"]
		if omitted.report == nil || omitted.expected == nil || *omitted.expected != 499 || omitted.actual != nil || omitted.line != nil {
			t.Errorf("the omitted transaction's row = %+v, want report named, expected 499, no actual, no line", omitted)
		}
		short := byKind["amount_mismatch"]
		if short.report == nil || *short.report != shorted || short.expected == nil || *short.expected != 499 || short.actual == nil || *short.actual != 450 {
			t.Errorf("the shorted transaction's row = %+v, want report B, expected 499, actual 450", short)
		}
		extra := byKind["paid_not_reported"]
		if extra.report != nil || extra.line == nil || *extra.line != "X" || extra.actual == nil || *extra.actual != 120 || extra.expected != nil {
			t.Errorf("the unmatched payment's row = %+v, want line X, actual 120, no report, no expected", extra)
		}
		for _, r := range rows {
			if r.currency != "EUR" || r.resolvedAt != nil {
				t.Errorf("row %+v: want EUR and open", r)
			}
		}

		// One event per row, as the contract lists it, each carrying the
		// money as a delta: paid less owed.
		events := foundEvents(ctx, t, tx, run)
		if len(events) != 3 {
			t.Fatalf("%d difference_found events, want one per row", len(events))
		}
		shortEvent := events[short.id.String()]
		delta, _ := shortEvent["delta"].(map[string]any)
		switch {
		case shortEvent == nil:
			t.Error("the shorted transaction's row was not announced")
		case shortEvent["kind"] != "amount_mismatch":
			t.Errorf("the shorted transaction's event says kind %v", shortEvent["kind"])
		case delta["minor"] != float64(-49) || delta["currency"] != "EUR":
			t.Errorf("the shorted transaction's delta = %v, want -49 EUR", shortEvent["delta"])
		}
		if omittedEvent := events[omitted.id.String()]; omittedEvent == nil {
			t.Error("the omitted transaction's row was not announced")
		} else if d, _ := omittedEvent["delta"].(map[string]any); d["minor"] != float64(-499) {
			t.Errorf("the omitted transaction's delta = %v, want -499 EUR", omittedEvent["delta"])
		}
		if extraEvent := events[extra.id.String()]; extraEvent == nil {
			t.Error("the unmatched payment's row was not announced")
		} else if d, _ := extraEvent["delta"].(map[string]any); d["minor"] != float64(120) {
			t.Errorf("the unmatched payment's delta = %v, want 120 EUR", extraEvent["delta"])
		}
		if entries, transitions := entriesAndTransitions(ctx, t, tx); entries != entriesBefore || transitions != transitionsBefore {
			t.Errorf("detection changed entries (%d -> %d) or transitions (%d -> %d); it must touch nothing but the queue",
				entriesBefore, entries, transitionsBefore, transitions)
		}
	})

	each(ctx, t, tx, "a repeat records nothing and announces nothing", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		run := imports(ctx, t, store, parties, `{"lines":[{"transaction_id":"X","paid":{"minor":120,"currency":"EUR"}}]}`)
		if _, err := store.DetectDifferences(ctx, run); err != nil {
			t.Fatalf("the first pass: %v", err)
		}
		again, err := store.DetectDifferences(ctx, run)
		if err != nil {
			t.Fatalf("the repeat: %v", err)
		}
		if len(again.Found) != 2 || again.Recorded != 0 {
			t.Errorf("the repeat found %d and recorded %d, want 2 found and 0 recorded", len(again.Found), again.Recorded)
		}
		if rows := differencesOf(ctx, t, tx, run); len(rows) != 2 {
			t.Errorf("%d rows after a repeat, want 2", len(rows))
		}
		if n := len(foundEvents(ctx, t, tx, run)); n != 2 {
			t.Errorf("%d difference_found events after a repeat, want the first pass's 2", n)
		}
	})

	each(ctx, t, tx, "a difference an operator resolved is not raised again", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		report := reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		run := imports(ctx, t, store, parties, `{"lines":[{"transaction_id":"A","paid":{"minor":450,"currency":"EUR"}}]}`)
		if _, err := store.DetectDifferences(ctx, run); err != nil {
			t.Fatalf("the first pass: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			update cashback.reconciliation_difference
			   set resolved_by = $3, resolved_reason = 'network deducted a returns adjustment', resolution = 'explained', resolved_at = now()
			 where run_id = $1 and network_transaction_id = $2`, run, report, parties.operator.ID); err != nil {
			t.Fatalf("resolving the difference: %v", err)
		}
		again, err := store.DetectDifferences(ctx, run)
		if err != nil {
			t.Fatalf("the repeat: %v", err)
		}
		if again.Recorded != 0 {
			t.Errorf("the repeat recorded %d, want 0: the operator already decided this", again.Recorded)
		}
		rows := differencesOf(ctx, t, tx, run)
		if len(rows) != 1 || rows[0].resolvedAt == nil {
			t.Errorf("rows after the repeat = %+v, want the one resolved row", rows)
		}
	})

	each(ctx, t, tx, "the network's latest word is what a payment is compared to", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		root := reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		tip := reported(ctx, t, tx, parties, "A", 450, inAugust, root)
		matching := imports(ctx, t, store, parties, `{"lines":[{"transaction_id":"A","paid":{"minor":450,"currency":"EUR"}}]}`)
		if d, err := store.DetectDifferences(ctx, matching); err != nil || len(d.Found) != 0 {
			t.Errorf("a payment matching the network's latest word raised %d difference(s) (err %v)", len(d.Found), err)
		}

		september := parties.statement(`{"lines":[{"transaction_id":"A","paid":{"minor":499,"currency":"EUR"}}]}`)
		september.Period = ops.Period{Start: august.End, End: august.End.AddDate(0, 1, 0)}
		stale, err := store.ImportStatement(ctx, september)
		if err != nil {
			t.Fatalf("importing the second statement: %v", err)
		}
		d, err := store.DetectDifferences(ctx, stale.ID)
		if err != nil {
			t.Fatalf("DetectDifferences(): %v", err)
		}
		if len(d.Found) != 1 || d.Found[0].Kind != ops.AmountMismatch || d.Found[0].Report != tip {
			t.Fatalf("a payment of the superseded figure derived %s; want one mismatch naming the current report %s", describe(d.Found), tip)
		}
		if d.Found[0].Expected.Minor != 450 || d.Found[0].Actual.Minor != 499 {
			t.Errorf("mismatch expected %d actual %d, want 450 and 499", d.Found[0].Expected.Minor, d.Found[0].Actual.Minor)
		}
	})

	each(ctx, t, tx, "a run nobody imported cannot be detected", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		_, err := store.DetectDifferences(ctx, uuid.New())
		if !errors.Is(err, ops.ErrNoSuchRun) {
			t.Errorf("DetectDifferences() = %v, want one wrapping ErrNoSuchRun", err)
		}
	})
}
