package account_test

// Self-registration (#544): the door in front of the gate. What it needs
// from a token, what it writes, what it answers the second time, and the
// property the whole route exists for — a person the gate refuses is a
// person this route serves.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/account"
)

// register is a POST to the one open route.
func register() *http.Request { return httptest.NewRequest(http.MethodPost, "/api/v1/account", nil) }

// unprovisioned is a caller the auth provider vouches for and this
// deployment has never seen: Authenticate refuses them, Verify answers.
func unprovisioned(id uuid.UUID, email string) fakeAuth {
	return fakeAuth{err: account.ErrUnauthenticated, claims: account.Claims{ID: id, Email: email}}
}

func decodeProfile(t *testing.T, rec *httptest.ResponseRecorder) account.Profile {
	t.Helper()
	var p account.Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding the profile: %v (body %q)", err, rec.Body.String())
	}
	return p
}

func TestRegistrationServesThePersonTheGateRefuses(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	want := account.Profile{ID: id, Email: "member@example.test", DisplayName: "member", Role: "reader"}
	store := &fakeStore{profile: want, created: true}
	auth := unprovisioned(id, "member@example.test")

	// The gate refuses them everywhere else...
	assertProblem(t, serve(t, store, auth, withToken(get("/api/v1/account"))), http.StatusUnauthorized)

	// ...and the door lets them in.
	rec := serve(t, store, auth, withToken(register()))
	assertStatus(t, rec, http.StatusCreated)
	if got := decodeProfile(t, rec); got != want {
		t.Errorf("registration answered %+v, want %+v", got, want)
	}
	if store.registered != 1 || store.gotID != id || store.gotEmail != "member@example.test" {
		t.Errorf("the store was asked to register %d time(s) for %s %q; want once for the token's subject and email", store.registered, store.gotID, store.gotEmail)
	}
}

func TestRegisteringAgainAnswersTheExistingRowUnchanged(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	// The row already says editor: a second registration does not touch
	// that, and the answer says what the row says.
	existing := account.Profile{ID: id, Email: "editor@example.test", DisplayName: "Ed", Role: "editor"}
	store := &fakeStore{profile: existing, created: false}
	rec := serve(t, store, fakeAuth{id: id, claims: account.Claims{ID: id, Email: "editor@example.test"}}, withToken(register()))
	assertStatus(t, rec, http.StatusOK)
	if got := decodeProfile(t, rec); got != existing {
		t.Errorf("a repeat registration answered %+v, want the existing %+v", got, existing)
	}
}

func TestRegistrationRefusesWhatItCannotRegister(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	for _, tc := range []struct {
		name  string
		store *fakeStore
		auth  fakeAuth
		req   *http.Request
		want  int
	}{
		{"no token", &fakeStore{}, unprovisioned(id, "m@example.test"), register(), http.StatusUnauthorized},
		{"a token that does not verify", &fakeStore{}, fakeAuth{verifyErr: account.ErrUnauthenticated}, withToken(register()), http.StatusUnauthorized},
		{"a verifier that could not answer", &fakeStore{}, fakeAuth{verifyErr: errors.New("jwks unreachable")}, withToken(register()), http.StatusInternalServerError},
		{"a token with no email", &fakeStore{}, unprovisioned(id, ""), withToken(register()), http.StatusBadRequest},
		{"an email another account holds", &fakeStore{emailTaken: true}, unprovisioned(id, "m@example.test"), withToken(register()), http.StatusConflict},
		{"a store that failed", &fakeStore{failWith: errors.New("connection reset")}, unprovisioned(id, "m@example.test"), withToken(register()), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := serve(t, tc.store, tc.auth, tc.req)
			assertProblem(t, rec, tc.want)
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 must say how to authenticate")
			}
		})
	}
	// A refusal before the store is reached is a refusal that wrote
	// nothing: the no-email case must not have registered anybody.
	store := &fakeStore{}
	serve(t, store, unprovisioned(id, ""), withToken(register()))
	if store.registered != 0 {
		t.Errorf("a token with no email registered %d account(s); it must register none", store.registered)
	}
}

func TestTheCallerReadsTheirOwnAccount(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	row := account.Profile{ID: id, Email: "op@example.test", DisplayName: "Op", Role: "operator"}
	rec := serve(t, &fakeStore{profile: row}, fakeAuth{id: id}, withToken(get("/api/v1/account")))
	assertStatus(t, rec, http.StatusOK)
	if got := decodeProfile(t, rec); got != row {
		t.Errorf("GET /api/v1/account = %+v, want %+v", got, row)
	}

	assertProblem(t, serve(t, &fakeStore{missing: true}, fakeAuth{id: id}, withToken(get("/api/v1/account"))), http.StatusNotFound)
	assertProblem(t, serve(t, &fakeStore{failWith: errors.New("connection reset")}, fakeAuth{id: id}, withToken(get("/api/v1/account"))), http.StatusInternalServerError)
	assertProblem(t, serve(t, &fakeStore{profile: row}, fakeAuth{id: id}, get("/api/v1/account")), http.StatusUnauthorized)
}

// The one open route is the one open route: the gate still fronts
// everything else, including an unrouted path under the prefix.
func TestOnlyRegistrationIsOpen(t *testing.T) {
	t.Parallel()
	auth := unprovisioned(uuid.New(), "m@example.test")
	for _, req := range []*http.Request{
		withToken(get("/api/v1/account/tours")),
		withToken(put("/api/v1/account/tours/editor", `{"cursor":"1"}`)),
		withToken(get("/api/v1/account/anything")),
	} {
		assertProblem(t, serve(t, &fakeStore{}, auth, req), http.StatusUnauthorized)
	}
	if got := account.Patterns(); len(got) != 4 || got[0] != "GET /api/v1/account" || got[3] != "PUT /api/v1/account/tours/{tour}" {
		t.Errorf("Patterns() = %v, want the four routes this module serves", got)
	}
}
