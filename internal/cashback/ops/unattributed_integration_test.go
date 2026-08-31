package ops_test

// The operator queue against the real, migrated schema (T059).
//
// The unit tests above fake the store, so they can say what the endpoint
// promises but nothing about whether the database agrees. Three things only
// the schema can answer, and each of them is money: that a dismissal and
// the event announcing it commit together or not at all; that two operators
// deciding the same row end with one recorded reason rather than the last
// writer's; and that "no longer open work" is derived from evidence that
// has moved, not from a flag somebody remembered to set.
//
// Every case seeds its own network, so two of them can name the same
// external transaction without meeting.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// queued is one seeded piece of work: the queue row, the report it names,
// and an operator who may close it.
type queued struct {
	row      uuid.UUID
	report   uuid.UUID
	operator ops.Operator
}

// suffix is a short random string, so a fixture cannot collide with one
// another case left behind - these rows are never deleted (0024), so every
// run adds to the same table.
func suffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// seed writes a network, a publisher account, an operator, one report the
// network attached no click reference to, and the queue row recording that
// it went unattributed.
func seed(ctx context.Context, t *testing.T, tx pgx.Tx) queued {
	t.Helper()
	tag := suffix(t)
	networkID := "opsfix_" + tag

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Ops Fixture Network', 'clickref', 31, 360, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var accountID uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.opsfix.credential', true)
		returning id`, networkID).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	var operatorID uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Ops Person', 'operator') returning id`,
		"ops-"+tag+"@example.test").Scan(&operatorID); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}

	report := storeReport(ctx, t, tx, networkID, accountID, "FIX-1001", uuid.Nil)

	var row uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.unattributed_transaction (network_transaction_id)
		values ($1) returning id`, report).Scan(&row); err != nil {
		t.Fatalf("seeding the queue row: %v", err)
	}
	return queued{
		row:      row,
		report:   report,
		operator: ops.Operator{ID: operatorID, Email: "ops-" + tag + "@example.test", DisplayName: "Ops Person"},
	}
}

// storeReport writes one evidence row with no click reference - the kind
// FR-034 queues - and answers its id. A non-nil supersedes makes it a
// successor, which needs a reported fact to differ or the digest makes it
// the same report.
func storeReport(ctx context.Context, t *testing.T, tx pgx.Tx, networkID string, accountID uuid.UUID, externalID string, supersedes uuid.UUID) uuid.UUID {
	t.Helper()
	at := time.Date(2026, time.August, 3, 9, 15, 0, 0, time.UTC)
	status := "pending"
	if supersedes != uuid.Nil {
		status = "confirmed"
	}
	var (
		id            uuid.UUID
		supersedesArg any
	)
	if supersedes != uuid.Nil {
		supersedesArg = supersedes
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end,
			raw_payload, supersedes_id)
		values ($1, $2, $3, null, $4, $4, 4999, 499, 'EUR', $5, $6, $7, $8, $9, $10)
		returning id`,
		networkID, accountID, externalID, status,
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour),
		[]byte(`{"transaction_id":"`+externalID+`","status":"`+status+`"}`), supersedesArg,
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// openRow finds one queue row in the store's own listing, walking pages so
// the assertion does not quietly depend on how much other work is queued.
func openRow(ctx context.Context, t *testing.T, store *ops.PGStore, id uuid.UUID) (networks.OpenReport, bool) {
	t.Helper()
	var after networks.After
	for range 100 {
		page, err := store.Open(ctx, after, 100)
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		for _, row := range page {
			if row.ID == id {
				return row, true
			}
		}
		if len(page) < 100 {
			return networks.OpenReport{}, false
		}
		after = page[len(page)-1].After()
	}
	t.Fatal("the queue never ended; a page is not advancing")
	return networks.OpenReport{}, false
}

// resolution reads back what the row actually holds.
func resolution(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID) (by *uuid.UUID, reason *string, at *time.Time) {
	t.Helper()
	if err := tx.QueryRow(ctx,
		`select resolved_by, resolved_reason, resolved_at from cashback.unattributed_transaction where id = $1`,
		id).Scan(&by, &reason, &at); err != nil {
		t.Fatalf("reading the resolution: %v", err)
	}
	return by, reason, at
}

// each runs one case over a savepoint of its own - pgx spells a nested Begin
// as one - with a store built on it, and rolls it back afterwards. That is
// what keeps a run from adding to a table nothing may delete from (0024),
// and it keeps a case that provokes a refusal from poisoning the next.
func each(ctx context.Context, t *testing.T, tx pgx.Tx, name string, scenario func(t *testing.T, tx pgx.Tx, store *ops.PGStore)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		sub, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer func() { _ = sub.Rollback(ctx) }()
		// A pgx.Tx is a Beginner too: the store's own transaction becomes a
		// nested savepoint, so its commit-or-nothing behaviour is exactly
		// the behaviour under test rather than something the harness has
		// replaced.
		store, err := ops.NewPGStore(sub)
		if err != nil {
			t.Fatalf("building the store: %v", err)
		}
		scenario(t, sub, store)
	})
}

func TestTheOperatorQueueAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the operator queue")
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
	// Rolled back, so a run leaves this shared table exactly as it found
	// it. The queue rows are never deletable (0024), so a case that
	// committed would be a case every later run pages past.
	defer func() { _ = tx.Rollback(ctx) }()

	each(ctx, t, tx, "a recorded observation is work an operator can see", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)

		row, found := openRow(ctx, t, store, work.row)
		if !found {
			t.Fatal("the seeded row is not in the queue")
		}
		if row.Report != work.report {
			t.Errorf("the row names report %s, want %s", row.Report, work.report)
		}
		// The network attached no reference, so an entry could lawfully
		// omit its click: this is the kind an operator may still attribute
		// once the earnings module can write one.
		if !row.Attributable {
			t.Error("a report with no click reference is reported as unattributable")
		}
		if row.Sale.Minor != 4999 || row.Sale.Currency != "EUR" {
			t.Errorf("sale = %+v, want 4999 EUR", row.Sale)
		}
	})

	each(ctx, t, tx, "a dismissal records who, why and when, and says so in the stream", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)
		const why = "the network confirmed this is a staff test order"

		dismissed, err := store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: why})
		if err != nil {
			t.Fatalf("Dismiss(): %v", err)
		}

		by, reason, at := resolution(ctx, t, tx, work.row)
		switch {
		case by == nil || reason == nil || at == nil:
			t.Fatalf("the row holds a half-recorded resolution: by=%v reason=%v at=%v", by, reason, at)
		case *by != work.operator.ID:
			t.Errorf("resolved_by = %s, want the acting operator %s", *by, work.operator.ID)
		case *reason != why:
			t.Errorf("resolved_reason = %q, want %q", *reason, why)
		}
		// The instant is the row's own, not one this process chose.
		if !dismissed.ResolvedAt.Equal(*at) {
			t.Errorf("reported resolved_at %s, want the row's %s", dismissed.ResolvedAt, *at)
		}

		var (
			producer string
			subject  uuid.UUID
			payload  []byte
		)
		if err := tx.QueryRow(ctx, `
			select producer, subject, payload from domain_event
			 where type = $1 and idempotency_key = $2`,
			ops.TypeUnattributedDismissed, ops.TypeUnattributedDismissed+":"+work.row.String(),
		).Scan(&producer, &subject, &payload); err != nil {
			t.Fatalf("the dismissal published no event: %v", err)
		}
		if producer != "cashback" {
			t.Errorf("producer = %q, want %q", producer, "cashback")
		}
		if subject != work.row {
			t.Errorf("subject = %s, want the queue row %s", subject, work.row)
		}
		var body struct {
			UnattributedID       string `json:"unattributed_id"`
			NetworkTransactionID string `json:"network_transaction_id"`
			ResolvedBy           string `json:"resolved_by"`
			Reason               string `json:"reason"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatalf("the event payload does not parse: %v (%s)", err, payload)
		}
		// FR-061 in the stream: a consumer learns who decided and why
		// without calling back into cashback to ask.
		if body.ResolvedBy != work.operator.ID.String() || body.Reason != why {
			t.Errorf("payload = %+v, want the acting operator and the recorded reason", body)
		}
		if body.NetworkTransactionID != work.report.String() {
			t.Errorf("payload names report %s, want %s", body.NetworkTransactionID, work.report)
		}

		if _, found := openRow(ctx, t, store, work.row); found {
			t.Error("a dismissed row is still listed as work")
		}
	})

	each(ctx, t, tx, "a second operator is told the row was taken, and the first reason stands", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)
		const first = "duplicate of TX-9"

		if _, err := store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: first}); err != nil {
			t.Fatalf("the first dismissal: %v", err)
		}

		_, err := store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: "no, actually something else"})
		var closed ops.ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("the second dismissal = %v, want a ClosedError", err)
		}
		if !closed.Why.Resolved || closed.Why.Reason != first {
			t.Errorf("the refusal says %+v, want it to name the resolution that stands", closed.Why)
		}
		// The point of the refusal: 0024 forbids erasing a resolution, and
		// the guard in the statement means the second operator never got
		// close enough to try.
		if _, reason, _ := resolution(ctx, t, tx, work.row); reason == nil || *reason != first {
			t.Errorf("the recorded reason is %v, want the first operator's %q", reason, first)
		}
	})

	each(ctx, t, tx, "an id that names no row is a mistake, not a race", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)

		_, err := store.Dismiss(ctx, ops.Dismissal{ID: uuid.New(), Operator: work.operator, Reason: "whatever"})
		if !errors.Is(err, ops.ErrNoSuchQueueRow) {
			t.Fatalf("Dismiss() of an unknown id = %v, want %v", err, ops.ErrNoSuchQueueRow)
		}
	})

	each(ctx, t, tx, "a superseded report is no longer work, and the answer says which", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)

		// The network says something new about the same transaction. The
		// queue row is untouched - nothing is edited (C-3) - and stops
		// being work because the report it names is no longer the tip.
		var networkID string
		var accountID uuid.UUID
		if err := tx.QueryRow(ctx,
			`select network_id, network_account_id from cashback.network_transaction where id = $1`,
			work.report).Scan(&networkID, &accountID); err != nil {
			t.Fatalf("reading the seeded report: %v", err)
		}
		storeReport(ctx, t, tx, networkID, accountID, "FIX-1001", work.report)

		if _, found := openRow(ctx, t, store, work.row); found {
			t.Error("a superseded row is still listed as work")
		}

		_, err := store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: "too late"})
		var closed ops.ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("Dismiss() = %v, want a ClosedError", err)
		}
		if !closed.Why.Superseded {
			t.Errorf("the refusal says %+v, want it to name the superseding report", closed.Why)
		}
		if by, _, _ := resolution(ctx, t, tx, work.row); by != nil {
			t.Errorf("the refused dismissal still wrote resolved_by = %s", *by)
		}
	})

	// The one that justifies the transaction. If the event cannot be
	// appended, the resolution must not exist either - otherwise a decision
	// is recorded that no consumer will ever hear about, and FR-061's audit
	// trail has a hole exactly where somebody's money was.
	each(ctx, t, tx, "the resolution and its event commit together or not at all", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		work := seed(ctx, t, tx)

		// Claim the idempotency key first, so the append inside the
		// dismissal collides with it.
		if _, err := tx.Exec(ctx, `
			insert into domain_event (type, payload, version, producer, subject, idempotency_key)
			values ($1, '{}'::jsonb, 1, 'cashback', $2, $3)`,
			ops.TypeUnattributedDismissed, work.row, ops.TypeUnattributedDismissed+":"+work.row.String(),
		); err != nil {
			t.Fatalf("claiming the idempotency key: %v", err)
		}

		if _, err := store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: "will not commit"}); err == nil {
			t.Fatal("Dismiss() succeeded although its event could not be appended")
		}

		by, reason, at := resolution(ctx, t, tx, work.row)
		if by != nil || reason != nil || at != nil {
			t.Errorf("the row holds a resolution the failed transaction should have rolled back: by=%v reason=%v at=%v", by, reason, at)
		}
		if _, found := openRow(ctx, t, store, work.row); !found {
			t.Error("the row is no longer work although nothing was recorded about it")
		}
	})
}

// TestTwoOperatorsDecidingAtOnce is the one case that cannot run inside a
// transaction: two decisions racing need two connections, and a savepoint
// harness would serialise exactly the thing under test.
//
// It therefore commits, and leaves one queue row behind - which is why it
// asserts only about the row it seeded.
func TestTwoOperatorsDecidingAtOnce(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise concurrent operator decisions")
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

	store, err := ops.NewPGStore(pool)
	if err != nil {
		t.Fatalf("building the store: %v", err)
	}

	// Seeded through a committed transaction, because the racing
	// dismissals run on connections of their own and must be able to see
	// the row.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	work := seed(ctx, t, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the fixture: %v", err)
	}

	reasons := [2]string{"first operator: a staff test order", "second operator: a chargeback"}
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		errs  [2]error
	)
	start.Add(1)
	done.Add(len(reasons))
	for i, why := range reasons {
		go func() {
			defer done.Done()
			start.Wait()
			_, errs[i] = store.Dismiss(ctx, ops.Dismissal{ID: work.row, Operator: work.operator, Reason: why})
		}()
	}
	start.Done()
	done.Wait()

	var won int
	var winner string
	for i, err := range errs {
		if err == nil {
			won, winner = won+1, reasons[i]
			continue
		}
		var closed ops.ClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("the losing dismissal = %v, want a ClosedError", err)
		}
		if !closed.Why.Resolved {
			t.Errorf("the refusal says %+v, want it to name the resolution that won", closed.Why)
		}
	}
	if won != 1 {
		t.Fatalf("%d of 2 concurrent dismissals succeeded, want exactly 1", won)
	}

	read, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = read.Rollback(ctx) }()
	if _, reason, _ := resolution(ctx, t, read, work.row); reason == nil || *reason != winner {
		t.Errorf("the recorded reason is %v, want the winner's %q", reason, winner)
	}
}
