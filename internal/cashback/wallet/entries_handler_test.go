package wallet_test

// What GET /wallet/entries answers (T079, US3).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// entriesRequest is a GET on the history with the given query string.
func entriesRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	target := wallet.Prefix + "/entries"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer t")
	return req
}

// serveEntries answers one history request over the given reader.
func serveEntries(t *testing.T, entries wallet.EntryReader, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t,
		aWallet(t, money.Amount{Minor: 2000, Currency: "EUR"}),
		history(t, entries),
		fakeAuth{token: "t", member: fakeMember},
		req)
}

func page(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
} {
	t.Helper()
	var out struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	raw, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the body is not a page: %v (%s)", err, raw)
	}
	return out
}

// TestTheHistoryIsAnsweredAsTheContractDescribesIt.
func TestTheHistoryIsAnsweredAsTheContractDescribesIt(t *testing.T) {
	t.Parallel()

	row := aRow(listedAt, "held", 150)
	row.NameInLanguageAsked = pgtype.Text{String: "Πολυκατάστημα", Valid: true}
	row.HoldRule = pgtype.Text{String: "new-member-first-purchase", Valid: true}
	row.Reason = pgtype.Text{String: "the network reported it", Valid: true}

	rec := serveEntries(t, &fakeEntries{rows: []store.MemberEntriesRow{row}}, entriesRequest(t, "lang=el"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	got := page(t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("returned %d items, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item["state"] != "held" {
		t.Errorf("state = %v, want held", item["state"])
	}
	if item["merchant_name"] != "Πολυκατάστημα" {
		t.Errorf("merchant_name = %v, want the Greek copy", item["merchant_name"])
	}
	if item["merchant_name_language"] != "el" || item["merchant_name_is_fallback"] != false {
		t.Errorf("the name came back as %v/%v, want el and not a fallback",
			item["merchant_name_language"], item["merchant_name_is_fallback"])
	}
	// A member looking at money they cannot count is owed the reason: the
	// totals do not show held money at all.
	if item["hold_rule"] != "new-member-first-purchase" {
		t.Errorf("hold_rule = %v, want the rule holding it", item["hold_rule"])
	}
	for _, field := range []string{"sale_amount", "cashback_amount"} {
		figure, shaped := item[field].(map[string]any)
		if !shaped || figure["currency"] != "EUR" {
			t.Errorf("%s = %#v, want an object naming its currency", field, item[field])
		}
	}
	if item["transacted_at"] != listedAt.Add(-24*time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("transacted_at = %v, want the instant the network reported", item["transacted_at"])
	}
	// Nothing in the schema records a confirmation window, so there is no
	// honest value - and a plausible one invented here is a date a member
	// would plan around.
	if item["expected_confirmation_at"] != nil {
		t.Errorf("expected_confirmation_at = %v, want null until a window exists to compute it from", item["expected_confirmation_at"])
	}
	if got.NextCursor != nil {
		t.Errorf("next_cursor = %v on a single-item page, want null", *got.NextCursor)
	}
}

// TestAReversalIsListedWithWhatItUndoes. US3 scenario 2: both halves, and
// the reason the money went back.
func TestAReversalIsListedWithWhatItUndoes(t *testing.T) {
	t.Parallel()

	original := aRow(listedAt.Add(-time.Hour), "confirmed", 150)
	reversal := aRow(listedAt, "reversed", 150)
	reversal.ReversalOfID = original.ID
	reversal.Reason = pgtype.Text{String: "the network took it back", Valid: true}

	rec := serveEntries(t, &fakeEntries{rows: []store.MemberEntriesRow{reversal, original}}, entriesRequest(t, ""))

	got := page(t, rec)
	if len(got.Items) != 2 {
		t.Fatalf("returned %d items, want both halves of the pair", len(got.Items))
	}
	if got.Items[0]["reversal_of_id"] != uuid.UUID(original.ID.Bytes).String() {
		t.Errorf("the reversal cites %v, want the credit it undoes", got.Items[0]["reversal_of_id"])
	}
	if got.Items[0]["reason"] != "the network took it back" {
		t.Errorf("reason = %v, want why the money went back", got.Items[0]["reason"])
	}
	// The credit itself undoes nothing, and says so rather than repeating
	// the reversal's own citation.
	if got.Items[1]["reversal_of_id"] != nil {
		t.Errorf("the original cites %v, want null", got.Items[1]["reversal_of_id"])
	}
}

// TestAMalformedQueryIsRefused. A misspelled filter silently dropped would
// answer with every entry, and a member who asked to see only their reversed
// ones would read the whole list as reversals.
func TestAMalformedQueryIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"a parameter this endpoint does not serve", "status=confirmed"},
		{"a limit that is not a number", "limit=lots"},
		{"a limit of nothing", "limit=0"},
		{"a negative limit", "limit=-5"},
		{"a state from another machine entirely", "state=awaiting_approval"},
		{"a cursor from somewhere else", "cursor=not-a-cursor!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := &fakeEntries{}
			rec := serveEntries(t, entries, entriesRequest(t, tc.query))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", got)
			}
		})
	}
}

// TestAPageCarriesItsCursorWhenThereIsMore, so a client knows to ask again.
func TestAPageCarriesItsCursorWhenThereIsMore(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{}
	for i := range 4 {
		entries.rows = append(entries.rows, aRow(listedAt.Add(-time.Duration(i)*time.Hour), "confirmed", 100))
	}

	rec := serveEntries(t, entries, entriesRequest(t, "limit=2"))

	got := page(t, rec)
	if len(got.Items) != 2 {
		t.Fatalf("returned %d items, want the 2 asked for", len(got.Items))
	}
	if got.NextCursor == nil || *got.NextCursor == "" {
		t.Fatal("the page carries no cursor although more entries exist")
	}
	// And the cursor it gave back is one it will accept.
	again := serveEntries(t, entries, entriesRequest(t, "limit=2&cursor="+*got.NextCursor))
	if again.Code != http.StatusOK {
		t.Fatalf("replaying the cursor answered %d, want 200 (%s)", again.Code, again.Body)
	}
}

// TestAMemberWithNoHistoryGetsAnEmptyList, not a null one: a client
// iterating the items must not have to check for it first.
func TestAMemberWithNoHistoryGetsAnEmptyList(t *testing.T) {
	t.Parallel()

	rec := serveEntries(t, &fakeEntries{}, entriesRequest(t, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"items":[]`) {
		t.Errorf("body = %s, want an empty items array", body)
	}
}
