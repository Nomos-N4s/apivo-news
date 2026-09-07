// GET /catalogue at the HTTP boundary (#545, US5 scenario 1).
//
// What a member's client actually receives - the card shape, the empty
// list rather than null, the cursor that is null on the last page, and
// above all a rate that is the member's - and the SET of statuses the
// endpoint produces, checked against api/openapi.json in both directions.

package catalogue_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// servedList builds the handler over a staged shelf and answers one request.
func servedList(t *testing.T, stub *stubListStore, auth aReader, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	handler := catalogue.NewHandler(discard(), stagedPage(t, aStagedMerchant()), stagedShelf(t, stub), auth,
		catalogue.WithPageClock(func() time.Time { return shelfAt }))
	req := httptest.NewRequest(method, target, nil)
	if auth.token != "" {
		req.Header.Set("Authorization", "Bearer "+auth.token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// catalogueURL is where a page of the shelf is asked for.
func catalogueURL(query url.Values) string {
	if len(query) == 0 {
		return catalogue.CataloguePrefix
	}
	return catalogue.CataloguePrefix + "?" + query.Encode()
}

// aShelfQuery is the ordinary request: Greek, one place.
func aShelfQuery() url.Values {
	return url.Values{"lang": {"el"}, "place": {"munich"}}
}

// itemsOf reads the items off a page body.
func itemsOf(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	raw, ok := bodyOf(t, rec)["items"].([]any)
	if !ok {
		t.Fatalf("items = %v, want a list", bodyOf(t, rec)["items"])
	}
	items := make([]map[string]any, 0, len(raw))
	for _, one := range raw {
		item, _ := one.(map[string]any)
		items = append(items, item)
	}
	return items
}

// TestTheShelfIsServedInTheReadersLanguage covers the ordinary answer: the
// cards in slug order, each labelled with the language it is actually in,
// and a rate that is the member's half of the band rather than the whole.
func TestTheShelfIsServedInTheReadersLanguage(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	alpha := stub.merchant("alpha")
	stub.bands = []store.PublishedBandsForMerchantsRow{{
		MerchantID: alpha.ID, ID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		RateKind: "percent", RateBps: pgtype.Int4{Int32: 400, Valid: true}, MemberShareBps: 5000,
		Conditions: pgtype.Text{String: "on electronics", Valid: true},
		ValidFrom:  pgtype.Timestamptz{Time: shelfAt.Add(-time.Hour), Valid: true},
	}}

	rec := servedList(t, stub, aReader{token: goodToken}, http.MethodGet, catalogueURL(aShelfQuery()))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	items := itemsOf(t, rec)
	slugs := make([]string, 0, len(items))
	for _, item := range items {
		slugs = append(slugs, item["slug"].(string))
	}
	if want := []string{"alpha", "bravo", "charlie", "delta", "echo"}; !slices.Equal(slugs, want) {
		t.Fatalf("slugs = %v, want %v, in slug order whatever order the store answered in", slugs, want)
	}
	if next, present := bodyOf(t, rec)["next_cursor"]; !present || next != nil {
		t.Errorf("next_cursor = %v, want present and null on the last page", next)
	}

	first := items[0]
	if first["merchant_id"] != uuid.UUID(alpha.ID.Bytes).String() {
		t.Errorf("merchant_id = %v, want alpha's", first["merchant_id"])
	}
	if first["name"] != "Κατάστημα alpha" || first["name_language"] != "el" || first["name_is_fallback"] != false {
		t.Errorf("alpha = %v, want the Greek copy, labelled as Greek and not a fallback", first)
	}
	if summary, present := first["summary"]; !present || summary != nil {
		t.Errorf("summary = %v, want present and null when the copy has none", summary)
	}
	second := items[1]
	if second["name"] != "Laden bravo" || second["name_language"] != "de" || second["name_is_fallback"] != true {
		t.Errorf("bravo = %v, want the German copy, labelled as a fallback", second)
	}

	rates, ok := first["rates"].([]any)
	if !ok || len(rates) != 1 {
		t.Fatalf("alpha's rates = %v, want the one band", first["rates"])
	}
	rate, _ := rates[0].(map[string]any)
	if rate["kind"] != "percent" || rate["bps"] != float64(200) {
		t.Errorf("rate = %v, want 200 bps - the member's half of a 400 bps commission", rate)
	}
	if rate["conditions"] != "on electronics" {
		t.Errorf("conditions = %v, want the band's own", rate["conditions"])
	}
	// A retailer with no band today is an empty list, never null, so a
	// client renders "no rates today" without telling null from absent.
	if none, ok := second["rates"].([]any); !ok || len(none) != 0 {
		t.Errorf("bravo's rates = %v, want an empty list", second["rates"])
	}
}

// TestTheShelfPagesThroughItsOwnCursor. The cursor the page hands out is
// the one the next request takes, and the next page starts where the last
// one stopped.
func TestTheShelfPagesThroughItsOwnCursor(t *testing.T) {
	t.Parallel()
	query := aShelfQuery()
	query.Set("lang", "de")
	query.Set("limit", "2")

	first := servedList(t, aStagedShelf(), aReader{token: goodToken}, http.MethodGet, catalogueURL(query))
	if first.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", first.Code, first.Body.String())
	}
	next, ok := bodyOf(t, first)["next_cursor"].(string)
	if !ok || next == "" {
		t.Fatalf("next_cursor = %v, want a cursor when more remains", bodyOf(t, first)["next_cursor"])
	}
	if got := len(itemsOf(t, first)); got != 2 {
		t.Errorf("the first page holds %d, want 2", got)
	}

	query.Set("cursor", next)
	second := servedList(t, aStagedShelf(), aReader{token: goodToken}, http.MethodGet, catalogueURL(query))
	if second.Code != http.StatusOK {
		t.Fatalf("the second page = %d, want 200 (%s)", second.Code, second.Body.String())
	}
	if items := itemsOf(t, second); len(items) != 2 || items[0]["slug"] != "charlie" || items[1]["slug"] != "delta" {
		t.Errorf("the second page = %v, want charlie and delta", items)
	}
}

// TestAnEmptyShelfIsAnEmptyList. A place that holds no retailer yet is an
// ordinary answer: 200 with nothing on it, never a 404 and never null.
func TestAnEmptyShelfIsAnEmptyList(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	stub.merchants = nil

	rec := servedList(t, stub, aReader{token: goodToken}, http.MethodGet, catalogueURL(aShelfQuery()))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if items := itemsOf(t, rec); len(items) != 0 {
		t.Errorf("items = %v, want none", items)
	}
}

// TestTheShelfRefusesWhatItCannotServe. Every refusal is a 400 in
// problem+json that says what to correct, and the one that names a slug
// the reader typed names it.
func TestTheShelfRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(url.Values)
		detail string
	}{
		"a category":         {func(q url.Values) { q.Set("category", "outdoor") }, "category"},
		"a limit of zero":    {func(q url.Values) { q.Set("limit", "0") }, "limit"},
		"a limit in words":   {func(q url.Values) { q.Set("limit", "ten") }, "limit"},
		"no language":        {func(q url.Values) { q.Del("lang") }, "lang"},
		"a language that is": {func(q url.Values) { q.Set("lang", "greek") }, "lang"},
		"no place":           {func(q url.Values) { q.Del("place") }, "place"},
		"an unknown place":   {func(q url.Values) { q.Add("place", "atlantis") }, "atlantis"},
		"a foreign cursor":   {func(q url.Values) { q.Set("cursor", "bm90LW1pbmU") }, "cursor"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			query := aShelfQuery()
			tc.change(query)
			rec := servedList(t, aStagedShelf(), aReader{token: goodToken}, http.MethodGet, catalogueURL(query))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q, want problem+json", got)
			}
			if !strings.Contains(rec.Body.String(), tc.detail) {
				t.Errorf("the refusal does not say what to correct: %s", rec.Body.String())
			}
		})
	}
}

// TestTheShelfIsNotAnAnonymousSurface. FR-023 holds for the listing as it
// holds for the page: a shelf of rates readable without a token is a rate
// card published to anyone who finds the path.
func TestTheShelfIsNotAnAnonymousSurface(t *testing.T) {
	t.Parallel()
	for name, auth := range map[string]aReader{
		"no token at all":   {},
		"a token nobody is": {token: "not-the-one"},
	} {
		handler := catalogue.NewHandler(discard(), stagedPage(t, aStagedMerchant()), stagedShelf(t, aStagedShelf()), aReader{token: goodToken})
		req := httptest.NewRequest(http.MethodGet, catalogueURL(aShelfQuery()), nil)
		if auth.token != "" {
			req.Header.Set("Authorization", "Bearer "+auth.token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate header, which HTTP requires on a 401", name)
		}
	}
}

// TestAFailedReadFailsTheShelf. A shelf that could not be read is a 500,
// not an empty shelf: "nothing here yet" over a database that is down is
// a lie a member acts on.
func TestAFailedReadFailsTheShelf(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	stub.bandErr = errors.New("the database is not answering")

	rec := servedList(t, stub, aReader{token: goodToken}, http.MethodGet, catalogueURL(aShelfQuery()))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed read = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want problem+json", got)
	}
}

// TestAWrongMethodOnTheShelfIsRefusedWithAllow. The error convention holds
// at the listing as at the page: the wrong method says which is right, and
// a sub-path nobody serves is a 404 in problem+json rather than the
// router's own text/plain.
func TestAWrongMethodOnTheShelfIsRefusedWithAllow(t *testing.T) {
	t.Parallel()

	rec := servedList(t, aStagedShelf(), aReader{token: goodToken}, http.MethodPost, catalogueURL(nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", allow)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want problem+json", got)
	}

	deeper := servedList(t, aStagedShelf(), aReader{token: goodToken}, http.MethodGet, catalogue.CataloguePrefix+"/categories")
	if deeper.Code != http.StatusNotFound {
		t.Errorf("an unrouted sub-path = %d, want 404", deeper.Code)
	}
	if got := deeper.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("an unrouted sub-path Content-Type = %q, want problem+json", got)
	}
}

// TestTheShelfProducesExactlyTheDocumentedStatuses asserts the SET, which
// no behaviour test can: a status the handler produces and the document
// omits is a client that cannot handle it, and one the document declares
// and the handler never produces is a client handling something that will
// never arrive.
func TestTheShelfProducesExactlyTheDocumentedStatuses(t *testing.T) {
	t.Parallel()

	broken := aStagedShelf()
	broken.merchantErr = errors.New("the database is not answering")
	unknown := aShelfQuery()
	unknown.Set("place", "atlantis")

	produced := map[int]bool{}
	for _, one := range []struct {
		stub  *stubListStore
		auth  aReader
		query url.Values
	}{
		{aStagedShelf(), aReader{token: goodToken}, aShelfQuery()},
		{aStagedShelf(), aReader{token: ""}, aShelfQuery()},
		{aStagedShelf(), aReader{token: goodToken}, unknown},
		{broken, aReader{token: goodToken}, aShelfQuery()},
	} {
		produced[servedList(t, one.stub, one.auth, http.MethodGet, catalogueURL(one.query)).Code] = true
	}

	got := make([]int, 0, len(produced))
	for code := range produced {
		got = append(got, code)
	}
	sort.Ints(got)

	if want := documentedStatuses(t, "listCatalogue"); !slices.Equal(got, want) {
		t.Errorf("the endpoint produces %v and the document declares %v", got, want)
	}
}
