package ops_test

// The accounting exports with the store faked (T114, FR-062): the two
// renderings, the envelope, the filename, the guard on text this process
// did not write, and the refusals that never reach the store.

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const augustWindow = "from=2026-08-01T00:00:00Z&to=2026-09-01T00:00:00Z"

var (
	ledgerAt   = time.Date(2026, time.August, 15, 12, 0, 0, 500000000, time.UTC)
	ledgerRows = []ops.LedgerRow{
		{
			TransitionID: uuid.New(), EntryID: uuid.New(), Member: uuid.New(), Brand: "fixture", Report: uuid.New(),
			From: "", To: "pending", Amount: money.Amount{Minor: 250, Currency: "EUR"}, TransferRef: "tx-1",
			Reason: "", Actor: uuid.Nil, OccurredAt: ledgerAt,
		},
		{
			TransitionID: uuid.New(), EntryID: uuid.New(), Member: uuid.New(), Brand: "fixture", Report: uuid.New(),
			From: "pending", To: "reversed", Amount: money.Amount{Minor: 250, Currency: "EUR"}, TransferRef: "tx-2",
			Reason: "=HYPERLINK(\"https://evil.example\")", Actor: anOperator.ID, OccurredAt: ledgerAt.Add(time.Hour),
		},
	}
)

func TestTheLedgerExportCarriesEveryRowAndSaysHowMany(t *testing.T) {
	t.Parallel()
	store := &reconciliationStore{ledger: ledgerRows}
	rec := reconcile(t, store, http.MethodGet, "exports/ledger?"+augustWindow, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !store.gotWindow.From.Equal(august.Start) || !store.gotWindow.To.Equal(august.End) {
		t.Errorf("the store was asked for %+v, want August", store.gotWindow)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="cashback-ledger-20260801-20260901.json"` {
		t.Errorf("Content-Disposition = %q, want the journal and the window's dates", got)
	}
	var doc struct {
		ExportedAt string `json:"exported_at"`
		From       string `json:"from"`
		To         string `json:"to"`
		RowCount   int    `json:"row_count"`
		Rows       []struct {
			TransitionID         string       `json:"transition_id"`
			FromState            *string      `json:"from_state"`
			ToState              string       `json:"to_state"`
			Amount               money.Amount `json:"amount"`
			LedgerTransferRef    string       `json:"ledger_transfer_ref"`
			Reason               *string      `json:"reason"`
			ActorID              *string      `json:"actor_id"`
			NetworkTransactionID string       `json:"network_transaction_id"`
			OccurredAt           string       `json:"occurred_at"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body: %v (%q)", err, rec.Body.String())
	}
	if doc.RowCount != 2 || len(doc.Rows) != 2 || doc.From != "2026-08-01T00:00:00Z" || doc.To != "2026-09-01T00:00:00Z" || doc.ExportedAt == "" {
		t.Fatalf("envelope = %+v, want August with 2 rows and a taken-at", doc)
	}
	opening, reversal := doc.Rows[0], doc.Rows[1]
	switch {
	case opening.FromState != nil || opening.ToState != "pending" || opening.Reason != nil || opening.ActorID != nil:
		t.Errorf("the opening row = %+v, want null from_state, reason and actor", opening)
	case opening.Amount.Minor != 250 || opening.Amount.Currency != "EUR" || opening.LedgerTransferRef != "tx-1":
		t.Errorf("the opening row's money = %+v / %s, want 250 EUR on tx-1", opening.Amount, opening.LedgerTransferRef)
	case reversal.FromState == nil || *reversal.FromState != "pending" || reversal.ActorID == nil || *reversal.ActorID != anOperator.ID.String():
		t.Errorf("the reversal row = %+v, want from pending by the operator", reversal)
	case reversal.Reason == nil || *reversal.Reason != ledgerRows[1].Reason:
		t.Errorf("the reversal's reason = %v, want it verbatim in JSON (the guard is the CSV's)", reversal.Reason)
	case opening.OccurredAt != ledgerAt.Format(time.RFC3339Nano):
		t.Errorf("occurred_at = %s, want %s", opening.OccurredAt, ledgerAt.Format(time.RFC3339Nano))
	}
}

func TestTheLedgerCSVIsTheJSONFlattenedWithMoneyAsTwoColumns(t *testing.T) {
	t.Parallel()
	store := &reconciliationStore{ledger: ledgerRows}
	rec := reconcile(t, store, http.MethodGet, "exports/ledger?"+augustWindow+"&format=csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", kind)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="cashback-ledger-20260801-20260901.csv"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not parse: %v\n%s", err, rec.Body.String())
	}
	if len(records) != 3 {
		t.Fatalf("%d records, want a header and two rows", len(records))
	}
	header := strings.Join(records[0], ",")
	if header != "transition_id,occurred_at,entry_id,account_id,brand_id,network_transaction_id,from_state,to_state,amount_minor,currency,ledger_transfer_ref,actor_id,reason" {
		t.Errorf("header = %s", header)
	}
	col := func(record []string, name string) string {
		for i, h := range records[0] {
			if h == name {
				return record[i]
			}
		}
		t.Fatalf("no column %s", name)
		return ""
	}
	opening, reversal := records[1], records[2]
	if col(opening, "amount_minor") != "250" || col(opening, "currency") != "EUR" || col(opening, "from_state") != "" || col(opening, "actor_id") != "" {
		t.Errorf("the opening row = %v, want 250 | EUR and empty from_state and actor", opening)
	}
	// A reason beginning with = is a formula to a spreadsheet; it is
	// written with a leading apostrophe so it stays text.
	if got := col(reversal, "reason"); got != "'"+ledgerRows[1].Reason {
		t.Errorf("reason cell = %q, want it neutralised with a leading apostrophe", got)
	}
	if col(reversal, "actor_id") != anOperator.ID.String() {
		t.Errorf("actor_id = %q, want the operator", col(reversal, "actor_id"))
	}
}

func TestTheReconciliationExportCarriesTheStatementAndTheDecision(t *testing.T) {
	t.Parallel()
	expected, actual := money.Amount{Minor: 499, Currency: "EUR"}, money.Amount{Minor: 450, Currency: "EUR"}
	run, report := uuid.New(), uuid.New()
	rows := []ops.ReconciliationRow{
		{
			DifferenceID: uuid.New(), Run: run, Account: uuid.New(), Network: "awin", Publisher: "publisher-1", Period: august,
			Kind: ops.AmountMismatch, Report: report, TransactionID: "A", Expected: &expected, Actual: &actual,
			Delta: money.Amount{Minor: -49, Currency: "EUR"}, DetectedAt: ledgerAt,
			Resolution: &ops.DifferenceResolution{Verdict: ops.VerdictAbsorbed, ResolvedBy: anOperator.ID, Reason: "-too small", ResolvedAt: ledgerAt.Add(time.Hour)},
		},
		{
			DifferenceID: uuid.New(), Run: run, Account: uuid.New(), Network: "awin", Publisher: "publisher-1", Period: august,
			Kind: ops.PaidNotReported, TransactionID: "+X", Actual: &actual, Delta: actual, DetectedAt: ledgerAt.Add(time.Minute),
		},
	}
	store := &reconciliationStore{reconciliation: rows}

	rec := reconcile(t, store, http.MethodGet, "exports/reconciliation?"+augustWindow, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var doc struct {
		RowCount int `json:"row_count"`
		Rows     []struct {
			RunID                string                      `json:"run_id"`
			NetworkID            string                      `json:"network_id"`
			ExternalPublisherID  string                      `json:"external_publisher_id"`
			StatementPeriod      struct{ Start, End string } `json:"statement_period"`
			Kind                 string                      `json:"kind"`
			NetworkTransactionID *string                     `json:"network_transaction_id"`
			TransactionID        string                      `json:"transaction_id"`
			Expected             *money.Amount               `json:"expected"`
			Delta                money.Amount                `json:"delta"`
			Resolution           *map[string]any             `json:"resolution"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body: %v (%q)", err, rec.Body.String())
	}
	if doc.RowCount != 2 || len(doc.Rows) != 2 {
		t.Fatalf("row_count %d, rows %d; want 2 and 2", doc.RowCount, len(doc.Rows))
	}
	decided, open := doc.Rows[0], doc.Rows[1]
	switch {
	case decided.RunID != run.String() || decided.NetworkID != "awin" || decided.ExternalPublisherID != "publisher-1":
		t.Errorf("the decided row's statement = %+v, want run %s at awin/publisher-1", decided, run)
	case decided.StatementPeriod.Start != "2026-08-01T00:00:00Z" || decided.StatementPeriod.End != "2026-09-01T00:00:00Z":
		t.Errorf("statement_period = %+v, want August", decided.StatementPeriod)
	case decided.NetworkTransactionID == nil || *decided.NetworkTransactionID != report.String() || decided.Expected == nil || decided.Delta.Minor != -49:
		t.Errorf("the decided row = %+v, want its report, expected and delta -49", decided)
	case decided.Resolution == nil || (*decided.Resolution)["resolution"] != "absorbed" || (*decided.Resolution)["reason"] != "-too small":
		t.Errorf("the decided row's resolution = %v, want absorbed with the reason verbatim", decided.Resolution)
	case open.NetworkTransactionID != nil || open.TransactionID != "+X" || open.Expected != nil || open.Resolution != nil:
		t.Errorf("the open row = %+v, want no report, line +X, no expected, no resolution", open)
	}

	rec = reconcile(t, store, http.MethodGet, "exports/reconciliation?"+augustWindow+"&format=csv", "")
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(records) != 3 {
		t.Fatalf("the CSV: %v, %d records (body %q)", err, len(records), rec.Body.String())
	}
	col := func(record []string, name string) string {
		for i, h := range records[0] {
			if h == name {
				return record[i]
			}
		}
		t.Fatalf("no column %s", name)
		return ""
	}
	first, second := records[1], records[2]
	if col(first, "expected_minor") != "499" || col(first, "actual_minor") != "450" || col(first, "delta_minor") != "-49" || col(first, "currency") != "EUR" {
		t.Errorf("the decided row's figures = %v", first)
	}
	if col(first, "resolution") != "absorbed" || col(first, "reason") != "'-too small" || col(first, "resolved_by") != anOperator.ID.String() {
		t.Errorf("the decided row's decision = %v, want absorbed with the reason neutralised", first)
	}
	if col(second, "expected_minor") != "" || col(second, "network_transaction_id") != "" || col(second, "transaction_id") != "'+X" || col(second, "resolution") != "" {
		t.Errorf("the open row = %v, want empty expected, report and decision, and the line id neutralised", second)
	}
}

func TestAnExportIsRefusedBeforeTheStoreForABadWindow(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, query, want string }{
		{"no window", "", "supply from and to"},
		{"no end", "from=2026-08-01T00:00:00Z", "supply from and to"},
		{"a start that is not a timestamp", "from=August&to=2026-09-01T00:00:00Z", "from is not an RFC 3339 timestamp"},
		{"an end that is not a timestamp", "from=2026-08-01T00:00:00Z&to=September", "to is not an RFC 3339 timestamp"},
		{"an inverted window", "from=2026-09-01T00:00:00Z&to=2026-08-01T00:00:00Z", "not after it starts"},
		{"an empty window", "from=2026-08-01T00:00:00Z&to=2026-08-01T00:00:00Z", "not after it starts"},
		{"a format nobody serves", augustWindow + "&format=xlsx", "format must be json or csv"},
		{"a repeated format", augustWindow + "&format=csv&format=json", `"format" was given 2 times`},
		{"a filter", augustWindow + "&kind=amount_mismatch", "unknown query parameter"},
	}
	for _, path := range []string{"exports/ledger", "exports/reconciliation"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				store := &reconciliationStore{}
				rec := reconcile(t, store, http.MethodGet, path+"?"+tc.query, "")
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
				}
				if detail := problemDetail(t, rec); !strings.Contains(detail, tc.want) {
					t.Errorf("detail = %q, want it to say %q", detail, tc.want)
				}
				if store.exports != 0 {
					t.Error("a refused window reached the store")
				}
			})
		}
	}
}

func TestTheStoresExportVerdictsBecomeTheRightStatus(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"exports/ledger", "exports/reconciliation"} {
		t.Run(path+"/too large", func(t *testing.T) {
			t.Parallel()
			rec := reconcile(t, &reconciliationStore{exportErr: ops.ErrExportTooLarge}, http.MethodGet, path+"?"+augustWindow, "")
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
			}
			if detail := problemDetail(t, rec); !strings.Contains(detail, "narrower window") {
				t.Errorf("detail = %q, want it to suggest a narrower window", detail)
			}
		})
		t.Run(path+"/a database that refused", func(t *testing.T) {
			t.Parallel()
			rec := reconcile(t, &reconciliationStore{exportErr: ops.ErrExportUnread}, http.MethodGet, path+"?"+augustWindow, "")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
		})
	}
}

func TestAnEmptyWindowIsAnEmptyDocumentNotAnError(t *testing.T) {
	t.Parallel()
	rec := reconcile(t, &reconciliationStore{}, http.MethodGet, "exports/ledger?"+augustWindow, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"row_count":0`) || !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("body = %s, want zero rows and an empty list, not null", rec.Body.String())
	}
}
