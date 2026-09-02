package ops_test

// Statement import against the real, migrated schema (T110, C-3, 0028).
//
// Three things only the database can answer, and each is money: that the
// run and the event announcing it commit together or not at all; that the
// same statement imported twice - by a retry, or by two operators - is one
// run, announced once; and that a corrected statement is a new run rather
// than a rewrite of an immutable one.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// statementParties is who a statement is between: the network, the
// publisher account it pays, and the operator importing it.
type statementParties struct {
	network  string
	account  uuid.UUID
	operator ops.Operator
}

// statement frames raw as this account's statement for August.
func (p statementParties) statement(raw string) ops.Statement {
	return ops.Statement{Account: p.account, Period: august, Raw: json.RawMessage(raw), Operator: p.operator}
}

// seedStatementParties writes a network, a publisher account and an
// operator, each suffixed so two cases never meet.
func seedStatementParties(ctx context.Context, t *testing.T, tx pgx.Tx) statementParties {
	t.Helper()
	tag := suffix(t)
	networkID := "recfix_" + tag
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Reconciliation Fixture Network', 'clickref', 31, 360, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var account uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.recfix.credential', true)
		returning id`, networkID).Scan(&account); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	var operatorID uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Ops Person', 'operator') returning id`,
		"rec-"+tag+"@example.test").Scan(&operatorID); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	return statementParties{
		network:  networkID,
		account:  account,
		operator: ops.Operator{ID: operatorID, Email: "rec-" + tag + "@example.test", DisplayName: "Ops Person"},
	}
}

// importEvents reads every import announcement for one run, oldest first.
func importEvents(ctx context.Context, t *testing.T, tx pgx.Tx, run uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := tx.Query(ctx,
		`select payload from domain_event where type = $1 and subject = $2 order by occurred_at`,
		ops.TypeStatementImported, run.String())
	if err != nil {
		t.Fatalf("reading the import events: %v", err)
	}
	defer rows.Close()
	var payloads []map[string]any
	for rows.Next() {
		var payload map[string]any
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scanning an import event: %v", err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the import events: %v", err)
	}
	return payloads
}

// runsFor counts the runs an account has.
func runsFor(ctx context.Context, t *testing.T, tx pgx.Tx, account uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.reconciliation_run where network_account_id = $1`, account).Scan(&n); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	return n
}

// announcementsOfImports counts every import event there is, so a case can
// prove a refused import announced nothing without knowing a run id.
func announcementsOfImports(ctx context.Context, t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from domain_event where type = $1`, ops.TypeStatementImported).Scan(&n); err != nil {
		t.Fatalf("counting import events: %v", err)
	}
	return n
}

func TestStatementImportAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise statement import")
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
		t.Fatalf("beginning: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	parties := seedStatementParties(ctx, t, tx)

	each(ctx, t, tx, "a statement becomes an immutable run and is announced", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		imported, err := store.ImportStatement(ctx, parties.statement(twoLines))
		if err != nil {
			t.Fatalf("ImportStatement(): %v", err)
		}
		switch {
		case imported.AlreadyImported:
			t.Error("a first import reports itself as a repeat")
		case imported.ID == uuid.Nil:
			t.Error("the run has no id")
		case imported.ImportedBy != parties.operator.ID:
			t.Errorf("imported_by = %s, want the operator %s", imported.ImportedBy, parties.operator.ID)
		case imported.Network != networks.NetworkID(parties.network):
			t.Errorf("network = %s, want %s", imported.Network, parties.network)
		case imported.Lines != 2:
			t.Errorf("lines = %d, want 2", imported.Lines)
		case imported.ImportedAt.IsZero():
			t.Error("the run has no imported_at")
		case len(imported.Digest) != 32:
			t.Errorf("digest = %q, want an md5 hex", imported.Digest)
		case !imported.Period.Start.Equal(august.Start) || !imported.Period.End.Equal(august.End):
			t.Errorf("period = %+v, want %+v", imported.Period, august)
		}

		var (
			account, importedBy uuid.UUID
			verbatim            bool
			digest              string
		)
		if err := tx.QueryRow(ctx, `
			select network_account_id, imported_by, raw_statement = $2::jsonb, statement_digest
			  from cashback.reconciliation_run where id = $1`, imported.ID, []byte(twoLines),
		).Scan(&account, &importedBy, &verbatim, &digest); err != nil {
			t.Fatalf("reading the run back: %v", err)
		}
		switch {
		case account != parties.account:
			t.Errorf("the run is for account %s, want %s", account, parties.account)
		case importedBy != parties.operator.ID:
			t.Errorf("the run was imported by %s, want %s", importedBy, parties.operator.ID)
		case !verbatim:
			t.Error("the stored statement is not the one supplied")
		case digest != imported.Digest:
			t.Errorf("the run's digest is %s, the import reported %s", digest, imported.Digest)
		}

		events := importEvents(ctx, t, tx, imported.ID)
		if len(events) != 1 {
			t.Fatalf("%d import events for the run, want exactly one", len(events))
		}
		payload := events[0]
		switch {
		case payload["run_id"] != imported.ID.String():
			t.Errorf("the event names run %v, want %s", payload["run_id"], imported.ID)
		case payload["network_account_id"] != parties.account.String():
			t.Errorf("the event names account %v, want %s", payload["network_account_id"], parties.account)
		case payload["network_id"] != parties.network:
			t.Errorf("the event names network %v, want %s", payload["network_id"], parties.network)
		case payload["imported_by"] != parties.operator.ID.String():
			t.Errorf("the event names importer %v, want %s (FR-061)", payload["imported_by"], parties.operator.ID)
		case payload["lines"] != float64(2):
			t.Errorf("the event says %v lines, want 2", payload["lines"])
		case payload["statement_digest"] != imported.Digest:
			t.Errorf("the event carries digest %v, want %s", payload["statement_digest"], imported.Digest)
		}
	})

	each(ctx, t, tx, "the same statement again is the same run, announced once", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		first, err := store.ImportStatement(ctx, parties.statement(twoLines))
		if err != nil {
			t.Fatalf("the first import: %v", err)
		}
		// Reformatted - other whitespace, other key order - because a retry
		// from a different client renders the same statement differently,
		// and it is the same statement.
		reformatted := `{ "lines": [ {"paid": {"currency": "EUR", "minor": 250}, "transaction_id": "AWIN-1"},
		                             {"paid": {"currency": "EUR", "minor": -40}, "transaction_id": "AWIN-2"} ] }`
		again, err := store.ImportStatement(ctx, parties.statement(reformatted))
		if err != nil {
			t.Fatalf("the repeat import: %v", err)
		}
		switch {
		case !again.AlreadyImported:
			t.Error("the repeat does not report itself as one")
		case again.ID != first.ID:
			t.Errorf("the repeat produced run %s; the first was %s", again.ID, first.ID)
		case again.Digest != first.Digest:
			t.Errorf("the repeat's digest is %s, the first's %s: formatting changed the content", again.Digest, first.Digest)
		case !again.ImportedAt.Equal(first.ImportedAt):
			t.Errorf("the repeat reports imported_at %s, the first %s", again.ImportedAt, first.ImportedAt)
		case again.ImportedBy != first.ImportedBy:
			t.Errorf("the repeat reports importer %s, the first %s", again.ImportedBy, first.ImportedBy)
		case again.Lines != 2:
			t.Errorf("the repeat reports %d lines, want 2", again.Lines)
		}
		if n := runsFor(ctx, t, tx, parties.account); n != 1 {
			t.Errorf("%d runs for the account, want 1", n)
		}
		if events := importEvents(ctx, t, tx, first.ID); len(events) != 1 {
			t.Errorf("%d import events for the run after a repeat, want 1: a repeat announces nothing", len(events))
		}
	})

	each(ctx, t, tx, "a corrected statement is a new run", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		first, err := store.ImportStatement(ctx, parties.statement(twoLines))
		if err != nil {
			t.Fatalf("the first import: %v", err)
		}
		corrected := `{"lines":[{"transaction_id":"AWIN-1","paid":{"minor":250,"currency":"EUR"}},` +
			`{"transaction_id":"AWIN-2","paid":{"minor":-45,"currency":"EUR"}}]}`
		second, err := store.ImportStatement(ctx, parties.statement(corrected))
		if err != nil {
			t.Fatalf("the corrected import: %v", err)
		}
		switch {
		case second.AlreadyImported:
			t.Error("a statement with a different amount reports itself as a repeat")
		case second.ID == first.ID:
			t.Error("the corrected statement landed in the first run; an immutable run was rewritten")
		case second.Digest == first.Digest:
			t.Error("two statements with different amounts share a digest")
		}
		if n := runsFor(ctx, t, tx, parties.account); n != 2 {
			t.Errorf("%d runs for the account, want 2", n)
		}
		if events := importEvents(ctx, t, tx, second.ID); len(events) != 1 {
			t.Errorf("%d import events for the new run, want 1", len(events))
		}
	})

	each(ctx, t, tx, "the same statement for another period is another run", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		first, err := store.ImportStatement(ctx, parties.statement(twoLines))
		if err != nil {
			t.Fatalf("the first import: %v", err)
		}
		september := parties.statement(twoLines)
		september.Period = ops.Period{Start: august.End, End: august.End.AddDate(0, 1, 0)}
		second, err := store.ImportStatement(ctx, september)
		if err != nil {
			t.Fatalf("the September import: %v", err)
		}
		if second.AlreadyImported || second.ID == first.ID {
			t.Errorf("the same lines for another period were taken for the same run (%s / %s)", first.ID, second.ID)
		}
	})

	each(ctx, t, tx, "a statement for no such account is refused", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		ghost := parties.statement(twoLines)
		ghost.Account = uuid.New()
		before := announcementsOfImports(ctx, t, tx)
		_, err := store.ImportStatement(ctx, ghost)
		if !errors.Is(err, ops.ErrNoSuchNetworkAccount) {
			t.Fatalf("ImportStatement() = %v, want one wrapping ErrNoSuchNetworkAccount", err)
		}
		if n := runsFor(ctx, t, tx, ghost.Account); n != 0 {
			t.Errorf("%d runs for an account that does not exist", n)
		}
		if after := announcementsOfImports(ctx, t, tx); after != before {
			t.Errorf("a refused import was announced (%d events before, %d after)", before, after)
		}
	})

	each(ctx, t, tx, "a row the schema refuses leaves nothing behind", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		// An operator id that is no account: imported_by's foreign key
		// refuses the row, and the event that would have gone with it must
		// not survive on its own.
		nobody := parties.statement(twoLines)
		nobody.Operator = ops.Operator{ID: uuid.New(), DisplayName: "Nobody"}
		before := announcementsOfImports(ctx, t, tx)
		_, err := store.ImportStatement(ctx, nobody)
		if !errors.Is(err, ops.ErrStatementNotImported) {
			t.Fatalf("ImportStatement() = %v, want one wrapping ErrStatementNotImported", err)
		}
		if n := runsFor(ctx, t, tx, parties.account); n != 0 {
			t.Errorf("%d runs written by a refused import", n)
		}
		if after := announcementsOfImports(ctx, t, tx); after != before {
			t.Errorf("a refused import was announced (%d events before, %d after)", before, after)
		}
	})

	each(ctx, t, tx, "a statement that cannot be read never reaches the database", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		_, err := store.ImportStatement(ctx, parties.statement(`{"total":{"minor":210,"currency":"EUR"}}`))
		if !errors.Is(err, ops.ErrInvalidStatement) {
			t.Fatalf("ImportStatement() = %v, want one wrapping ErrInvalidStatement", err)
		}
		if n := runsFor(ctx, t, tx, parties.account); n != 0 {
			t.Errorf("%d runs written from a statement with no lines", n)
		}
	})
}
