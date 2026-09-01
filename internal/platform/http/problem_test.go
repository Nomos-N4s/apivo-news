package http_test

// What a problem document carries, and what it refuses to let a caller
// overwrite.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// decode reads the response as a problem document, failing the case when it
// is not one.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestAProblemCarriesTheFourStandardMembers(t *testing.T) {
	rec := httptest.NewRecorder()
	platformhttp.Problem(rec, http.StatusConflict, "that already happened")

	body := decode(t, rec)
	if rec.Code != http.StatusConflict {
		t.Errorf("status line = %d, want 409", rec.Code)
	}
	if body["type"] != "about:blank" || body["title"] != http.StatusText(http.StatusConflict) {
		t.Errorf("type/title = %v/%v, want about:blank and the status text", body["type"], body["title"])
	}
	if body["status"] != float64(http.StatusConflict) || body["detail"] != "that already happened" {
		t.Errorf("status/detail = %v/%v, want 409 and the detail given", body["status"], body["detail"])
	}
}

// TestAProblemWithNoDetailOmitsIt. An empty detail is nothing to say, and a
// member reading `"detail": ""` learns less than one reading nothing.
func TestAProblemWithNoDetailOmitsIt(t *testing.T) {
	rec := httptest.NewRecorder()
	platformhttp.Problem(rec, http.StatusInternalServerError, "")

	if _, present := decode(t, rec)["detail"]; present {
		t.Error("the document carries an empty detail, want none at all")
	}
}

// TestExtensionMembersReachTheClient is the point of ProblemWith: a client
// branches on the code and renders the figure, without parsing prose.
func TestExtensionMembersReachTheClient(t *testing.T) {
	rec := httptest.NewRecorder()
	platformhttp.ProblemWith(rec, http.StatusConflict, "you are short", map[string]any{
		"code":      "insufficient_confirmed_balance",
		"shortfall": map[string]any{"minor": 400, "currency": "EUR"},
	})

	body := decode(t, rec)
	if body["code"] != "insufficient_confirmed_balance" {
		t.Errorf("code = %v, want the code passed", body["code"])
	}
	shortfall, ok := body["shortfall"].(map[string]any)
	if !ok {
		t.Fatalf("shortfall = %v, want the object passed", body["shortfall"])
	}
	if shortfall["minor"] != float64(400) || shortfall["currency"] != "EUR" {
		t.Errorf("shortfall = %v, want 400 EUR", shortfall)
	}
}

// TestAnExtensionCannotOverwriteAStandardMember. The one thing worse than
// dropping a caller's mistake is answering with a status field that
// disagrees with the status line.
func TestAnExtensionCannotOverwriteAStandardMember(t *testing.T) {
	rec := httptest.NewRecorder()
	platformhttp.ProblemWith(rec, http.StatusConflict, "the real detail", map[string]any{
		"type":   "https://example.test/made-up",
		"title":  "Made Up",
		"status": http.StatusOK,
		"detail": "the wrong detail",
		"code":   "kept",
	})

	body := decode(t, rec)
	if body["type"] != "about:blank" || body["title"] != http.StatusText(http.StatusConflict) {
		t.Errorf("type/title = %v/%v, want the document's own", body["type"], body["title"])
	}
	if body["status"] != float64(http.StatusConflict) {
		t.Errorf("status = %v, want the status line's %d", body["status"], http.StatusConflict)
	}
	if body["detail"] != "the real detail" {
		t.Errorf("detail = %v, want the one passed as the argument", body["detail"])
	}
	if body["code"] != "kept" {
		t.Errorf("code = %v, want the extension that is not a standard member", body["code"])
	}
}

// TestAnUnmarshallableExtensionStillAnswers. The status has to reach the
// client even when the extension cannot be encoded; a half-written body
// behind a problem+json status line would be worse than a plain one.
func TestAnUnmarshallableExtensionStillAnswers(t *testing.T) {
	rec := httptest.NewRecorder()
	platformhttp.ProblemWith(rec, http.StatusConflict, "detail", map[string]any{
		"code": make(chan int),
	})

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 even when the body could not be built", rec.Code)
	}
}
