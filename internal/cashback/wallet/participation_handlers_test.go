package wallet_test

// The opt-in over HTTP: the statuses, and what each body says (T080, US3).
//
// Over a real database, because every status here is a verdict the database
// reaches - 409 is an upsert predicate that found an active row, 404 is a
// read that found none - and a fake service would be a place to write the
// answers down rather than reach them.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// participationHandler builds the module's handler over a real
// participation service and a member the token resolves to.
func participationHandler(t *testing.T, tx pgx.Tx, terms wallet.Terms, member uuid.UUID) http.Handler {
	t.Helper()
	service, err := wallet.NewParticipations(tx, store.New(tx), terms)
	if err != nil {
		t.Fatalf("NewParticipations(): %v", err)
	}
	return wallet.NewHandler(slog.New(slog.DiscardHandler), nil, nil, service, nil,
		fakeAuth{token: "a-token", member: member})
}

// participationRequest is one authenticated request to the opt-in path.
func participationRequest(t *testing.T, method, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(method, wallet.ParticipationPrefix, nil)
	} else {
		req = httptest.NewRequest(method, wallet.ParticipationPrefix, reader)
	}
	req.Header.Set("Authorization", "Bearer a-token")
	return req
}

// decoded reads a JSON response body.
func decoded(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestOptingInAnswers201WithTheParticipation(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.1.0"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	body := decoded(t, rec)
	switch {
	case body["status"] != "active":
		t.Errorf("status = %v, want active", body["status"])
	case body["terms_version"] != "3.1.0":
		t.Errorf("terms_version = %v, want 3.1.0", body["terms_version"])
	case body["default_currency"] != "SEK":
		t.Errorf("default_currency = %v, want SEK", body["default_currency"])
	case body["left_at"] != nil:
		t.Errorf("left_at = %v on an active participation, want null", body["left_at"])
	}
	if _, ok := body["opted_in_at"].(string); !ok {
		t.Errorf("opted_in_at = %v, want a timestamp", body["opted_in_at"])
	}
}

// The refusal names the version in force. A client that had to guess would
// guess, and the member would end up accepting whatever it had cached.
func TestOptingInWithStaleTermsAnswers400NamingTheVersion(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.0.0"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if detail, _ := decoded(t, rec)["detail"].(string); !strings.Contains(detail, "3.1.0") {
		t.Errorf("the refusal %q does not name the version in force", detail)
	}
}

func TestOptingInTwiceAnswers409(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	if rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.1.0"}`)); rec.Code != http.StatusCreated {
		t.Fatalf("the first opt-in: %d %s", rec.Code, rec.Body)
	}
	rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.1.0"}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
}

// No brand file, no terms to offer. 503 rather than 500 - nothing is broken
// - and rather than a 404, which would say the API is not here.
func TestOptingInWithoutABrandAnswers503(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, wallet.Terms{}, member)

	rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.1.0"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
}

// A misspelled field silently dropped would reach the service as the empty
// string, and the member would be told their consent was stale when what
// was wrong was the client.
func TestOptingInRefusesABodyItDoesNotUnderstand(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	for _, body := range []string{
		// camelCase rather than snake_case, which is the mistake a client
		// actually makes, and a field nobody serves beside a good one.
		`{"termsVersion":"3.1.0"}`,
		`{"terms_version":"3.1.0","brand":"somebody-elses"}`,
		`{"terms_version":"3.1.0"} {"terms_version":"3.1.0"}`,
		`{"terms_version":"3.1.0"}}`,
		`not json at all`,
		``,
	} {
		rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %q answered %d, want 400", body, rec.Code)
		}
	}
}

// 404 on a member who never joined is how the frontend knows to render the
// opt-in rather than an error.
func TestReadingAParticipationThatIsNotThereAnswers404(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	rec := serveHandler(t, handler, participationRequest(t, http.MethodGet, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestLeavingAnswers200WithTheDate(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	if rec := serveHandler(t, handler, participationRequest(t, http.MethodPost, `{"terms_version":"3.1.0"}`)); rec.Code != http.StatusCreated {
		t.Fatalf("Join: %d %s", rec.Code, rec.Body)
	}
	rec := serveHandler(t, handler, participationRequest(t, http.MethodDelete, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := decoded(t, rec)
	if body["status"] != "left" {
		t.Errorf("status = %v, want left", body["status"])
	}
	if _, ok := body["left_at"].(string); !ok {
		t.Errorf("left_at = %v, want the date they left", body["left_at"])
	}

	// DELETE is the method a client retries without thinking.
	again := serveHandler(t, handler, participationRequest(t, http.MethodDelete, ""))
	if again.Code != http.StatusOK {
		t.Errorf("the second DELETE answered %d, want 200: %s", again.Code, again.Body)
	}
	if decoded(t, again)["left_at"] != body["left_at"] {
		t.Errorf("the second DELETE answered a different date: %v then %v", body["left_at"], decoded(t, again)["left_at"])
	}
}

func TestLeavingWithoutHavingJoinedAnswers404(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	handler := participationHandler(t, tx, theTerms, member)

	rec := serveHandler(t, handler, participationRequest(t, http.MethodDelete, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// The opt-in path sits behind the same gate as the wallet's, and the gate
// wraps the whole mux rather than each route - so a route added here cannot
// be left open by omission.
func TestTheOptInPathIsBehindTheAuthGate(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	handler := participationHandler(t, tx, theTerms, aMember(ctx, t, tx))

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, wallet.ParticipationPrefix, strings.NewReader(`{"terms_version":"3.1.0"}`))
		if rec := serveHandler(t, handler, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s answered %d, want 401", method, rec.Code)
		}
	}
}

// A method nobody serves gets 405 and an Allow header naming the ones that
// are, and a sub-path nobody claimed gets a problem+json 404 from this
// module rather than whatever else holds the namespace.
func TestTheOptInPathAnswersForWhatItDoesNotServe(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	handler := participationHandler(t, tx, theTerms, aMember(ctx, t, tx))

	req := httptest.NewRequest(http.MethodPut, wallet.ParticipationPrefix, nil)
	req.Header.Set("Authorization", "Bearer a-token")
	rec := serveHandler(t, handler, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT answered %d, want 405", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow %q does not name %s", allow, method)
		}
	}

	stray := httptest.NewRequest(http.MethodGet, wallet.ParticipationPrefix+"/nowhere", nil)
	stray.Header.Set("Authorization", "Bearer a-token")
	if rec := serveHandler(t, handler, stray); rec.Code != http.StatusNotFound {
		t.Errorf("a stray sub-path answered %d, want 404", rec.Code)
	}
}
