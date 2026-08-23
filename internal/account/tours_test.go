package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/account"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore records what it was asked to write, so the tests can assert on
// the call rather than only on the status code.
type fakeStore struct {
	tours    map[string]string
	full     bool
	missing  bool
	failWith error

	gotTour   string
	gotCursor string
	writes    int
}

func (s *fakeStore) Tours(_ context.Context, _ uuid.UUID) (map[string]string, error) {
	if s.missing {
		return nil, account.ErrNoAccount
	}
	if s.failWith != nil {
		return nil, s.failWith
	}
	return s.tours, nil
}

func (s *fakeStore) SetTour(_ context.Context, _ uuid.UUID, tourID, cursor string) (bool, error) {
	s.writes++
	s.gotTour, s.gotCursor = tourID, cursor
	if s.missing {
		return false, account.ErrNoAccount
	}
	if s.failWith != nil {
		return false, s.failWith
	}
	return !s.full, nil
}

type fakeAuth struct {
	id  uuid.UUID
	err error
}

func (a fakeAuth) Authenticate(context.Context, string) (account.Account, error) {
	if a.err != nil {
		return account.Account{}, a.err
	}
	return account.Account{ID: a.id}, nil
}

func serve(t *testing.T, store account.TourStore, auth account.Authenticator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	account.NewHandler(discardLogger(), store, auth).ServeHTTP(rec, req)
	return rec
}

func withToken(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer a-token")
	return req
}

func get(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

func put(path, body string) *http.Request {
	return httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

// assertProblem also checks the media type: this module's whole reason for
// having a catch-all is that every error under the prefix is problem+json,
// and a status code alone would not catch a text/plain body from ServeMux.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	assertStatus(t, rec, want)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content type = %q, want application/problem+json", ct)
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

func TestARequestWithoutATokenIsRefused(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	rec := serve(t, store, fakeAuth{id: uuid.New()}, get("/api/v1/account/tours"))
	assertProblem(t, rec, http.StatusUnauthorized)
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must say how to authenticate")
	}
}

func TestATokenThatResolvesToNobodyIsRefused(t *testing.T) {
	t.Parallel()
	auth := fakeAuth{err: account.ErrUnauthenticated}
	rec := serve(t, &fakeStore{}, auth, withToken(get("/api/v1/account/tours")))
	assertProblem(t, rec, http.StatusUnauthorized)
}

// A failure to ANSWER the auth question is not the same as answering "no".
// Reporting 401 for a database outage would send the caller to sign in
// again, which cannot help and hides the outage.
func TestAnAuthFailureIsNotA401(t *testing.T) {
	t.Parallel()
	auth := fakeAuth{err: errors.New("jwks unreachable")}
	rec := serve(t, &fakeStore{}, auth, withToken(get("/api/v1/account/tours")))
	assertProblem(t, rec, http.StatusInternalServerError)
}

// The point of this module existing separately from editorial. A reader
// owns their own tour progress exactly as an editor owns theirs, and
// nothing here asks about a role.
func TestNoRoleIsRequired(t *testing.T) {
	t.Parallel()
	store := &fakeStore{tours: map[string]string{"reader": "2"}}
	rec := serve(t, store, fakeAuth{id: uuid.New()}, withToken(get("/api/v1/account/tours")))
	assertStatus(t, rec, http.StatusOK)
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

func TestAnAccountWithNoProgressReadsAsAnEmptyObject(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{}, fakeAuth{id: uuid.New()}, withToken(get("/api/v1/account/tours")))
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Tours map[string]string `json:"tours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v (body %q)", err, rec.Body.String())
	}
	// Not null: a client that has to tell an absent field from an empty one
	// will get it wrong in one of the two places it checks.
	if body.Tours == nil {
		t.Fatalf("tours = null, want {} (body %q)", rec.Body.String())
	}
	if len(body.Tours) != 0 {
		t.Fatalf("tours = %v, want empty", body.Tours)
	}
}

func TestProgressIsReturnedAsStored(t *testing.T) {
	t.Parallel()
	store := &fakeStore{tours: map[string]string{"editor": "7", "reader": "done"}}
	rec := serve(t, store, fakeAuth{id: uuid.New()}, withToken(get("/api/v1/account/tours")))
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Tours map[string]string `json:"tours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Tours["editor"] != "7" || body.Tours["reader"] != "done" {
		t.Fatalf("tours = %v, want the stored document", body.Tours)
	}
}

func TestAVanishedAccountReads404(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{missing: true}, fakeAuth{id: uuid.New()}, withToken(get("/api/v1/account/tours")))
	assertProblem(t, rec, http.StatusNotFound)
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

func TestACursorIsRecorded(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	rec := serve(t, store, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"7"}`)))
	assertStatus(t, rec, http.StatusNoContent)
	if store.gotTour != "editor" || store.gotCursor != "7" {
		t.Fatalf("stored (%q, %q), want (editor, 7)", store.gotTour, store.gotCursor)
	}
}

func TestDoneIsACursor(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	rec := serve(t, store, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"done"}`)))
	assertStatus(t, rec, http.StatusNoContent)
	if store.gotCursor != "done" {
		t.Fatalf("cursor = %q, want done", store.gotCursor)
	}
}

// The id becomes a key in a jsonb document on a row that ordinary requests
// read. Everything outside the shape is refused BEFORE the store is asked,
// so a hostile id never reaches SQL at all.
func TestATourIdOutsideTheShapeIsRefusedBeforeTheStore(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"Editor", "editor!", "1editor", "-editor", "editor.tour", strings.Repeat("e", 65)} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			rec := serve(t, store, fakeAuth{id: uuid.New()},
				withToken(put("/api/v1/account/tours/"+id, `{"cursor":"1"}`)))
			assertProblem(t, rec, http.StatusBadRequest)
			if store.writes != 0 {
				t.Fatalf("the store was asked to write %q", id)
			}
		})
	}
}

func TestACursorOutsideTheShapeIsRefusedBeforeTheStore(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"empty":      `{"cursor":""}`,
		"negative":   `{"cursor":"-1"}`,
		"fractional": `{"cursor":"1.5"}`,
		"word":       `{"cursor":"finished"}`,
		"too long":   `{"cursor":"99999"}`,
		"whitespace": `{"cursor":" 1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			rec := serve(t, store, fakeAuth{id: uuid.New()},
				withToken(put("/api/v1/account/tours/editor", body)))
			assertProblem(t, rec, http.StatusBadRequest)
			if store.writes != 0 {
				t.Fatalf("the store was asked to write %q", body)
			}
		})
	}
}

// A misspelled field silently ignored would read as acceptance: the caller
// would believe progress was recorded and it would not be.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	rec := serve(t, store, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"1","step":2}`)))
	assertProblem(t, rec, http.StatusBadRequest)
	if store.writes != 0 {
		t.Fatal("a body with an unknown field was written")
	}
}

func TestASecondDocumentInTheBodyIsRefused(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	rec := serve(t, store, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"1"}{"cursor":"2"}`)))
	assertProblem(t, rec, http.StatusBadRequest)
	if store.writes != 0 {
		t.Fatal("a body with two documents was written")
	}
}

// The tour ids come from the front end, so an authenticated caller can
// invent them. Past the cap the answer names the problem rather than
// growing the row.
func TestANewTourPastTheCapIsRefused(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{full: true}, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/invented", `{"cursor":"1"}`)))
	assertProblem(t, rec, http.StatusConflict)
}

func TestAWriteToAVanishedAccountIs404(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{missing: true}, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"1"}`)))
	assertProblem(t, rec, http.StatusNotFound)
}

func TestAStoreFailureIsAnOpaque500(t *testing.T) {
	t.Parallel()
	store := &fakeStore{failWith: errors.New("connection refused to db-1.internal")}
	rec := serve(t, store, fakeAuth{id: uuid.New()},
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"1"}`)))
	assertProblem(t, rec, http.StatusInternalServerError)
	// Internals are for the log, never the wire.
	if strings.Contains(rec.Body.String(), "db-1.internal") {
		t.Fatalf("the response leaked the internal error: %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The prefix
// ---------------------------------------------------------------------------

func TestAnUnservedPathUnderThePrefixIsProblemJSON(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{}, fakeAuth{id: uuid.New()},
		withToken(get("/api/v1/account/nothing-here")))
	assertProblem(t, rec, http.StatusNotFound)
}

func TestAWrongMethodIsProblemJSON(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{}, fakeAuth{id: uuid.New()},
		withToken(httptest.NewRequest(http.MethodDelete, "/api/v1/account/tours", nil)))
	assertProblem(t, rec, http.StatusNotFound)
}

// The gate is in front of the mux, so even a path nobody serves refuses an
// anonymous caller rather than confirming what does and does not exist.
func TestAnUnservedPathStillRequiresAToken(t *testing.T) {
	t.Parallel()
	rec := serve(t, &fakeStore{}, fakeAuth{id: uuid.New()}, get("/api/v1/account/nothing-here"))
	assertProblem(t, rec, http.StatusUnauthorized)
}

func TestPatternsListsEveryRouteSorted(t *testing.T) {
	t.Parallel()
	got := account.Patterns()
	want := []string{"GET /api/v1/account/tours", "PUT /api/v1/account/tours/{tour}"}
	if len(got) != len(want) {
		t.Fatalf("Patterns() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Patterns() = %v, want %v", got, want)
		}
	}
}
