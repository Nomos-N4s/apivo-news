// Self-registration (#544): the one route under this prefix a person
// reaches BEFORE they have an account, and the read of the account they
// then have.
//
// The auth provider has already verified who the caller is; what it cannot
// do is make them exist here. Editors are seeded by an operator, and until
// this route every member was too — a service-role call and an insert by
// hand. This is the door a sign-in walks through on its own: a verified
// token whose subject has no row creates one, as a reader, and a token
// whose subject already has one gets it back unchanged.

package account

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Profile is the caller's own account row, as this API reports it.
type Profile struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	// Role is what the api will let this person do, read from the row and
	// never from the token: a client that showed an operator queue on the
	// strength of a claim would show it to whoever could mint the claim.
	Role string `json:"role"`
}

// ErrEmailTaken reports a registration whose email already belongs to a
// different account id. The email is unique here and the subject is the
// identity, so this is a person signing in through a second provider
// account with an address they used before — a thing to refuse and name,
// not to merge.
var ErrEmailTaken = errors.New("account: the email belongs to another account")

// ProfileStore creates and reads the caller's own row.
type ProfileStore interface {
	// Register creates the row for id with the reader role when none
	// exists, reporting true; when one exists it is returned unchanged
	// and false. An email held by a different id reports ErrEmailTaken.
	Register(ctx context.Context, id uuid.UUID, email string) (Profile, bool, error)
	// Profile reads one row. A missing account reports ErrNoAccount.
	Profile(ctx context.Context, id uuid.UUID) (Profile, error)
}

// Store is everything this module reads and writes. PGStore satisfies it.
type Store interface {
	TourStore
	ProfileStore
}

// register implements POST /api/v1/account. It sits in FRONT of
// requireAccount — see NewHandler — and does its own token check with the
// Verifier, because the gate's whole job is to refuse the person this
// route exists to serve.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="account"`)
		platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
		return
	}
	claims, err := h.verifier.Verify(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="account", error="invalid_token"`)
		platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid")
		return
	case err != nil:
		h.internalError(w, r, "verifying a registration", err)
		return
	}
	if claims.Email == "" {
		// A verified person with no verified address: the row could not be
		// written, and inventing an address to write it with would be
		// worse than saying so.
		platformhttp.Problem(w, http.StatusBadRequest, "the token carries no email, and an account needs one")
		return
	}

	profile, created, err := h.store.Register(r.Context(), claims.ID, claims.Email)
	switch {
	case errors.Is(err, ErrEmailTaken):
		platformhttp.Problem(w, http.StatusConflict, "that email already belongs to another account")
		return
	case err != nil:
		h.internalError(w, r, "registering an account", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.writeJSON(w, r, status, profile)
}

// readProfile implements GET /api/v1/account: the caller's own row, role
// included, which is the one answer to "what may I do here" a client can
// trust.
func (h *Handler) readProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := h.store.Profile(r.Context(), accountFrom(r.Context()).ID)
	switch {
	case errors.Is(err, ErrNoAccount):
		platformhttp.Problem(w, http.StatusNotFound, "this token authenticates an account that no longer exists")
		return
	case err != nil:
		h.internalError(w, r, "reading the account", err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, profile)
}
