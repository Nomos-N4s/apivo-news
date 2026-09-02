// GET /merchants/{slug} at the HTTP boundary (T104, US5 scenario 3).
//
// Two things are asserted here that no lower test can. What a member's
// client actually receives - the field names, the nulls, and above all that
// the rate on the wire is the member's and not the network's - and the SET
// of statuses this endpoint produces, checked against api/openapi.json in
// both directions, because a status the handler produces and the document
// omits is a client that cannot handle it.

package catalogue_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// aReader is an authenticator that accepts one token and refuses the rest,
// with a third answer for a verifier that is simply broken.
type aReader struct {
	token  string
	broken error
}

func (a aReader) AuthenticateReader(_ context.Context, token string) error {
	switch {
	case a.broken != nil:
		return a.broken
	case token == a.token:
		return nil
	default:
		return catalogue.ErrUnauthenticated
	}
}

const goodToken = "a-token-that-resolves"

// discard is a logger the tests do not read.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// served builds the handler over a staged store and answers one request.
func served(t *testing.T, stub *stubDetailStore, auth aReader, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	handler := catalogue.NewHandler(discard(), stagedPage(t, stub), auth,
		catalogue.WithPageClock(func() time.Time { return detailAt }))
	req := httptest.NewRequest(method, target, nil)
	if auth.token != "" {
		req.Header.Set("Authorization", "Bearer "+auth.token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// merchantURL is where a page is asked for.
func merchantURL(slug, language string) string {
	target := catalogue.MerchantPrefix + "/" + slug
	if language != "" {
		target += "?lang=" + language
	}
	return target
}

// bodyOf decodes a successful response.
func bodyOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response does not parse: %v (%s)", err, rec.Body.String())
	}
	return body
}

// TestAMerchantPageIsServedInTheReadersLanguage covers the ordinary answer:
// the copy, the label that says which language it is in, and a rate that is
// the member's half of the band rather than the network's whole.
func TestAMerchantPageIsServedInTheReadersLanguage(t *testing.T) {
	t.Parallel()
	stub := aStagedMerchant()
	band := aStagedBand()
	band.Conditions = pgtype.Text{String: "on electronics", Valid: true}
	stub.bands = []store.PublishedBandsRow{band}
	closing := aStagedBand()
	closing.ValidTo = pgtype.Timestamptz{Time: detailAt.Add(24 * time.Hour), Valid: true}
	stub.bands = append(stub.bands, closing)

	rec := served(t, stub, aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "el"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	body := bodyOf(t, rec)
	if body["name"] != "Bühne" {
		t.Errorf("name = %v, want the source-language copy", body["name"])
	}
	if body["name_language"] != "de" {
		t.Errorf("name_language = %v, want de", body["name_language"])
	}
	if body["name_is_fallback"] != true {
		t.Error("name_is_fallback is not set, so the page would claim to be in Greek")
	}
	if _, present := body["typical_confirmation_days"]; !present {
		t.Error("typical_confirmation_days is absent; it must be present and null")
	}
	if body["typical_confirmation_days"] != nil {
		t.Errorf("typical_confirmation_days = %v, want null - nothing in the schema records it",
			body["typical_confirmation_days"])
	}

	rates, ok := body["rates"].([]any)
	if !ok || len(rates) != 2 {
		t.Fatalf("rates = %v, want both bands", body["rates"])
	}
	// "Until tomorrow" is part of what a member is being offered, so a band
	// with a published end says when it ends.
	closes, _ := rates[1].(map[string]any)
	if want := detailAt.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano); closes["valid_to"] != want {
		t.Errorf("valid_to = %v, want %s", closes["valid_to"], want)
	}
	rate, _ := rates[0].(map[string]any)
	if rate["kind"] != "percent" {
		t.Errorf("kind = %v, want percent", rate["kind"])
	}
	if rate["bps"] != float64(200) {
		t.Errorf("bps = %v, want 200 - the member's half of a 400 bps commission, not the commission",
			rate["bps"])
	}
	if _, present := rate["amount"]; present {
		t.Errorf("a percent band carries an amount: %v", rate["amount"])
	}
	if rate["conditions"] != "on electronics" {
		t.Errorf("conditions = %v, want the band's own", rate["conditions"])
	}
	if rate["exclusions"] != nil {
		t.Errorf("exclusions = %v, want null for a band that records none", rate["exclusions"])
	}
	if rate["valid_to"] != nil {
		t.Errorf("valid_to = %v, want null for an open-ended band", rate["valid_to"])
	}
}

// TestAFixedBandIsServedAsMoney. A fixed rate is money, so it goes out in
// the shape every money value on this API has - minor units beside an
// explicit currency (C-6) - and never as a bare number beside a percent.
func TestAFixedBandIsServedAsMoney(t *testing.T) {
	t.Parallel()
	stub := aStagedMerchant()
	band := aStagedBand()
	band.RateKind = "fixed"
	band.RateBps = pgtype.Int4{}
	band.RateFixedMinor = pgtype.Int8{Int64: 250, Valid: true}
	band.Currency = pgtype.Text{String: "EUR", Valid: true}
	stub.bands = []store.PublishedBandsRow{band}

	rec := served(t, stub, aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "de"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	rates, _ := bodyOf(t, rec)["rates"].([]any)
	if len(rates) != 1 {
		t.Fatalf("rates = %v, want one band", rates)
	}
	rate, _ := rates[0].(map[string]any)
	if rate["kind"] != "fixed" {
		t.Errorf("kind = %v, want fixed", rate["kind"])
	}
	if _, present := rate["bps"]; present {
		t.Errorf("a fixed band carries bps: %v", rate["bps"])
	}
	amount, ok := rate["amount"].(map[string]any)
	if !ok {
		t.Fatalf("amount = %v, want an object", rate["amount"])
	}
	if amount["minor"] != float64(125) || amount["currency"] != "EUR" {
		t.Errorf("amount = %v, want 125 EUR - the member's half of 250", amount)
	}
}

// TestARetailerWithNoRatesTodayIsStillAPage. The empty list is the answer,
// and it is an empty list rather than null so a client renders "no rates
// today" without telling null from absent.
func TestARetailerWithNoRatesTodayIsStillAPage(t *testing.T) {
	t.Parallel()

	rec := served(t, aStagedMerchant(), aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "de"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rates, ok := bodyOf(t, rec)["rates"].([]any)
	if !ok {
		t.Fatalf("rates = %v, want an empty list rather than null", bodyOf(t, rec)["rates"])
	}
	if len(rates) != 0 {
		t.Errorf("rates = %v, want none", rates)
	}
}

// TestAnUnknownAddressIs404, and a nameless retailer is deliberately NOT:
// one is a page that does not exist and the other is a row that is broken,
// and answering both 404 would retire the second into a number nobody
// investigates.
func TestAnUnknownAddressIs404AndABrokenRowIsNot(t *testing.T) {
	t.Parallel()

	missing := aStagedMerchant()
	missing.merchantErr = pgx.ErrNoRows
	if rec := served(t, missing, aReader{token: goodToken}, http.MethodGet, merchantURL("gone", "de")); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown address = %d, want 404", rec.Code)
	}

	nameless := aStagedMerchant()
	nameless.copies = nil
	if rec := served(t, nameless, aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "de")); rec.Code != http.StatusInternalServerError {
		t.Errorf("a retailer with no name in any language = %d, want 500", rec.Code)
	}
}

// TestTheCatalogueIsNotAnAnonymousSurface. FR-023: there is no anonymous
// cashback surface, and a rate card readable without a token is a rate card
// published to anyone who finds the path.
func TestTheCatalogueIsNotAnAnonymousSurface(t *testing.T) {
	t.Parallel()

	for name, auth := range map[string]aReader{
		"no token at all":   {},
		"a token nobody is": {token: "not-the-one"},
	} {
		stub := aStagedMerchant()
		handler := catalogue.NewHandler(discard(), stagedPage(t, stub), aReader{token: goodToken})
		req := httptest.NewRequest(http.MethodGet, merchantURL("staged", "de"), nil)
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

// TestAVerifierThatIsBrokenIsNotARefusal. A token that could not be checked
// is not a token that was rejected: answering 401 would tell a member their
// session had expired because a key server was down.
func TestAVerifierThatIsBrokenIsNotARefusal(t *testing.T) {
	t.Parallel()

	rec := served(t, aStagedMerchant(),
		aReader{token: goodToken, broken: errors.New("the key server is not answering")},
		http.MethodGet, merchantURL("staged", "de"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a broken verifier = %d, want 500", rec.Code)
	}
}

// TestAFailedReadIsNotAMissingPage. A page that could not be read is not a
// page that does not exist.
func TestAFailedReadIsNotAMissingPage(t *testing.T) {
	t.Parallel()
	stub := aStagedMerchant()
	stub.bandErr = errors.New("the database is not answering")

	rec := served(t, stub, aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "de"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed read = %d, want 500", rec.Code)
	}
}

// TestAWrongMethodIsRefusedWithAllow. The error convention holds in the
// corners: everything under this path answers in problem+json, and a known
// path reached the wrong way says which way is right.
func TestAWrongMethodIsRefusedWithAllow(t *testing.T) {
	t.Parallel()

	rec := served(t, aStagedMerchant(), aReader{token: goodToken}, http.MethodPost, merchantURL("staged", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", allow)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want problem+json", got)
	}

	// And a path under this tree that no route claims is an ordinary 404 in
	// the same convention, rather than the router's own text/plain.
	deeper := served(t, aStagedMerchant(), aReader{token: goodToken},
		http.MethodGet, catalogue.MerchantPrefix+"/staged/reviews")
	if deeper.Code != http.StatusNotFound {
		t.Errorf("an unrouted sub-path = %d, want 404", deeper.Code)
	}
	if got := deeper.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("an unrouted sub-path Content-Type = %q, want problem+json", got)
	}
}

// TestPatternsMatchTheRoutesRegistered. The list the OpenAPI drift gate
// reads must be the routes the mux actually serves.
func TestPatternsMatchTheRoutesRegistered(t *testing.T) {
	t.Parallel()

	want := []string{"GET " + catalogue.MerchantPrefix + "/{slug}"}
	if got := catalogue.Patterns(); !slices.Equal(got, want) {
		t.Errorf("Patterns() = %v, want %v", got, want)
	}
}

// documentedStatuses reads what api/openapi.json promises for an operation.
// Read from the file rather than restated here, because a copy is the drift
// this test exists to catch.
func documentedStatuses(t *testing.T, operationID string) []int {
	t.Helper()
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}
	for _, methods := range doc.Paths {
		for _, raw := range methods {
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(raw, &op); err != nil || op.OperationID != operationID {
				continue
			}
			codes := make([]int, 0, len(op.Responses))
			for code := range op.Responses {
				n, err := strconv.Atoi(code)
				if err != nil {
					t.Fatalf("the contract declares a non-numeric response %q for %s", code, operationID)
				}
				codes = append(codes, n)
			}
			sort.Ints(codes)
			return codes
		}
	}
	t.Fatalf("the contract describes no operation %s", operationID)
	return nil
}

// TestTheMerchantPageProducesExactlyTheDocumentedStatuses asserts the SET,
// which is the thing no behaviour test can: a status added to the handler
// and not to the document is a client that cannot handle it, and one left in
// the document that the handler never produces is a client handling
// something that will never arrive. Both drift silently, because every
// individual test still passes.
func TestTheMerchantPageProducesExactlyTheDocumentedStatuses(t *testing.T) {
	t.Parallel()

	missing := aStagedMerchant()
	missing.merchantErr = pgx.ErrNoRows
	broken := aStagedMerchant()
	broken.bandErr = errors.New("the database is not answering")

	produced := map[int]bool{}
	for _, one := range []struct {
		stub *stubDetailStore
		auth aReader
	}{
		{aStagedMerchant(), aReader{token: goodToken}},
		{aStagedMerchant(), aReader{token: ""}},
		{missing, aReader{token: goodToken}},
		{broken, aReader{token: goodToken}},
	} {
		produced[served(t, one.stub, one.auth, http.MethodGet, merchantURL("staged", "de")).Code] = true
	}

	got := make([]int, 0, len(produced))
	for code := range produced {
		got = append(got, code)
	}
	sort.Ints(got)

	if want := documentedStatuses(t, "getMerchant"); !slices.Equal(got, want) {
		t.Errorf("the endpoint produces %v and the document declares %v", got, want)
	}
}

// stagedMerchantID is the id the staged store answers with, so a case can
// assert the page names the retailer it read.
func TestThePageNamesTheRetailerItRead(t *testing.T) {
	t.Parallel()
	stub := aStagedMerchant()

	rec := served(t, stub, aReader{token: goodToken}, http.MethodGet, merchantURL("staged", "de"))
	body := bodyOf(t, rec)
	if body["slug"] != "staged" {
		t.Errorf("slug = %v, want staged", body["slug"])
	}
	want := uuid.UUID(stub.merchant.ID.Bytes).String()
	if body["merchant_id"] != want {
		t.Errorf("merchant_id = %v, want %s", body["merchant_id"], want)
	}
}
