package store_test

// SC-002: 100% of member credits trace to a stored network transaction and,
// where attributed, to a click - verified by a query returning zero orphans
// (T075).
//
// A query that returns zero proves nothing on its own, and here it proves
// less than usual: the schema makes every orphan shape unrepresentable, so a
// detector that looked at the wrong column would return zero for the wrong
// reason and go on doing it forever. Half this file therefore exists to
// DEFEAT the schema - the guard is disabled and the keys dropped inside a
// savepoint, an orphan of that exact shape is planted, and the detector is
// required to find it and say why. The savepoint rolls back, so nothing
// here outlives the case.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
)

// creditable seeds a member, an offer, a click and a report naming that
// click's reference, so an entry can rest on real evidence.
func creditable(ctx context.Context, t *testing.T, tx pgx.Tx) (member, click, report pgtype.UUID) {
	t.Helper()
	networkID, publisher, member, offer := world(ctx, t, tx)
	ref := "orphan-check-reference-" + tag(t)

	if err := tx.QueryRow(ctx, `
		insert into cashback.click
		    (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
		values ($1, $2, $3, '{"kind":"fixed"}'::jsonb, 6000) returning id`,
		ref, member, offer).Scan(&click); err != nil {
		t.Fatalf("seeding the click: %v", err)
	}
	report = reportNaming(ctx, t, tx, networkID, publisher, ref)
	return member, click, report
}

// reportNaming stores a report that carries the given click reference.
//
// Inserted with it rather than updated afterwards, because
// network_transaction is immutable (C-3): what a network said is evidence,
// and the schema refuses to let a test rewrite it any more than it would let
// the poller.
func reportNaming(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, publisher pgtype.UUID, ref string) pgtype.UUID {
	t.Helper()
	at := purchasedAt
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload)
		values ($1, $2, $3, $4, 'confirmed', 'confirmed', 4999, 499, 'EUR', $5, $6, $7, $8, $9)
		returning id`,
		networkID, publisher, "ORPHAN-"+tag(t), ref,
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"ORPHAN"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// credit writes one entry.
func credit(ctx context.Context, tx pgx.Tx, member, report, click pgtype.UUID) error {
	_, err := tx.Exec(ctx, `
		insert into cashback.entry
		    (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
		values ('apivo-de', $1, $2, $3, 'pending', 3000, 'EUR')`,
		member, report, click)
	return err
}

// orphans runs the detector.
func orphans(ctx context.Context, t *testing.T, q *store.Queries) []store.OrphanCreditsRow {
	t.Helper()
	found, err := q.OrphanCredits(ctx)
	if err != nil {
		t.Fatalf("OrphanCredits(): %v", err)
	}
	return found
}

// defeatTheSchema removes, for the rest of this savepoint, everything that
// stops an orphan being written. It is the only way to prove the detector can
// see one: the guards are doing their job, so the shapes it looks for cannot
// otherwise exist to be looked for.
//
// CALL IT FIRST, before the case's fixtures. Disabling a trigger and dropping
// a constraint take an ACCESS EXCLUSIVE lock on cashback.entry, which
// conflicts with every other transaction touching that table - and `go test`
// runs packages in parallel against ONE database, so the integration suites
// in the parent package are doing exactly that. Asked for while this
// savepoint already holds rows another transaction wants, the request
// completes a cycle and Postgres aborts one of the two. Asked for while this
// savepoint holds nothing, it can only wait - and waiting is what the timeout
// and the retry below are for.
func defeatTheSchema(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	lockEvidenceExclusively(ctx, t, tx)
	if _, err := tx.Exec(ctx, `alter table cashback.entry disable trigger entry_evidence_guard`); err != nil {
		t.Fatalf("disabling the evidence guard: %v", err)
	}
	for _, constraint := range []string{"entry_click_belongs_to_member", "entry_click_id_fkey"} {
		if _, err := tx.Exec(ctx,
			`alter table cashback.entry drop constraint if exists `+constraint); err != nil {
			t.Fatalf("dropping %s: %v", constraint, err)
		}
	}
}

// evidenceLockTimeout bounds each lock the helper below waits for.
//
// It is deliberately SHORTER than the server's deadlock_timeout, which is a
// second by default, and that relationship is the whole of the fix for the
// deadlocks this helper used to inflict on other packages (#419). Have the
// reasoning in front of you before raising it:
//
// The helper wants ACCESS EXCLUSIVE on two tables and Postgres grants them
// one at a time. Between the first and the second, this transaction holds a
// lock every parallel suite wants while waiting on a lock one of them holds.
// If that suite now touches the first table, that is a cycle, and Postgres
// resolves a cycle by aborting whichever side has waited deadlock_timeout.
// That side was almost never this one: the other suite's statement began
// waiting later, so its timer fired later, found the cycle, and aborted an
// insert that had nothing to do with orphans. Seven to nine of those per run
// of the suite, every run.
//
// Under deadlock_timeout, this transaction ALWAYS gives up first, before
// either timer fires. The attempt is rolled back, the other statement
// proceeds, and the retry below asks again. Nobody else is ever the victim.
// TestTheBoundStaysUnderTheDeadlockDetector holds the number to that.
const evidenceLockTimeout = "200ms"

// evidenceLockAttempts is how many times the helper asks before declaring
// the tables stuck rather than busy.
//
// The two constants together are the patience budget. lock_timeout applies
// to each lock separately, so an attempt lasts one bound if the first table
// is busy and two if the second is: 150 attempts is thirty seconds to a
// minute of waiting. That is the half-minute the previous six attempts at
// five seconds gave, spent in pieces small enough that no single wait
// outlives the deadlock detector. A run that exhausts it is not contended,
// it is stuck, and the failure says so.
const evidenceLockAttempts = 150

// lockEvidenceExclusively takes every lock the DDL below needs, in one
// statement, bounded and retried.
//
// BOTH tables, and that is the whole of it. entry_click_belongs_to_member is
// a foreign key INTO cashback.click, so dropping it takes an ACCESS EXCLUSIVE
// lock on the referenced table as well as on cashback.entry - a fact the
// statement does not mention and the planner does not announce. Locking only
// the entries left this transaction holding one table and reaching for the
// other, which is half a deadlock cycle waiting for any parallel suite that
// touches a click.
//
// One statement rather than two, because two would be the same hazard in
// miniature: between them this transaction would hold the first lock while
// asking for the second. One statement still takes the locks one at a time,
// which is why the bound above matters as much as the statement's shape.
//
// Bounded, because an unbounded wait for a lock a parallel suite holds is a
// hung test rather than a failing one - and bounded UNDER the deadlock
// detector, so that when the wait is half a cycle it is this side that lets
// go. Retried, because contention is not a verdict on the detector: the case
// asserts what the query sees, and taking the locks a moment later asserts
// exactly the same thing.
//
// EACH ATTEMPT INSIDE A SAVEPOINT, and without that the retry is a fiction.
// A statement that fails aborts its transaction, so a lock_timeout or a
// deadlock leaves every later command answering 25P02 - including the next
// attempt, which then fails as an unrecognised error rather than retrying.
// That is not a hypothetical: the first version of this loop shipped without
// the savepoint and turned a lock timeout into
// "current transaction is aborted, commands ignored until end of transaction
// block". Rolling back to the savepoint clears the abort; releasing it on
// success keeps the locks, because a released savepoint keeps its effects.
//
// pgx spells a nested Begin as a savepoint, which is why this reads as a
// transaction inside a transaction.
//
// It must be called before ANY fixture in the case. Holding nothing, this
// transaction can only wait; holding a row somebody else wants, it can
// deadlock - which is what happened when one case seeded its report first.
func lockEvidenceExclusively(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	// set_config rather than SET LOCAL, because SET LOCAL takes no parameter
	// and the bound is a Go constant, not a string spliced into SQL.
	if _, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true)`, evidenceLockTimeout); err != nil {
		t.Fatalf("bounding the lock wait: %v", err)
	}
	var err error
	for attempt := 1; attempt <= evidenceLockAttempts; attempt++ {
		var attempted pgx.Tx
		if attempted, err = tx.Begin(ctx); err != nil {
			t.Fatalf("opening the savepoint for attempt %d: %v", attempt, err)
		}
		_, err = attempted.Exec(ctx,
			`lock table cashback.entry, cashback.click in access exclusive mode`)
		if err == nil {
			// Released, not rolled back: the locks stay with the outer
			// transaction, which is the whole point of taking them.
			if err = attempted.Commit(ctx); err != nil {
				t.Fatalf("releasing the savepoint that took the locks: %v", err)
			}
			return
		}
		// Back to before the attempt, which is what makes the transaction
		// usable for the next one.
		if rollback := attempted.Rollback(ctx); rollback != nil {
			t.Fatalf("rolling back attempt %d: %v (after %v)", attempt, rollback, err)
		}
		// DeadlockDetected stays although the bound should make it
		// unreachable here: on a server whose deadlock_timeout has been
		// lowered under the bound this side can be the victim again, and
		// being the victim is still not a verdict on the detector.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != pgerrcode.LockNotAvailable && pgErr.Code != pgerrcode.DeadlockDetected) {
			t.Fatalf("locking the evidence: %v", err)
		}
	}
	t.Fatalf("the evidence tables stayed locked by another suite through %d attempts bounded at %s each: %v",
		evidenceLockAttempts, evidenceLockTimeout, err)
}

// onlyOrphan requires exactly one orphan and returns its reason.
func onlyOrphan(t *testing.T, found []store.OrphanCreditsRow) string {
	t.Helper()
	if len(found) != 1 {
		t.Fatalf("found %d orphan(s), want the one just planted", len(found))
	}
	return found[0].Reason
}

func TestTheOrphanCreditQueryAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaTx(t)
	defer done()

	// SC-002 itself.
	each(ctx, t, tx, "a fully evidenced credit is not an orphan", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		member, click, report := creditable(ctx, t, tx)
		if err := credit(ctx, tx, member, report, click); err != nil {
			t.Fatalf("a lawful credit was rejected: %v", err)
		}
		if found := orphans(ctx, t, q); len(found) != 0 {
			t.Errorf("a fully evidenced credit was reported as an orphan: %+v", found)
		}
	})

	// The lawful null click. An operator attributed this by hand because the
	// network named no reference, so there is no click to cite - and a
	// detector that flagged it would put every hand-attributed credit in
	// front of somebody as a defect.
	each(ctx, t, tx, "an operator-attributed credit is not an orphan", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		networkID, publisher, member, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		if err := credit(ctx, tx, member, report, pgtype.UUID{}); err != nil {
			t.Fatalf("an operator-attributed credit was rejected: %v", err)
		}
		if found := orphans(ctx, t, q); len(found) != 0 {
			t.Errorf("a credit the network gave no reference for was reported as an orphan: %+v", found)
		}
	})

	// From here the schema is defeated on purpose. A detector looking at the
	// wrong column would return zero in these cases exactly as it does in the
	// two above, and nothing else could tell the two apart.
	each(ctx, t, tx, "a credit dropping the click the network named is found", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		defeatTheSchema(ctx, t, tx)
		member, _, report := creditable(ctx, t, tx)

		if err := credit(ctx, tx, member, report, pgtype.UUID{}); err != nil {
			t.Fatalf("planting the orphan: %v", err)
		}
		if reason := onlyOrphan(t, orphans(ctx, t, q)); reason != "the network reported a click reference and the credit cites no click" {
			t.Errorf("reason = %q, which does not say what is wrong with it", reason)
		}
	})

	each(ctx, t, tx, "a credit citing another member's click is found", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		defeatTheSchema(ctx, t, tx)
		_, click, _ := creditable(ctx, t, tx)
		other, _, othersReport := creditable(ctx, t, tx)

		// The other member's own report and credit, resting on the FIRST
		// member's click: real evidence, belonging to somebody else.
		if err := credit(ctx, tx, other, othersReport, click); err != nil {
			t.Fatalf("planting the orphan: %v", err)
		}
		if reason := onlyOrphan(t, orphans(ctx, t, q)); reason != "the click it cites belongs to another member" {
			t.Errorf("reason = %q, which does not say what is wrong with it", reason)
		}
	})

	each(ctx, t, tx, "a credit citing a click that does not exist is found", func(t *testing.T, tx pgx.Tx, q *store.Queries) {
		// Defeated FIRST, before any fixture. Seeding the report first left
		// this transaction holding rows another suite wanted while it reached
		// for the table locks, which is a deadlock rather than a wait.
		defeatTheSchema(ctx, t, tx)
		networkID, publisher, member, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)

		if err := credit(ctx, tx, member, report, pgtype.UUID{Bytes: [16]byte{7, 7, 7}, Valid: true}); err != nil {
			t.Fatalf("planting the orphan: %v", err)
		}
		if reason := onlyOrphan(t, orphans(ctx, t, q)); reason != "the click it cites does not exist" {
			t.Errorf("reason = %q, which does not say what is wrong with it", reason)
		}
	})
}
