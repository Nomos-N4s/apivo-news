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
	lockEntriesExclusively(ctx, t, tx)
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

// lockEntriesExclusively takes the lock the DDL below needs, bounded and
// retried.
//
// Bounded, because an unbounded wait for a lock a parallel suite holds is a
// hung test rather than a failing one. Retried, because contention is not a
// verdict on the detector: the case asserts what the query sees, and taking
// the lock a moment later asserts exactly the same thing.
func lockEntriesExclusively(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
		t.Fatalf("bounding the lock wait: %v", err)
	}
	var err error
	for attempt := 1; attempt <= 6; attempt++ {
		if _, err = tx.Exec(ctx, `lock table cashback.entry in access exclusive mode`); err == nil {
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != pgerrcode.LockNotAvailable && pgErr.Code != pgerrcode.DeadlockDetected) {
			t.Fatalf("locking the entries: %v", err)
		}
	}
	t.Fatalf("the entry table stayed locked by another suite for half a minute: %v", err)
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
		networkID, publisher, member, _ := world(ctx, t, tx)
		report := reportAt(ctx, t, tx, networkID, publisher)
		defeatTheSchema(ctx, t, tx)

		if err := credit(ctx, tx, member, report, pgtype.UUID{Bytes: [16]byte{7, 7, 7}, Valid: true}); err != nil {
			t.Fatalf("planting the orphan: %v", err)
		}
		if reason := onlyOrphan(t, orphans(ctx, t, q)); reason != "the click it cites does not exist" {
			t.Errorf("reason = %q, which does not say what is wrong with it", reason)
		}
	})
}
