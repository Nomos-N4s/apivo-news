// The tests for handlers.go: what each refusal answers, and what a client
// can act on without reading prose (T091, T099).
//
// They run against the same scratch database the service tests do, because
// the answers under test are the ones the real checks produce - a fake
// service would be a second statement of the mapping, agreeing with itself.

package payout_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// stubAuth resolves one token to one member and refuses everything else.
type stubAuth struct {
	token  string
	member uuid.UUID
	err    error
}

func (s stubAuth) AuthenticateMember(_ context.Context, token string) (payout.Member, error) {
	if s.err != nil {
		return payout.Member{}, s.err
	}
	if token != s.token {
		return payout.Member{}, payout.ErrUnauthenticated
	}
	return payout.Member{ID: s.member}, nil
}

// discardLogger keeps the expected errors out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// handlerFor builds the endpoint over a fixture, authenticating that
// fixture's member as "good-token".
func handlerFor(t *testing.T, f fixture) http.Handler {
	t.Helper()
	h, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, &stubVault{}, stubAuth{token: "good-token", member: f.member})
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	return h
}

// post sends a withdrawal request body and returns the response.
func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, payout.Prefix, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// problem decodes a problem+json body, failing the case when the response is
// not one.
func problem(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json; body: %s", got, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestAWithdrawalRequestAnswersWhatWasReserved(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 1000, 2000)
	h := handlerFor(t, f)

	rec := post(t, h, "good-token",
		`{"destination_id":"`+f.destination.String()+`","amount":{"minor":1500,"currency":"EUR"}}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RequestID      string `json:"request_id"`
		State          string `json:"state"`
		ReservedAmount struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"reserved_amount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if _, err := uuid.Parse(body.RequestID); err != nil {
		t.Errorf("request_id = %q, want a uuid", body.RequestID)
	}
	if body.State != string(payout.StateAwaitingApproval) {
		t.Errorf("state = %q, want %q", body.State, payout.StateAwaitingApproval)
	}
	// 10.00 does not cover 15.00, so 20.00 is taken too: reserved_amount is
	// what was actually taken, not what was asked for.
	if body.ReservedAmount.Minor != 3000 || body.ReservedAmount.Currency != "EUR" {
		t.Errorf("reserved_amount = %d %s, want 3000 EUR", body.ReservedAmount.Minor, body.ReservedAmount.Currency)
	}
}

// TestTooLittleConfirmedMoneyAnswersAShortfallAClientCanUse is US4 scenario
// 1 on the wire: the figure is a member of the document, not a phrase in it.
func TestTooLittleConfirmedMoneyAnswersAShortfallAClientCanUse(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 2000)
	h := handlerFor(t, f)

	rec := post(t, h, "good-token",
		`{"destination_id":"`+f.destination.String()+`","amount":{"minor":5000,"currency":"EUR"}}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	body := problem(t, rec)
	if body["code"] != payout.CodeInsufficientConfirmedBalance {
		t.Errorf("code = %v, want %q", body["code"], payout.CodeInsufficientConfirmedBalance)
	}
	shortfall, ok := body["shortfall"].(map[string]any)
	if !ok {
		t.Fatalf("shortfall = %v, want {minor, currency}", body["shortfall"])
	}
	if shortfall["minor"] != float64(3000) || shortfall["currency"] != "EUR" {
		t.Errorf("shortfall = %v, want 3000 EUR", shortfall)
	}
	// The standard members are still the standard members.
	if body["status"] != float64(http.StatusConflict) || body["title"] != http.StatusText(http.StatusConflict) {
		t.Errorf("the document is %v, want a well-formed problem+json", body)
	}
}

// TestBelowTheThresholdIsTheSameCodeAndADifferentReason. A client acts on
// both the same way - make up the shortfall - so they share a code; the
// detail is what tells a member which wall they hit.
func TestBelowTheThresholdIsTheSameCodeAndADifferentReason(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 2500, 1500)
	h := handlerFor(t, f)

	rec := post(t, h, "good-token",
		`{"destination_id":"`+f.destination.String()+`","amount":{"minor":1000,"currency":"EUR"}}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	body := problem(t, rec)
	if body["code"] != payout.CodeInsufficientConfirmedBalance {
		t.Errorf("code = %v, want %q", body["code"], payout.CodeInsufficientConfirmedBalance)
	}
	shortfall, _ := body["shortfall"].(map[string]any)
	if shortfall["minor"] != float64(1000) {
		t.Errorf("shortfall = %v, want the 10.00 missing from the threshold", shortfall)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "withdrawals start at") {
		t.Errorf("detail = %q, want it to say withdrawals start at an amount", detail)
	}
	// The threshold and the balance travel as figures too, so a client can
	// say "you need 10.00 more to reach 25.00" without parsing the detail.
	threshold, _ := body["threshold"].(map[string]any)
	if threshold["minor"] != float64(2500) || threshold["currency"] != "EUR" {
		t.Errorf("threshold = %v, want 2500 EUR", threshold)
	}
	confirmed, _ := body["confirmed"].(map[string]any)
	if confirmed["minor"] != float64(1500) {
		t.Errorf("confirmed = %v, want 1500 EUR", confirmed)
	}
	// No figure is spelled into the prose: money.Amount.String renders minor
	// units, so a detail carrying one would show a member "2500 EUR" for
	// twenty-five euros.
	if detail, _ := body["detail"].(string); strings.Contains(detail, "2500") || strings.Contains(detail, "1500") {
		t.Errorf("detail = %q, want no minor-unit figures in member-facing prose", detail)
	}
}

// TestAnotherMembersDestinationIsForbidden is US4 scenario 6 and T099.
func TestAnotherMembersDestinationIsForbidden(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	stranger := seedMember(ctx, t, pool)
	theirs := seedDestination(ctx, t, pool, stranger, true)
	h := handlerFor(t, f)

	rec := post(t, h, "good-token",
		`{"destination_id":"`+theirs.String()+`","amount":{"minor":2000,"currency":"EUR"}}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// A destination that does not exist answers the same way. A 404 for the
	// second would confirm which ids are real, one request at a time.
	rec = post(t, h, "good-token",
		`{"destination_id":"`+uuid.NewString()+`","amount":{"minor":2000,"currency":"EUR"}}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("an unknown destination answers %d, want the same 403", rec.Code)
	}
}

// TestAnUnverifiedDestinationIsAConflictNotAForbidden. The destination IS
// theirs; telling them it is forbidden would send them looking for the wrong
// problem.
func TestAnUnverifiedDestinationIsAConflictNotAForbidden(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	unverified := seedDestination(ctx, t, pool, f.member, false)
	h := handlerFor(t, f)

	rec := post(t, h, "good-token",
		`{"destination_id":"`+unverified.String()+`","amount":{"minor":2000,"currency":"EUR"}}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if body := problem(t, rec); body["code"] != payout.CodeDestinationNotVerified {
		t.Errorf("code = %v, want %q", body["code"], payout.CodeDestinationNotVerified)
	}
}

// TestAWithdrawalNeedsAToken. The member comes from the token, so no token
// is no member and no request.
func TestAWithdrawalNeedsAToken(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	h := handlerFor(t, f)

	body := `{"destination_id":"` + f.destination.String() + `","amount":{"minor":2000,"currency":"EUR"}}`
	if rec := post(t, h, "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("with no token: status = %d, want 401", rec.Code)
	}
	if rec := post(t, h, "not-the-token", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("with a bad token: status = %d, want 401", rec.Code)
	}
}

// TestABodyThisEndpointCannotReadIsRefused. Unknown fields included: on a
// surface that moves money, a misspelled field silently ignored would be a
// request nobody made being accepted as one somebody did.
func TestABodyThisEndpointCannotReadIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	h := handlerFor(t, f)
	destination := f.destination.String()

	for name, body := range map[string]string{
		"not JSON":         `{`,
		"two documents":    `{"destination_id":"` + destination + `","amount":{"minor":1,"currency":"EUR"}}{}`,
		"an unknown field": `{"destination_id":"` + destination + `","amount":{"minor":1,"currency":"EUR"},"approve":true}`,
		"no destination":   `{"amount":{"minor":1000,"currency":"EUR"}}`,
		"a bad currency":   `{"destination_id":"` + destination + `","amount":{"minor":1000,"currency":"euro"}}`,
		"a zero amount":    `{"destination_id":"` + destination + `","amount":{"minor":0,"currency":"EUR"}}`,
		"a negative":       `{"destination_id":"` + destination + `","amount":{"minor":-100,"currency":"EUR"}}`,
	} {
		if rec := post(t, h, "good-token", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestAMethodThisEndpointDoesNotServeSaysWhatItDoes.
func TestAMethodThisEndpointDoesNotServeSaysWhatItDoes(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	h := handlerFor(t, aFixture(ctx, t, 1000, 5000))

	req := httptest.NewRequest(http.MethodDelete, payout.Prefix, nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
		t.Errorf("Allow = %q, want it to name POST", allow)
	}
}

// TestAPathNobodyServesIsANotFound, still behind the gate.
func TestAPathNobodyServesIsANotFound(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	h := handlerFor(t, aFixture(ctx, t, 1000, 5000))

	req := httptest.NewRequest(http.MethodGet, payout.Prefix+"/"+uuid.NewString()+"/nothing-here", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}

	// And without a token it is a 401: the gate wraps the whole mux, so a
	// path nobody serves cannot be probed unauthenticated either.
	unauthenticated := httptest.NewRequest(http.MethodGet, payout.Prefix+"/"+uuid.NewString()+"/nothing-here", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, unauthenticated)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token: status = %d, want 401", rec.Code)
	}
}

// TestEveryRouteIsListed keeps the route table and what Patterns reports in
// step, so the OpenAPI check reads what is registered rather than what
// somebody remembered.
func TestEveryRouteIsListed(t *testing.T) {
	want := []string{
		"GET " + payout.DestinationsPrefix,
		"GET " + payout.Prefix,
		"GET " + payout.Prefix + "/{id}",
		"POST " + payout.DestinationsPrefix,
		"POST " + payout.Prefix,
	}
	if got := payout.Patterns(); !slices.Equal(got, want) {
		t.Errorf("Patterns() = %v, want %v", got, want)
	}
}

// TestAHandlerMissingAPartIsRefusedAtConstruction. Each of these, discovered
// later, is discovered inside a request that has already begun moving money.
func TestAHandlerMissingAPartIsRefusedAtConstruction(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	auth := stubAuth{token: "good-token", member: f.member}

	if _, err := payout.NewHandler(nil, f.withdrawals, f.destinations, &stubVault{}, auth); err == nil {
		t.Error("a handler with no logger was built, want a refusal")
	}
	if _, err := payout.NewHandler(discardLogger(), nil, f.destinations, &stubVault{}, auth); !errors.Is(err, payout.ErrNoWithdrawalStore) {
		t.Errorf("with no service = %v, want one wrapping %v", err, payout.ErrNoWithdrawalStore)
	}
	if _, err := payout.NewHandler(discardLogger(), f.withdrawals, nil, &stubVault{}, auth); !errors.Is(err, payout.ErrNoDestinationStore) {
		t.Errorf("with no destination store = %v, want one wrapping %v", err, payout.ErrNoDestinationStore)
	}
	if _, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, &stubVault{}, nil); err == nil {
		t.Error("a handler with nowhere to authenticate was built, want a refusal")
	}
}

// TestADeploymentThatCannotPayOutAnswers503. Nothing is broken; the
// deployment is incomplete, and the two are different things to whoever is
// paged. The keys go to the log, not to the member.
func TestADeploymentThatCannotPayOutAnswers503(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	for name, withdrawals := range map[string]*payout.Withdrawals{
		"no receivable": mustWithdrawals(t, f, "", euro(t, 1000)),
		"no threshold":  mustWithdrawals(t, f, receivable, money.Amount{}),
	} {
		h, err := payout.NewHandler(discardLogger(), withdrawals, f.destinations, &stubVault{},
			stubAuth{token: "good-token", member: f.member})
		if err != nil {
			t.Fatalf("%s: NewHandler(): %v", name, err)
		}
		rec := post(t, h, "good-token",
			`{"destination_id":"`+f.destination.String()+`","amount":{"minor":2000,"currency":"EUR"}}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503; body: %s", name, rec.Code, rec.Body.String())
		}
		if body := problem(t, rec); body["detail"] == nil {
			t.Errorf("%s: the document says nothing", name)
		}
	}
}

// mustWithdrawals builds the service over the fixture with the given
// receivable and threshold.
func mustWithdrawals(t *testing.T, f fixture, receivable string, threshold money.Amount) *payout.Withdrawals {
	t.Helper()
	w, err := payout.NewWithdrawals(f.pool, f.ledger, receivable, threshold)
	if err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}
	return w
}
