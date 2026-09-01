package ops_test

// GET /api/v1/cashback/ops/unattributed: the page an operator works from.
//
// The store is faked here because what this file is about is the endpoint's
// own promises - the money shape, the paging bound, the cursor it will and
// will not accept. Whether the rows it renders are genuinely still open is
// the store's question, and it is asserted against the schema in the
// networks module where that predicate lives.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// unreachableStore fails loudly. The auth cases must never reach the store,
// and a case that does is asserting something it did not mean to.
type unreachableStore struct{}

func (unreachableStore) Open(context.Context, networks.After, int) ([]networks.OpenReport, error) {
	return nil, errors.New("this case must not reach the store")
}

func (unreachableStore) Dismiss(context.Context, ops.Dismissal) (ops.Dismissed, error) {
	return ops.Dismissed{}, errors.New("this case must not reach the store")
}

// pageStore answers with a canned page and records what it was asked for.
type pageStore struct {
	rows []networks.OpenReport
	err  error

	gotAfter networks.After
	gotLimit int
}

func (*pageStore) Dismiss(context.Context, ops.Dismissal) (ops.Dismissed, error) {
	return ops.Dismissed{}, errors.New("the listing cases must not dismiss anything")
}

func (s *pageStore) Open(_ context.Context, after networks.After, limit int) ([]networks.OpenReport, error) {
	s.gotAfter, s.gotLimit = after, limit
	if s.err != nil {
		return nil, s.err
	}
	// Honour the bound, so the endpoint's "is there another page?" test
	// sees what a real store would give it.
	if len(s.rows) > limit {
		return s.rows[:limit], nil
	}
	return s.rows, nil
}

// detectedAt is the instant every fixture row carries, offset by its index.
// One poll stamps every observation it records with the same clock read, so
// the fixtures share a second and differ in microseconds - which is the case
// the (detected_at, id) keyset exists for.
var detectedAt = time.Date(2026, time.August, 30, 9, 15, 0, 0, time.UTC)

// report builds one open report, distinguishable from its neighbours by
// index alone.
func report(t *testing.T, i int) networks.OpenReport {
	t.Helper()
	sale, err := money.New(int64(10_000+i), "EUR")
	if err != nil {
		t.Fatalf("sale amount: %v", err)
	}
	commission, err := money.New(int64(500+i), "EUR")
	if err != nil {
		t.Fatalf("commission amount: %v", err)
	}
	return networks.OpenReport{
		ID:           uuid.New(),
		DetectedAt:   detectedAt.Add(time.Duration(i) * time.Microsecond),
		Report:       uuid.New(),
		Account:      uuid.New(),
		Network:      "awin",
		ExternalID:   "TX-" + strconv.Itoa(i),
		Status:       "pending",
		Sale:         sale,
		Commission:   commission,
		TransactedAt: detectedAt.Add(-2 * time.Hour),
		RetrievedAt:  detectedAt.Add(-time.Minute),
		Attributable: i%2 == 0,
	}
}

// reports builds n of them.
func reports(t *testing.T, n int) []networks.OpenReport {
	t.Helper()
	rows := make([]networks.OpenReport, 0, n)
	for i := range n {
		rows = append(rows, report(t, i))
	}
	return rows
}

// listedPage is the response body, decoded loosely enough that a field this
// test does not name still has to be valid JSON in the right place.
type listedPage struct {
	Items []struct {
		ID                   string          `json:"id"`
		DetectedAt           string          `json:"detected_at"`
		NetworkTransactionID string          `json:"network_transaction_id"`
		NetworkAccountID     string          `json:"network_account_id"`
		NetworkID            string          `json:"network_id"`
		ExternalID           string          `json:"external_id"`
		Status               string          `json:"status"`
		Sale                 json.RawMessage `json:"sale"`
		Commission           json.RawMessage `json:"commission"`
		TransactedAt         string          `json:"transacted_at"`
		RetrievedAt          string          `json:"retrieved_at"`
		Attributable         bool            `json:"attributable"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// list sends an authenticated GET with the given query and returns the
// recorder.
func list(t *testing.T, store ops.UnattributedStore, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := ops.Prefix + "unattributed"
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), store, unreachableApprover{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// listOK sends the request and decodes a 200 body.
func listOK(t *testing.T, store ops.UnattributedStore, query string) listedPage {
	t.Helper()
	rec := list(t, store, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var page listedPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body is not the contract's list shape: %v (body %q)", err, rec.Body.String())
	}
	return page
}

func TestAnEmptyQueueIsAnEmptyListAndNotANullOne(t *testing.T) {
	t.Parallel()

	// The distinction matters to the client: `[]` renders "nothing to do",
	// `null` is a field a caller has to defend against before it can say
	// the same thing.
	page := listOK(t, &pageStore{}, "")

	if page.Items == nil {
		t.Error("items = null, want an empty list")
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %d rows, want none", len(page.Items))
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null on an empty queue", *page.NextCursor)
	}
}

func TestEveryFactTheOperatorNeedsSurvivesTheRendering(t *testing.T) {
	t.Parallel()

	row := report(t, 0)
	page := listOK(t, &pageStore{rows: []networks.OpenReport{row}}, "")

	if len(page.Items) != 1 {
		t.Fatalf("items = %d rows, want 1", len(page.Items))
	}
	got := page.Items[0]

	for _, pair := range []struct{ name, got, want string }{
		{"id", got.ID, row.ID.String()},
		{"network_transaction_id", got.NetworkTransactionID, row.Report.String()},
		{"network_account_id", got.NetworkAccountID, row.Account.String()},
		{"network_id", got.NetworkID, string(row.Network)},
		{"external_id", got.ExternalID, row.ExternalID},
		{"status", got.Status, string(row.Status)},
		{"detected_at", got.DetectedAt, row.DetectedAt.UTC().Format(time.RFC3339Nano)},
		{"transacted_at", got.TransactedAt, row.TransactedAt.UTC().Format(time.RFC3339Nano)},
		{"retrieved_at", got.RetrievedAt, row.RetrievedAt.UTC().Format(time.RFC3339Nano)},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %q, want %q", pair.name, pair.got, pair.want)
		}
	}
	if got.Attributable != row.Attributable {
		t.Errorf("attributable = %v, want %v", got.Attributable, row.Attributable)
	}
}

// TestMoneyIsAlwaysMinorUnitsAndACurrency is C-6 on the wire. A decimal
// here would be a rounding decision taken by whichever JSON parser read it,
// on a number an operator is about to credit somebody.
func TestMoneyIsAlwaysMinorUnitsAndACurrency(t *testing.T) {
	t.Parallel()

	row := report(t, 0)
	page := listOK(t, &pageStore{rows: []networks.OpenReport{row}}, "")

	for _, field := range []struct {
		name string
		raw  json.RawMessage
		want money.Amount
	}{
		{"sale", page.Items[0].Sale, row.Sale},
		{"commission", page.Items[0].Commission, row.Commission},
	} {
		var amount struct {
			Minor    json.Number `json:"minor"`
			Currency string      `json:"currency"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(field.raw)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&amount); err != nil {
			t.Errorf("%s = %s, which is not {minor, currency}: %v", field.name, field.raw, err)
			continue
		}
		if strings.ContainsAny(amount.Minor.String(), ".eE") {
			t.Errorf("%s minor = %s, which is not an integer literal", field.name, amount.Minor)
		}
		if amount.Minor.String() != strconv.FormatInt(field.want.Minor, 10) {
			t.Errorf("%s minor = %s, want %d", field.name, amount.Minor, field.want.Minor)
		}
		if amount.Currency != string(field.want.Currency) {
			t.Errorf("%s currency = %q, want %q", field.name, amount.Currency, field.want.Currency)
		}
	}
}

func TestThePageIsBoundedAndTheDefaultIsTheContractsTwenty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		query     string
		wantLimit int
	}{
		{name: "no limit is the default", query: "", wantLimit: 20},
		{name: "a limit is honoured", query: "limit=5", wantLimit: 5},
		{name: "the maximum is honoured", query: "limit=100", wantLimit: 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &pageStore{}
			listOK(t, store, tc.query)

			// One more than the page, so the endpoint can tell a full page
			// from a last page without a second query.
			if store.gotLimit != tc.wantLimit+1 {
				t.Errorf("store asked for %d rows, want %d (the page plus the probe)", store.gotLimit, tc.wantLimit+1)
			}
		})
	}
}

func TestNextCursorAppearsExactlyWhenThereIsAnotherPage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rows     int
		limit    int
		wantMore bool
	}{
		{name: "a partial page is the last page", rows: 3, limit: 5},
		// The corner a "was the page full?" test gets wrong: exactly a
		// page of work, and nothing after it. A cursor here sends the
		// operator to an empty page that reads as more work.
		{name: "an exactly full page is still the last page", rows: 5, limit: 5},
		{name: "one more row means one more page", rows: 6, limit: 5, wantMore: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := reports(t, tc.rows)
			page := listOK(t, &pageStore{rows: rows}, "limit="+strconv.Itoa(tc.limit))

			wantItems := min(tc.rows, tc.limit)
			if len(page.Items) != wantItems {
				t.Fatalf("items = %d, want %d", len(page.Items), wantItems)
			}
			if (page.NextCursor != nil) != tc.wantMore {
				t.Fatalf("next_cursor = %v, want present = %v", page.NextCursor, tc.wantMore)
			}
		})
	}
}

// TestTheNextCursorResumesAfterTheLastRowRendered follows the cursor round
// trip end to end: the position the endpoint hands out has to be the last
// row the caller actually saw, or the next page repeats work an operator
// has already judged, or skips work nobody will look at again.
func TestTheNextCursorResumesAfterTheLastRowRendered(t *testing.T) {
	t.Parallel()

	rows := reports(t, 6)
	first := listOK(t, &pageStore{rows: rows}, "limit=5")
	if first.NextCursor == nil {
		t.Fatal("next_cursor = null, want a cursor: a sixth row is waiting")
	}

	store := &pageStore{rows: rows[5:]}
	listOK(t, store, "limit=5&cursor="+url.QueryEscape(*first.NextCursor))

	last := rows[4]
	if store.gotAfter.ID != last.ID {
		t.Errorf("resumed after row %s, want the last row rendered, %s", store.gotAfter.ID, last.ID)
	}
	if !store.gotAfter.DetectedAt.Equal(last.DetectedAt) {
		t.Errorf("resumed at %s, want %s", store.gotAfter.DetectedAt, last.DetectedAt)
	}
}

// TestAnAbsentCursorStartsAtTheBeginning pins what the zero position means:
// the first page, not some position in the middle of the queue.
func TestAnAbsentCursorStartsAtTheBeginning(t *testing.T) {
	t.Parallel()

	store := &pageStore{}
	listOK(t, store, "")

	if store.gotAfter != (networks.After{}) {
		t.Errorf("store asked from %+v, want the zero position", store.gotAfter)
	}
}

func TestTheEndpointRefusesAQueryItDidNotPromiseToAnswer(t *testing.T) {
	t.Parallel()

	// A cursor for another list, encoded the way that list encodes them.
	// It decodes as base64 and splits into three parts, so only the tag
	// tells it apart - which is exactly the mistake worth refusing, because
	// answering it would silently page one queue with another's position.
	foreign := "cXVldWV8MjAyNi0wOC0zMFQwOToxNTowMFp8MDE5OGI4YTQtMDAwMC03MDAwLTgwMDAtMDAwMDAwMDAwMDAx"

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{name: "an unknown parameter", query: "state=open", want: "unknown query parameter"},
		{name: "a repeated parameter", query: "limit=10&limit=20", want: "at most once"},
		{name: "a limit of zero", query: "limit=0", want: "between 1 and 100"},
		{name: "a limit past the maximum", query: "limit=101", want: "between 1 and 100"},
		{name: "a limit that is not a number", query: "limit=lots", want: "between 1 and 100"},
		{name: "an empty limit", query: "limit=", want: "between 1 and 100"},
		{name: "a cursor that is not base64", query: "cursor=not!base64", want: "cursor"},
		{name: "a cursor from another list", query: "cursor=" + foreign, want: "cursor"},
		{name: "an oversized cursor", query: "cursor=" + strings.Repeat("a", 257), want: "cursor"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := list(t, unreachableStore{}, tc.query)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if _, detail := problemOf(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

// TestAFailedReadIsOpaque keeps the store's troubles off the wire.
func TestAFailedReadIsOpaque(t *testing.T) {
	t.Parallel()

	const secret = `pq: relation "cashback.unattributed_transaction" does not exist`
	rec := list(t, &pageStore{err: errors.New(secret)}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("the answer leaks the failure: %q", rec.Body.String())
	}
}

// TestTheWrongMethodIsA405WithAnAllowHeader is the classifier reaching a
// path this module really serves - the case a 404 catch-all gets wrong.
func TestTheWrongMethodIsA405WithAnAllowHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodDelete, ops.Prefix+"unattributed", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", allow, "GET, HEAD")
	}
}

// TestEveryRegisteredRouteIsReachable proves Patterns() is not bookkeeping
// that drifted from the mux. A pattern that lost its registration falls to
// the catch-all, which dresses the loss up as an ordinary 404 - so it has
// to be caught by the detail rather than by the status code.
func TestEveryRegisteredRouteIsReachable(t *testing.T) {
	t.Parallel()

	h := ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, stubAuth{op: anOperator})

	for _, pattern := range ops.Patterns() {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("pattern %q is not %q", pattern, "METHOD /path")
			}
			req := httptest.NewRequest(method, strings.ReplaceAll(path, "{id}", uuid.New().String()), strings.NewReader(""))
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			var problem struct {
				Detail string `json:"detail"`
			}
			// A 200 is not problem+json at all, which is itself a pass.
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err == nil {
				if strings.Contains(problem.Detail, "no such endpoint") ||
					strings.Contains(problem.Detail, "is not allowed on this endpoint") {
					t.Fatalf("%s is listed by Patterns() but the catch-all answered it: %q", pattern, problem.Detail)
				}
			}
		})
	}
}

// TestAStoreWithNoDatabaseIsRefusedAtConstruction keeps the failure at the
// composition root rather than in a request. A surface built over nothing
// answers 500 to an operator who is trying to decide money; refusing to
// build it means the process says so at startup instead.
func TestAStoreWithNoDatabaseIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	if _, err := ops.NewPGStore(nil); err == nil {
		t.Fatal("NewPGStore(nil) built a store over no database")
	}
}
