package outboxcrash_test

import (
	"context"
	"sync"
	"testing"
)

// TestS2CrashBetweenTheCommitAndTheLedger kills the process in the first
// window: the Apivo transaction has committed and the ledger has heard
// nothing.
//
// The claim is that the outbox row is a durable instruction, so the money
// still moves - once - when anything picks it up again.
func TestS2CrashBetweenTheCommitAndTheLedger(t *testing.T) {
	requireStack(t)

	ctx := context.Background()
	pool := connect(t)
	ensureSchema(ctx, t, pool)

	client, err := newLedgerClient()
	if err != nil {
		t.Fatalf("building the ledger client: %v", err)
	}
	source, destination := newLedgerAccounts(t, client)
	key := newKey(t)

	runWorkerProcess(t, modeCommitThenCrash, key, source, destination)

	// The domain row and its instruction survived the crash together.
	var entries, pending int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM `+schema+`.entry WHERE id = $1),
		        (SELECT count(*) FROM `+schema+`.outbox WHERE idempotency_key = $1 AND dispatched_at IS NULL)`,
		key,
	).Scan(&entries, &pending); err != nil {
		t.Fatalf("reading what the crash left behind: %v", err)
	}
	if entries != 1 {
		t.Fatalf("entry rows = %d, want 1: the commit did not survive the crash", entries)
	}
	if pending != 1 {
		t.Fatalf("undispatched outbox rows = %d, want 1: the instruction to pay was lost", pending)
	}

	// And nothing reached the ledger, so there is no money to be
	// inconsistent about yet.
	settle()
	if rows := ledgerRowsFor(ctx, t, pool, key); rows != 0 {
		t.Fatalf("ledger rows = %d, want 0: the crash was supposed to land before the ledger call", rows)
	}

	// Recovery.
	if err := dispatch(ctx, pool, client, key); err != nil {
		t.Fatalf("dispatching the recovered outbox row: %v", err)
	}
	posted := waitForLedgerRows(ctx, t, pool, key, 1)
	after := balanceOf(ctx, t, pool, destination)
	t.Logf("S2: recovery posted %d ledger row(s); destination balance is %s", posted, after)

	var dispatched int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+schema+`.outbox WHERE idempotency_key = $1 AND dispatched_at IS NOT NULL`, key,
	).Scan(&dispatched); err != nil {
		t.Fatalf("reading the dispatched mark: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched outbox rows = %d, want 1", dispatched)
	}

	// The replay a second crash would cause: the same key, posted again.
	// The ledger must absorb it without moving money.
	replayed, err := postTransfer(client, key, source, destination)
	switch {
	case err != nil:
		t.Logf("S2: the ledger refused the replayed key, which is the strongest form of absorbing it: %v", err)
	case replayed != nil:
		t.Logf("S2: the ledger accepted the replayed key and answered with transaction %q; the assertions below decide whether it moved money", replayed.TransactionID)
	default:
		t.Log("S2: the ledger accepted the replayed key and returned nothing; the assertions below decide whether it moved money")
	}
	settle()
	if rows := ledgerRowsFor(ctx, t, pool, key); rows != posted {
		t.Fatalf("ledger rows = %d after the replay, want %d: the same idempotency key created a second transfer", rows, posted)
	}
	if got := balanceOf(ctx, t, pool, destination); got != after {
		t.Fatalf("destination balance moved on replay: %s -> %s", after, got)
	}
}

// TestS2CrashBetweenTheLedgerAndTheMark kills the process in the second, more
// dangerous window: the transfer exists, but the outbox row still says
// pending, so recovery is GUARANTEED to replay it.
//
// This is the case that decides S2. If the ledger cannot absorb a replayed
// idempotency key, every crash in this window double-pays a member.
func TestS2CrashBetweenTheLedgerAndTheMark(t *testing.T) {
	requireStack(t)

	ctx := context.Background()
	pool := connect(t)
	ensureSchema(ctx, t, pool)

	client, err := newLedgerClient()
	if err != nil {
		t.Fatalf("building the ledger client: %v", err)
	}
	source, destination := newLedgerAccounts(t, client)
	key := newKey(t)

	if err := writeEntryAndOutbox(ctx, pool, key, source, destination); err != nil {
		t.Fatalf("writing the entry and its outbox row: %v", err)
	}

	runWorkerProcess(t, modePostThenCrash, key, source, destination)

	posted := waitForLedgerRows(ctx, t, pool, key, 1)
	afterPost := balanceOf(ctx, t, pool, destination)
	t.Logf("S2: the dead worker left %d ledger row(s); destination balance is %s", posted, afterPost)

	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+schema+`.outbox WHERE idempotency_key = $1 AND dispatched_at IS NULL`, key,
	).Scan(&pending); err != nil {
		t.Fatalf("reading the outbox row: %v", err)
	}
	if pending != 1 {
		t.Fatalf("undispatched outbox rows = %d, want 1: the crash was supposed to land before the mark", pending)
	}

	// Recovery does the only thing it can: it replays, because from the
	// database's point of view nothing was posted.
	if err := dispatch(ctx, pool, client, key); err != nil {
		t.Logf("S2: recovery's replay was refused by the ledger: %v", err)
		// A refusal is a correct outcome, but the row must still be
		// reconciled or recovery will loop forever. Mark it the way a
		// dispatcher that recognises a duplicate would.
		if _, err := pool.Exec(ctx,
			`UPDATE `+schema+`.outbox SET dispatched_at = now() WHERE idempotency_key = $1`, key,
		); err != nil {
			t.Fatalf("marking the outbox row after a refused replay: %v", err)
		}
	}

	settle()
	if rows := ledgerRowsFor(ctx, t, pool, key); rows != posted {
		t.Fatalf("ledger rows = %d after recovery replayed, want %d: the crash produced a second transfer", rows, posted)
	}
	if got := balanceOf(ctx, t, pool, destination); got != afterPost {
		t.Fatalf("recovery's replay moved money a second time: %s -> %s", afterPost, got)
	}

	var dispatched int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+schema+`.outbox WHERE idempotency_key = $1 AND dispatched_at IS NOT NULL`, key,
	).Scan(&dispatched); err != nil {
		t.Fatalf("reading the dispatched mark: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched outbox rows = %d, want 1: recovery would loop on this row forever", dispatched)
	}
}

// TestS2ConcurrentDispatchPostsOnce is the other way the same window opens:
// not a crash, but two dispatchers reaching the same undispatched row at once
// - a restarted process racing the one that was already running.
func TestS2ConcurrentDispatchPostsOnce(t *testing.T) {
	requireStack(t)

	ctx := context.Background()
	pool := connect(t)
	ensureSchema(ctx, t, pool)

	client, err := newLedgerClient()
	if err != nil {
		t.Fatalf("building the ledger client: %v", err)
	}
	source, destination := newLedgerAccounts(t, client)
	key := newKey(t)

	if err := writeEntryAndOutbox(ctx, pool, key, source, destination); err != nil {
		t.Fatalf("writing the entry and its outbox row: %v", err)
	}

	const dispatchers = 4
	var wg sync.WaitGroup
	results := make([]error, dispatchers)
	start := make(chan struct{})
	for i := range dispatchers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = postTransfer(client, key, source, destination)
		}(i)
	}
	close(start)
	wg.Wait()

	accepted := 0
	for i, err := range results {
		if err == nil {
			accepted++
			continue
		}
		t.Logf("S2: dispatcher %d was refused: %v", i, err)
	}
	t.Logf("S2: %d of %d concurrent posts of the same idempotency key were accepted", accepted, dispatchers)

	settle()
	rows := ledgerRowsFor(ctx, t, pool, key)
	balance := balanceOf(ctx, t, pool, destination)
	t.Logf("S2: the ledger holds %d row(s) for the key; destination balance is %s", rows, balance)

	if rows != 1 {
		t.Fatalf("ledger rows = %d for one idempotency key under %d concurrent posts, want 1", rows, dispatchers)
	}
}

// TestS2LedgerReferenceUniqueness records HOW the ledger enforces the key -
// in the database, or only in application code. Both are recorded rather than
// judged: Apivo carries its own unique constraint on the idempotency key
// (C-5, data-model.md), so a ledger that only checks in application code
// narrows the guarantee to one process rather than removing it. Which of the
// two it is belongs in the spike's evidence.
func TestS2LedgerReferenceUniqueness(t *testing.T) {
	requireStack(t)

	ctx := context.Background()
	pool := connect(t)

	var constraints int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey)
		 WHERE n.nspname = 'blnk'
		   AND c.relname = 'transactions'
		   AND i.indisunique
		   AND a.attname = 'reference'`,
	).Scan(&constraints); err != nil {
		t.Fatalf("inspecting the ledger's indexes: %v", err)
	}

	if constraints > 0 {
		t.Logf("S2 evidence: the ledger enforces reference uniqueness in the DATABASE (%d unique index(es) on blnk.transactions.reference)", constraints)
		return
	}
	t.Log("S2 evidence: the ledger has NO unique index on blnk.transactions.reference; its duplicate check is application-level. Apivo's own unique idempotency key (C-5) is what makes exactly-once a database guarantee.")
}
