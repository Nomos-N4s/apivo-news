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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// schemaPool opens the migrated database and one transaction every case
// nests a savepoint under, or skips the test when there is no database.
func schemaPool(t *testing.T) (context.Context, pgx.Tx, func()) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise reconciliation")
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
		t.Fatalf("beginning: %v", err)
	}
	return ctx, tx, func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	}
}

func TestStatementImportAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaPool(t)
	defer done()
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

// US6's independent test (T116), end to end through the endpoints and the
// real store: a statement that omits one approved transaction and shorts
// another. Both must be flagged with their deltas, and neither may silently
// change a member's balance - which here means no entry changes state, no
// transition is written, no ledger posting is made, and the confirmation
// gate (FR-043) holds both transactions until an operator decides them.

// gateHolds answers the confirmation gate's own question for a report - the
// same reading earnings makes - without importing another module's store.
func gateHolds(ctx context.Context, t *testing.T, tx pgx.Tx, report uuid.UUID) bool {
	t.Helper()
	var reconciled bool
	if err := tx.QueryRow(ctx, `
		select exists (
		    select 1 from cashback.reconciliation_run run
		     where run.network_account_id = nt.network_account_id
		       and nt.transacted_at >= run.statement_period_start
		       and nt.transacted_at < run.statement_period_end
		) and not exists (
		    select 1 from cashback.reconciliation_difference diff
		      join cashback.network_transaction named on named.id = diff.network_transaction_id
		     where named.network_id = nt.network_id and named.external_id = nt.external_id
		       and diff.resolved_at is null
		)
		  from cashback.network_transaction nt where nt.id = $1`, report).Scan(&reconciled); err != nil {
		t.Fatalf("asking the gate about %s: %v", report, err)
	}
	return reconciled
}

// moneyFootprint is everything a balance is made of: entry states,
// transitions and ledger postings. Two equal footprints mean no member's
// balance moved.
func moneyFootprint(ctx context.Context, t *testing.T, tx pgx.Tx, memberID uuid.UUID) string {
	t.Helper()
	var footprint string
	if err := tx.QueryRow(ctx, `
		select (select string_agg(e.network_transaction_id::text || ':' || e.state || ':' || e.amount_minor, ',' order by e.network_transaction_id)
		          from cashback.entry e where e.account_id = $1)
		    || ' | transitions ' || (select count(*) from cashback.entry_transition)
		    || ' | postings ' || (select count(*) from ledger.posting)
		    || ' | posted ' || (select coalesce(sum(amount_minor), 0) from ledger.posting)`, memberID).Scan(&footprint); err != nil {
		t.Fatalf("reading the money footprint: %v", err)
	}
	return footprint
}

func TestAnOmittedAndAShortedTransactionAreFlaggedAndMoveNoMoney(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaPool(t)
	defer done()
	parties := seedStatementParties(ctx, t, tx)

	each(ctx, t, tx, "US6", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		memberID := member(ctx, t, tx)
		omitted := reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		matched := reported(ctx, t, tx, parties, "B", 300, inAugust, uuid.Nil)
		shorted := reported(ctx, t, tx, parties, "C", 499, inAugust, uuid.Nil)
		moved(ctx, t, tx, memberID, omitted, nil, "pending", "tx-a-"+suffix(t), inAugust.Add(time.Hour), nil, nil)
		moved(ctx, t, tx, memberID, matched, nil, "pending", "tx-b-"+suffix(t), inAugust.Add(time.Hour), nil, nil)
		moved(ctx, t, tx, memberID, shorted, nil, "confirmed", "tx-c-"+suffix(t), inAugust.Add(time.Hour), nil, nil)
		before := moneyFootprint(ctx, t, tx, memberID)

		h := ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{}, unreachableSettler{}, store, stubAuth{op: parties.operator})
		call := func(method, path, body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, ops.Prefix+path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}

		// The statement pays B in full, shorts C, and never mentions A.
		rec := call(http.MethodPost, "reconciliation/runs",
			`{"network_account_id":"`+parties.account.String()+`","period":{"start":"2026-08-01T00:00:00Z","end":"2026-09-01T00:00:00Z"},`+
				`"statement":{"lines":[{"transaction_id":"B","paid":{"minor":300,"currency":"EUR"}},{"transaction_id":"C","paid":{"minor":450,"currency":"EUR"}}]}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("import status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var imported struct {
			RunID       string `json:"run_id"`
			Differences struct {
				Found    int `json:"found"`
				Recorded int `json:"recorded"`
			} `json:"differences"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
			t.Fatalf("import body: %v", err)
		}
		if imported.Differences.Found != 2 || imported.Differences.Recorded != 2 {
			t.Fatalf("differences = %+v, want the omitted and the shorted transaction, both recorded", imported.Differences)
		}

		// Scenario 1: both listed, each with its amount difference.
		rec = call(http.MethodGet, "reconciliation/runs/"+imported.RunID+"/differences", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("listing status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var page struct {
			Items []struct {
				ID                   string  `json:"id"`
				Kind                 string  `json:"kind"`
				NetworkTransactionID *string `json:"network_transaction_id"`
				TransactionID        string  `json:"transaction_id"`
				Delta                struct {
					Minor    int64  `json:"minor"`
					Currency string `json:"currency"`
				} `json:"delta"`
				Resolution *map[string]any `json:"resolution"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("listing body: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("%d differences listed, want 2", len(page.Items))
		}
		var shortedID string
		for _, item := range page.Items {
			switch item.TransactionID {
			case "A":
				if item.Kind != "reported_not_paid" || item.Delta.Minor != -499 || item.Delta.Currency != "EUR" || item.NetworkTransactionID == nil || *item.NetworkTransactionID != omitted.String() {
					t.Errorf("the omitted transaction is listed as %+v, want reported_not_paid, -499 EUR, naming its report", item)
				}
			case "C":
				shortedID = item.ID
				if item.Kind != "amount_mismatch" || item.Delta.Minor != -49 || item.NetworkTransactionID == nil || *item.NetworkTransactionID != shorted.String() {
					t.Errorf("the shorted transaction is listed as %+v, want amount_mismatch, -49 EUR, naming its report", item)
				}
			default:
				t.Errorf("an unexpected difference was listed: %+v", item)
			}
		}

		// Nothing moved, and the gate holds exactly the two disputed ones.
		if after := moneyFootprint(ctx, t, tx, memberID); after != before {
			t.Errorf("importing changed the money:\n before %s\n after  %s", before, after)
		}
		if gateHolds(ctx, t, tx, omitted) || gateHolds(ctx, t, tx, shorted) {
			t.Error("the gate would confirm a transaction the statement disputes")
		}
		if !gateHolds(ctx, t, tx, matched) {
			t.Error("the gate holds the transaction the statement paid in full")
		}

		// Scenario 2: a resolution records who and why, and moves nothing
		// either - the shortfall is absorbed, and only that gate opens.
		rec = call(http.MethodPost, "reconciliation/differences/"+shortedID+"/resolve",
			`{"resolution":"absorbed","reason":"a 49 cent shortfall is not worth disputing"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"resolved_by":"`+parties.operator.ID.String()+`"`) ||
			!strings.Contains(rec.Body.String(), `"reason":"a 49 cent shortfall is not worth disputing"`) {
			t.Errorf("the resolution does not record who and why: %s", rec.Body.String())
		}
		if after := moneyFootprint(ctx, t, tx, memberID); after != before {
			t.Errorf("resolving changed the money:\n before %s\n after  %s", before, after)
		}
		if !gateHolds(ctx, t, tx, shorted) {
			t.Error("the absorbed shortfall still holds its transaction at the gate")
		}
		if gateHolds(ctx, t, tx, omitted) {
			t.Error("resolving one difference opened the gate for the other")
		}
	})
}
