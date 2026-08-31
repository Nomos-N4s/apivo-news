package wallet_test

// What the export endpoint sends, in both renderings (T081, FR-003).
//
// The interesting cases are the ones about the DOCUMENT rather than the
// data: that the two renderings agree, that a spreadsheet cannot be made to
// execute a merchant's name, and that a request for part of an export is
// refused rather than quietly answered with all of it.

import (
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// exportHandler builds the module's handler over an exporter and a member
// the token resolves to.
func exportHandler(t *testing.T, entries wallet.EntryReader) http.Handler {
	t.Helper()
	return wallet.NewHandler(slog.New(slog.DiscardHandler), nil, nil, nil,
		exporter(t, entries), fakeAuth{token: "a-token", member: uuid.New()})
}

// exportRequest is one authenticated GET with the given query string.
func exportRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	path := wallet.ExportPrefix
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer a-token")
	return req
}

// exportedCSV parses the response as a spreadsheet would.
func exportedCSV(t *testing.T, rec *httptest.ResponseRecorder) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the body is not CSV: %v (%s)", err, rec.Body)
	}
	return rows
}

func TestTheJSONExportCarriesEveryEntryAndSaysHowMany(t *testing.T) {
	t.Parallel()
	const total = wallet.MaxPageSize + 3
	rec := serveHandler(t, exportHandler(t, aHistory(total)), exportRequest(t, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		ExportedAt string           `json:"exported_at"`
		EntryCount int              `json:"entry_count"`
		Entries    []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("the body is not an export: %v", err)
	}
	if len(got.Entries) != total {
		t.Errorf("the export holds %d entries, want all %d", len(got.Entries), total)
	}
	// The count is what lets a reader DETECT a truncated file, so it has to
	// agree with the array rather than be computed from it.
	if got.EntryCount != len(got.Entries) {
		t.Errorf("entry_count is %d beside %d entries", got.EntryCount, len(got.Entries))
	}
	taken, err := time.Parse(time.RFC3339Nano, got.ExportedAt)
	if err != nil {
		t.Fatalf("exported_at %q is not a timestamp: %v", got.ExportedAt, err)
	}
	if time.Since(taken) > time.Minute {
		t.Errorf("exported_at is %v, which is not when this was taken", taken)
	}
}

// A browser has to be told to save it, and the filename carries neither the
// brand nor the member: it lands in a downloads folder, gets mailed on, and
// ends up in a support ticket.
func TestBothRenderingsAreSentAsAttachments(t *testing.T) {
	t.Parallel()
	for query, want := range map[string]string{
		"":            "cashback-history.json",
		"format=json": "cashback-history.json",
		"format=csv":  "cashback-history.csv",
	} {
		rec := serveHandler(t, exportHandler(t, aHistory(1)), exportRequest(t, query))
		disposition := rec.Header().Get("Content-Disposition")
		if !strings.Contains(disposition, want) {
			t.Errorf("%q sent Content-Disposition %q, want it to name %s", query, disposition, want)
		}
	}
}

func TestTheCSVExportHasAHeaderAndARowPerEntry(t *testing.T) {
	t.Parallel()
	const total = wallet.MaxPageSize + 3
	rec := serveHandler(t, exportHandler(t, aHistory(total)), exportRequest(t, "format=csv"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", kind)
	}
	rows := exportedCSV(t, rec)
	if len(rows) != total+1 {
		t.Fatalf("the file holds %d rows, want a header and %d entries", len(rows), total)
	}
	// Every money figure is two columns: C-6 holds in a spreadsheet exactly
	// as on the wire, and one column of bare integers is a column somebody
	// sums across currencies.
	for _, pair := range [][2]string{
		{"sale_amount_minor", "sale_currency"},
		{"cashback_amount_minor", "cashback_currency"},
	} {
		for _, column := range pair {
			if !slices.Contains(rows[0], column) {
				t.Errorf("the header has no %s column: %v", column, rows[0])
			}
		}
	}
	if len(rows[1]) != len(rows[0]) {
		t.Errorf("a row has %d cells against %d headers", len(rows[1]), len(rows[0]))
	}
}

// The two renderings must describe the same history. One export answered
// two ways that disagreed would be worse than either alone.
func TestTheTwoRenderingsAgree(t *testing.T) {
	t.Parallel()
	const total = 5

	asJSON := serveHandler(t, exportHandler(t, aHistory(total)), exportRequest(t, ""))
	var document struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(asJSON.Body.Bytes(), &document); err != nil {
		t.Fatalf("the JSON body is not an export: %v", err)
	}
	rows := exportedCSV(t, serveHandler(t, exportHandler(t, aHistory(total)), exportRequest(t, "format=csv")))

	if len(rows)-1 != len(document.Entries) {
		t.Fatalf("the CSV holds %d entries and the JSON %d", len(rows)-1, len(document.Entries))
	}
	// Same order, newest first, in both.
	state := slices.Index(rows[0], "state")
	for i, entry := range document.Entries {
		if rows[i+1][state] != entry["state"] {
			t.Errorf("entry %d is %v in the JSON and %q in the CSV", i, entry["state"], rows[i+1][state])
		}
	}
}

// A merchant name comes from an affiliate network's catalogue and a reason
// is typed by an operator: neither is text this process wrote. A cell
// beginning =, +, -, @ or a control character is a FORMULA to Excel,
// LibreOffice and Sheets, which is how a name becomes something a member
// runs in a file their own wallet handed them.
func TestTheCSVDoesNotHandASpreadsheetAFormula(t *testing.T) {
	t.Parallel()
	for _, hostile := range []string{
		`=HYPERLINK("https://evil.test","Click for your refund")`,
		`+1+1`,
		`-2+3`,
		`@SUM(A1:A9)`,
		"\t=1+1",
	} {
		row := aRow(listedAt, "confirmed", 100)
		row.NameInLanguageAsked = pgtype.Text{String: hostile, Valid: true}
		rec := serveHandler(t, exportHandler(t, &pagingEntries{rows: []store.MemberEntriesRow{row}}),
			exportRequest(t, "format=csv"))

		rows := exportedCSV(t, rec)
		if len(rows) != 2 {
			t.Fatalf("the file holds %d rows, want a header and one entry", len(rows))
		}
		name := rows[1][slices.Index(rows[0], "merchant_name")]
		if !strings.HasPrefix(name, "'") {
			t.Errorf("%q went into the file as %q; a spreadsheet would run it", hostile, name)
		}
		// Neutralised, not mangled: the name is still readable.
		if strings.TrimPrefix(name, "'") != hostile {
			t.Errorf("the name was altered beyond the leading quote: %q", name)
		}
	}
}

// An ordinary name is untouched. A defence that quoted every cell would
// make every export slightly wrong to read.
func TestAnOrdinaryNameIsWrittenAsItIs(t *testing.T) {
	t.Parallel()
	row := aRow(listedAt, "confirmed", 100)
	row.NameInLanguageAsked = pgtype.Text{String: "Kaufhaus Süd", Valid: true}

	rows := exportedCSV(t, serveHandler(t, exportHandler(t, &pagingEntries{rows: []store.MemberEntriesRow{row}}),
		exportRequest(t, "format=csv")))
	if got := rows[1][slices.Index(rows[0], "merchant_name")]; got != "Kaufhaus Süd" {
		t.Errorf("merchant_name = %q, want the name as it is", got)
	}
}

// An export is the complete record. A client that asked for part of one and
// silently got all of it - or thought it had asked for all and got part -
// would hand a member a document that is not what they think it is.
func TestTheExportRefusesToBeNarrowed(t *testing.T) {
	t.Parallel()
	handler := exportHandler(t, aHistory(3))

	for _, query := range []string{"state=confirmed", "limit=10", "cursor=abc"} {
		rec := serveHandler(t, handler, exportRequest(t, query))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q answered %d, want 400", query, rec.Code)
			continue
		}
		detail, _ := decoded(t, rec)["detail"].(string)
		if !strings.Contains(detail, "complete") {
			t.Errorf("%q was refused with %q, which does not say why", query, detail)
		}
	}
}

func TestTheExportRefusesWhatItDoesNotServe(t *testing.T) {
	t.Parallel()
	handler := exportHandler(t, aHistory(3))

	// format=JSON is refused rather than upcased, for the reason
	// money.ParseCurrency refuses a lowercase code: a value in the wrong
	// case means some caller assembled it by hand, and that is worth seeing.
	for _, query := range []string{"format=xlsx", "lnag=de", "format=JSON", "format=csv&format=xlsx"} {
		if rec := serveHandler(t, handler, exportRequest(t, query)); rec.Code != http.StatusBadRequest {
			t.Errorf("%q answered %d, want 400", query, rec.Code)
		}
	}
}

// An empty value is an absent one, which is what every other parameter on
// this module's surface already means by it: parseEntriesPage reads an
// empty state, lang and limit the same way. One convention across the
// surface beats a stricter rule on one parameter.
func TestAnEmptyFormatIsTheDefaultOne(t *testing.T) {
	t.Parallel()

	rec := serveHandler(t, exportHandler(t, aHistory(1)), exportRequest(t, "format=&lang="))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if kind := rec.Header().Get("Content-Type"); !strings.HasPrefix(kind, "application/json") {
		t.Errorf("Content-Type = %q, want the default JSON", kind)
	}
}

// A history above the bound is refused with 413, not 500 and not a shorter
// file: nothing is broken, and the difference matters to whoever is paged.
func TestAHistoryTooLargeToExportAnswers413(t *testing.T) {
	t.Parallel()
	rec := serveHandler(t, exportHandler(t, &endlessEntries{}), exportRequest(t, ""))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body)
	}
	if detail, _ := decoded(t, rec)["detail"].(string); !strings.Contains(detail, "too large") {
		t.Errorf("the refusal %q does not say what happened", detail)
	}
}

// The export path is behind the same gate as the rest of the module's, and
// the gate wraps the whole mux rather than each route.
func TestTheExportPathIsBehindTheAuthGate(t *testing.T) {
	t.Parallel()
	handler := exportHandler(t, aHistory(1))

	rec := serveHandler(t, handler, httptest.NewRequest(http.MethodGet, wallet.ExportPrefix, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated export answered %d, want 401", rec.Code)
	}
}

func TestTheExportPathAnswersForWhatItDoesNotServe(t *testing.T) {
	t.Parallel()
	handler := exportHandler(t, aHistory(1))

	req := httptest.NewRequest(http.MethodPost, wallet.ExportPrefix, nil)
	req.Header.Set("Authorization", "Bearer a-token")
	rec := serveHandler(t, handler, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST answered %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow %q does not name GET", allow)
	}

	stray := httptest.NewRequest(http.MethodGet, wallet.ExportPrefix+"/nowhere", nil)
	stray.Header.Set("Authorization", "Bearer a-token")
	if rec := serveHandler(t, handler, stray); rec.Code != http.StatusNotFound {
		t.Errorf("a stray sub-path answered %d, want 404", rec.Code)
	}
}

// A member who has earned nothing gets a document saying so, in both
// renderings: an empty record is still a record.
func TestAnEmptyHistoryExportsAnEmptyDocument(t *testing.T) {
	t.Parallel()

	rec := serveHandler(t, exportHandler(t, &pagingEntries{}), exportRequest(t, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"entry_count":0`) {
		t.Errorf("the export does not say it is empty: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("entries is not an empty array: %s", rec.Body)
	}

	rows := exportedCSV(t, serveHandler(t, exportHandler(t, &pagingEntries{}), exportRequest(t, "format=csv")))
	if len(rows) != 1 {
		t.Errorf("the empty CSV holds %d rows, want the header alone", len(rows))
	}
}
