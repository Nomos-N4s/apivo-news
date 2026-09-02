package ops_test

// The reconciliation endpoints with the store faked (T113). What the
// endpoints owe on their own: the shape of each body, the fields refused
// before anything is written, the operator taken from the token, and the
// statuses the store's answers map to. Whether the store keeps its promises
// is proved against the schema beside this file.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// reconciliationStore answers with canned results and records what it was
// asked.
type reconciliationStore struct {
	imported     ops.ImportedStatement
	importErr    error
	gotStatement ops.Statement
	imports      int

	detection ops.Detection
	detectErr error
	detected  []uuid.UUID

	listed  []ops.ListedDifference
	listErr error
	gotRun  uuid.UUID
	gotPage struct {
		after ops.DifferenceAfter
		limit int
	}

	resolved      ops.Resolved
	resolveErr    error
	gotResolution ops.Resolution
	resolutions   int

	ledger         []ops.LedgerRow
	reconciliation []ops.ReconciliationRow
	exportErr      error
	gotWindow      ops.ExportWindow
	exports        int
}

func (s *reconciliationStore) ImportStatement(_ context.Context, st ops.Statement) (ops.ImportedStatement, error) {
	s.gotStatement, s.imports = st, s.imports+1
	return s.imported, s.importErr
}

func (s *reconciliationStore) DetectDifferences(_ context.Context, run uuid.UUID) (ops.Detection, error) {
	s.detected = append(s.detected, run)
	return s.detection, s.detectErr
}

func (s *reconciliationStore) ListDifferences(_ context.Context, run uuid.UUID, after ops.DifferenceAfter, limit int) ([]ops.ListedDifference, error) {
	s.gotRun, s.gotPage.after, s.gotPage.limit = run, after, limit
	if s.listErr != nil {
		return nil, s.listErr
	}
	if len(s.listed) > limit {
		return s.listed[:limit], nil
	}
	return s.listed, nil
}

func (s *reconciliationStore) ResolveDifference(_ context.Context, r ops.Resolution) (ops.Resolved, error) {
	s.gotResolution, s.resolutions = r, s.resolutions+1
	return s.resolved, s.resolveErr
}

func (s *reconciliationStore) ExportLedger(_ context.Context, w ops.ExportWindow) ([]ops.LedgerRow, error) {
	s.gotWindow, s.exports = w, s.exports+1
	return s.ledger, s.exportErr
}

func (s *reconciliationStore) ExportReconciliation(_ context.Context, w ops.ExportWindow) ([]ops.ReconciliationRow, error) {
	s.gotWindow, s.exports = w, s.exports+1
	return s.reconciliation, s.exportErr
}

// reconcile sends an authenticated request to the reconciliation surface.
func reconcile(t *testing.T, store ops.ReconciliationStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, ops.Prefix+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{}, unreachableSettler{}, store, unreachableHeld{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// problemDetail reads the detail of a problem+json answer.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not problem+json: %v (body %q)", err, rec.Body.String())
	}
	return problem.Detail
}

var (
	importedAt = time.Date(2026, time.September, 2, 9, 0, 0, 123456000, time.UTC)
	augustJSON = `"period":{"start":"2026-08-01T00:00:00Z","end":"2026-09-01T00:00:00Z"}`
)

func anImport(account uuid.UUID, statement string) string {
	return `{"network_account_id":"` + account.String() + `",` + augustJSON + `,"statement":` + statement + `}`
}

func TestAnImportRecordsTheOperatorAndAnswersTheRun(t *testing.T) {
	t.Parallel()
	account, run := uuid.New(), uuid.New()
	store := &reconciliationStore{
		imported: ops.ImportedStatement{
			ID: run, Account: account, Network: "awin", Period: august, Lines: 2, Digest: "abc123",
			ImportedBy: anOperator.ID, ImportedAt: importedAt,
		},
		detection: ops.Detection{Run: run, Found: make([]ops.Difference, 3), Recorded: 2},
	}
	rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", anImport(account, twoLines))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	// The operator comes from the token, never the body.
	if store.gotStatement.Operator.ID != anOperator.ID {
		t.Errorf("imported by %s, want the authenticated caller %s", store.gotStatement.Operator.ID, anOperator.ID)
	}
	if store.gotStatement.Account != account || !store.gotStatement.Period.Start.Equal(august.Start) || !store.gotStatement.Period.End.Equal(august.End) {
		t.Errorf("statement framed as %+v, want account %s for August", store.gotStatement, account)
	}
	if string(store.gotStatement.Raw) != twoLines {
		t.Errorf("raw statement = %s, want the document verbatim", store.gotStatement.Raw)
	}
	if len(store.detected) != 1 || store.detected[0] != run {
		t.Errorf("detection ran for %v, want once for %s", store.detected, run)
	}

	var body struct {
		RunID            string `json:"run_id"`
		NetworkAccountID string `json:"network_account_id"`
		NetworkID        string `json:"network_id"`
		Period           struct{ Start, End string }
		Lines            int    `json:"lines"`
		StatementDigest  string `json:"statement_digest"`
		ImportedBy       string `json:"imported_by"`
		ImportedAt       string `json:"imported_at"`
		AlreadyImported  bool   `json:"already_imported"`
		Differences      struct {
			Found    int `json:"found"`
			Recorded int `json:"recorded"`
		} `json:"differences"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%q)", err, rec.Body.String())
	}
	for _, pair := range []struct{ name, got, want string }{
		{"run_id", body.RunID, run.String()},
		{"network_account_id", body.NetworkAccountID, account.String()},
		{"network_id", body.NetworkID, "awin"},
		{"period.start", body.Period.Start, "2026-08-01T00:00:00Z"},
		{"period.end", body.Period.End, "2026-09-01T00:00:00Z"},
		{"statement_digest", body.StatementDigest, "abc123"},
		{"imported_by", body.ImportedBy, anOperator.ID.String()},
		{"imported_at", body.ImportedAt, importedAt.Format(time.RFC3339Nano)},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %q, want %q", pair.name, pair.got, pair.want)
		}
	}
	if body.Lines != 2 || body.AlreadyImported || body.Differences.Found != 3 || body.Differences.Recorded != 2 {
		t.Errorf("lines %d already %v differences %+v; want 2, false, {3 2}", body.Lines, body.AlreadyImported, body.Differences)
	}
}

func TestARepeatedImportIsTheSameRunAndAnswers200(t *testing.T) {
	t.Parallel()
	store := &reconciliationStore{imported: ops.ImportedStatement{ID: uuid.New(), AlreadyImported: true, ImportedBy: uuid.New(), ImportedAt: importedAt}}
	rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", anImport(uuid.New(), twoLines))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a run that already existed (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"already_imported":true`) {
		t.Errorf("body does not say the run already existed: %s", rec.Body.String())
	}
	if len(store.detected) != 1 {
		t.Errorf("detection ran %d times on a repeat, want once: it records only what the first pass did not", len(store.detected))
	}
}

func TestAnImportIsRefusedBeforeTheStoreWhenItsFrameIsWrong(t *testing.T) {
	t.Parallel()
	account := uuid.New().String()
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"not JSON", `not json`, "not valid JSON", http.StatusBadRequest},
		{"an unknown field", `{"network_account_id":"` + account + `",` + augustJSON + `,"statement":{"lines":[]},"operator":"me"}`, "not valid JSON", http.StatusBadRequest},
		{"an account that is not a UUID", `{"network_account_id":"awin-1",` + augustJSON + `,"statement":{"lines":[]}}`, "network_account_id is not a UUID", http.StatusBadRequest},
		{"a start that is not a timestamp", `{"network_account_id":"` + account + `","period":{"start":"August","end":"2026-09-01T00:00:00Z"},"statement":{"lines":[]}}`, "period.start is not an RFC 3339 timestamp", http.StatusBadRequest},
		{"a missing end", `{"network_account_id":"` + account + `","period":{"start":"2026-08-01T00:00:00Z"},"statement":{"lines":[]}}`, "period.end is not an RFC 3339 timestamp", http.StatusBadRequest},
		{"a body past the limit", `{"network_account_id":"` + account + `",` + augustJSON + `,"statement":{"lines":[],"padding":"` + strings.Repeat("x", 4<<20) + `"}}`, "larger than this endpoint accepts", http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reconciliationStore{}
			rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if detail := problemDetail(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to say %q", detail, tc.want)
			}
			if store.imports != 0 {
				t.Error("a refused frame reached the store")
			}
		})
	}
}

func TestTheStoresImportVerdictsBecomeTheRightStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"a statement that cannot be read", errors.New("ops: the statement cannot be read: line 2 names no transaction_id"), http.StatusBadRequest, "line 2 names no transaction_id"},
		{"an account nobody has", ops.ErrNoSuchNetworkAccount, http.StatusNotFound, "no such publisher account"},
		{"a database that refused", ops.ErrStatementNotImported, http.StatusInternalServerError, ""},
	}
	// The first case wraps the sentinel the way the store does.
	cases[0].err = errors.Join(ops.ErrInvalidStatement, cases[0].err)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reconciliationStore{importErr: tc.err}
			rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", anImport(uuid.New(), twoLines))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.want != "" && !strings.Contains(problemDetail(t, rec), tc.want) {
				t.Errorf("detail = %q, want it to say %q", problemDetail(t, rec), tc.want)
			}
			if len(store.detected) != 0 {
				t.Error("detection ran on a statement that was not imported")
			}
		})
	}
}

func TestTheDetailOfARefusedStatementNamesTheLine(t *testing.T) {
	t.Parallel()
	// The real refusal, not a canned one: the store's own text, with the
	// module's prefix trimmed so the detail starts at the field.
	refused := ops.Statement{Account: uuid.New(), Period: august, Raw: json.RawMessage(`{"lines":[{"paid":{"minor":1,"currency":"EUR"}}]}`), Operator: anOperator}
	store := &reconciliationStore{importErr: refused.Validate()}
	rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", anImport(uuid.New(), `{"lines":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if detail := problemDetail(t, rec); detail != "line 1 names no transaction_id" {
		t.Errorf("detail = %q, want the field without the module prefix", detail)
	}
}

func TestADetectionFailureAfterTheImportIsA500(t *testing.T) {
	t.Parallel()
	store := &reconciliationStore{imported: ops.ImportedStatement{ID: uuid.New()}, detectErr: ops.ErrNotDetected}
	rec := reconcile(t, store, http.MethodPost, "reconciliation/runs", anImport(uuid.New(), twoLines))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if store.imports != 1 {
		t.Errorf("the import ran %d times, want once: the run stands and a retry finds it", store.imports)
	}
}

// listedRow builds one listed difference at a given instant.
func listedRow(at time.Time, kind ops.DifferenceKind) ops.ListedDifference {
	expected, actual := money.Amount{Minor: 499, Currency: "EUR"}, money.Amount{Minor: 450, Currency: "EUR"}
	row := ops.ListedDifference{ID: uuid.New(), Kind: kind, TransactionID: "A", DetectedAt: at, Delta: money.Amount{Minor: -49, Currency: "EUR"}}
	switch kind {
	case ops.AmountMismatch:
		row.Report, row.Expected, row.Actual = uuid.New(), &expected, &actual
	case ops.ReportedNotPaid:
		row.Report, row.Expected, row.Delta = uuid.New(), &expected, money.Amount{Minor: -499, Currency: "EUR"}
	case ops.PaidNotReported:
		row.Actual, row.Delta, row.TransactionID = &actual, actual, "X"
	}
	return row
}

func TestAListingRendersEachRowAndPagesLikeEveryQueue(t *testing.T) {
	t.Parallel()
	run := uuid.New()
	at := time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC)
	rows := []ops.ListedDifference{
		listedRow(at, ops.AmountMismatch),
		listedRow(at.Add(time.Second), ops.PaidNotReported),
		listedRow(at.Add(2*time.Second), ops.ReportedNotPaid),
	}
	rows[0].Superseded = true
	rows[0].Resolution = &ops.DifferenceResolution{Verdict: ops.VerdictAbsorbed, ResolvedBy: anOperator.ID, Reason: "too small", ResolvedAt: at.Add(time.Hour)}
	store := &reconciliationStore{listed: rows}

	rec := reconcile(t, store, http.MethodGet, "reconciliation/runs/"+run.String()+"/differences?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if store.gotRun != run || store.gotPage.limit != 3 {
		t.Errorf("asked the store for run %s with limit %d, want %s and one more than the page", store.gotRun, store.gotPage.limit, run)
	}
	var page struct {
		Items []struct {
			ID                   string          `json:"id"`
			Kind                 string          `json:"kind"`
			NetworkTransactionID *string         `json:"network_transaction_id"`
			TransactionID        string          `json:"transaction_id"`
			Expected             *money.Amount   `json:"expected"`
			Actual               *money.Amount   `json:"actual"`
			Delta                money.Amount    `json:"delta"`
			DetectedAt           string          `json:"detected_at"`
			Superseded           bool            `json:"superseded"`
			Resolution           *map[string]any `json:"resolution"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body: %v (%q)", err, rec.Body.String())
	}
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("page has %d items and cursor %v, want 2 items and a cursor", len(page.Items), page.NextCursor)
	}
	first, second := page.Items[0], page.Items[1]
	switch {
	case first.Kind != "amount_mismatch" || first.NetworkTransactionID == nil || *first.NetworkTransactionID != rows[0].Report.String():
		t.Errorf("first item = %+v, want the mismatch naming its report", first)
	case first.Expected == nil || first.Expected.Minor != 499 || first.Actual == nil || first.Actual.Minor != 450 || first.Delta.Minor != -49:
		t.Errorf("first item's figures = %v/%v/%v, want 499/450/-49", first.Expected, first.Actual, first.Delta)
	case !first.Superseded:
		t.Error("the superseded flag was dropped")
	case first.Resolution == nil || (*first.Resolution)["resolution"] != "absorbed" || (*first.Resolution)["resolved_by"] != anOperator.ID.String():
		t.Errorf("first item's resolution = %v, want absorbed by the operator", first.Resolution)
	case second.Kind != "paid_not_reported" || second.NetworkTransactionID != nil || second.TransactionID != "X" || second.Expected != nil || second.Resolution != nil:
		t.Errorf("second item = %+v, want money matching no report, open, naming line X", second)
	}

	// The cursor continues after the last row shown, on this list only.
	rec = reconcile(t, store, http.MethodGet, "reconciliation/runs/"+run.String()+"/differences?cursor="+*page.NextCursor, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second page status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if !store.gotPage.after.DetectedAt.Equal(rows[1].DetectedAt) || store.gotPage.after.ID != rows[1].ID {
		t.Errorf("the cursor continued after %+v, want after the second row", store.gotPage.after)
	}
}

func TestAListingIsRefusedForABadPage(t *testing.T) {
	t.Parallel()
	run := uuid.New().String()
	cases := []struct{ name, path, want string }{
		{"a run id that is not a UUID", "reconciliation/runs/run-1/differences", "the run id is not a UUID"},
		{"a limit past the bound", "reconciliation/runs/" + run + "/differences?limit=101", "limit must be a whole number between 1 and 100"},
		{"a cursor from another queue", "reconciliation/runs/" + run + "/differences?cursor=dW5hdHRyaWJ1dGVkfDIwMjYtMDgtMzBUMTE6MDA6MDBafDAwMDAwMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMA", "cursor is not one this endpoint issued"},
		{"an unknown parameter", "reconciliation/runs/" + run + "/differences?kind=open", "unknown query parameter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reconciliationStore{}
			rec := reconcile(t, store, http.MethodGet, tc.path, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if detail := problemDetail(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to say %q", detail, tc.want)
			}
			if store.gotRun != uuid.Nil {
				t.Error("a refused page reached the store")
			}
		})
	}
}

func TestAListingOfARunNobodyImportedIs404(t *testing.T) {
	t.Parallel()
	store := &reconciliationStore{listErr: ops.ErrNoSuchRun}
	rec := reconcile(t, store, http.MethodGet, "reconciliation/runs/"+uuid.New().String()+"/differences", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	store = &reconciliationStore{listErr: ops.ErrDifferencesUnread}
	if rec := reconcile(t, store, http.MethodGet, "reconciliation/runs/"+uuid.New().String()+"/differences", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a store failure answered %d, want 500", rec.Code)
	}
}

func TestAResolutionRecordsTheOperatorTheVerdictAndTheReason(t *testing.T) {
	t.Parallel()
	id, run := uuid.New(), uuid.New()
	resolvedAt := importedAt.Add(time.Hour)
	store := &reconciliationStore{resolved: ops.Resolved{
		ID: id, Run: run, Kind: ops.AmountMismatch, Verdict: ops.VerdictExplained,
		ResolvedBy: anOperator.ID, Reason: "paid in full on the September statement", ResolvedAt: resolvedAt,
	}}
	rec := reconcile(t, store, http.MethodPost, "reconciliation/differences/"+id.String()+"/resolve",
		`{"resolution":"explained","reason":"  paid in full on the September statement  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := store.gotResolution
	if got.ID != id || got.Verdict != ops.VerdictExplained || got.Operator.ID != anOperator.ID || strings.TrimSpace(got.Reason) != "paid in full on the September statement" {
		t.Errorf("the store was asked %+v; want the row, the verdict, the token's operator and the reason", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v (%q)", err, rec.Body.String())
	}
	for key, want := range map[string]any{
		"id": id.String(), "run_id": run.String(), "kind": "amount_mismatch", "resolution": "explained",
		"resolved_by": anOperator.ID.String(), "reason": "paid in full on the September statement",
		"resolved_at": resolvedAt.Format(time.RFC3339Nano),
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
}

func TestAResolutionIsRefusedBeforeTheStore(t *testing.T) {
	t.Parallel()
	id := uuid.New().String()
	cases := []struct{ name, path, body, want string }{
		{"an id that is not a UUID", "reconciliation/differences/row-1/resolve", `{"resolution":"explained","reason":"x"}`, "the difference id is not a UUID"},
		{"a body that is not this endpoint's", "reconciliation/differences/" + id + "/resolve", `{"verdict":"explained","reason":"x"}`, "not valid JSON"},
		{"a verdict nobody knows", "reconciliation/differences/" + id + "/resolve", `{"resolution":"chasing","reason":"x"}`, `"chasing" is not a verdict`},
		{"a blank reason", "reconciliation/differences/" + id + "/resolve", `{"resolution":"absorbed","reason":"   "}`, "non-blank reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reconciliationStore{}
			rec := reconcile(t, store, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if detail := problemDetail(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to say %q", detail, tc.want)
			}
			if store.resolutions != 0 {
				t.Error("a refused resolution reached the store (FR-061: the audit record is part of the action)")
			}
		})
	}
}

func TestTheStoresResolutionVerdictsBecomeTheRightStatus(t *testing.T) {
	t.Parallel()
	first := ops.AlreadyResolvedError{ID: uuid.New(), By: uuid.New(), Verdict: ops.VerdictAbsorbed, Reason: "too small to dispute", At: importedAt}
	cases := []struct {
		name   string
		err    error
		status int
		want   []string
	}{
		{"a row nobody has", ops.ErrNoSuchDifference, http.StatusNotFound, []string{"no such difference"}},
		{"a row somebody decided first", first, http.StatusConflict, []string{"absorbed", first.By.String(), "too small to dispute", "reload the queue"}},
		{"a database that refused", ops.ErrNotResolved, http.StatusInternalServerError, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &reconciliationStore{resolveErr: tc.err}
			rec := reconcile(t, store, http.MethodPost, "reconciliation/differences/"+uuid.New().String()+"/resolve", `{"resolution":"explained","reason":"x"}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			detail := problemDetail(t, rec)
			for _, want := range tc.want {
				if !strings.Contains(detail, want) {
					t.Errorf("detail = %q, want it to mention %q", detail, want)
				}
			}
		})
	}
}
