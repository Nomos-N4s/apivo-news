// Package account serves what a signed-in person owns about themselves,
// as distinct from what they may do to the product's records.
//
// The editorial module answers "may this named human approve an article",
// and gates every route on the editor role because approval is a
// privileged, attributed act (I-1). Nothing here is privileged in that
// sense: it is one person's own state, readable and writable by them and
// nobody else, and a reader has as much right to it as an editor does.
// That is why this is its own module with its own gate rather than another
// branch inside editorial's — a route that only needs "somebody is signed
// in" must not be reached through a door marked "editors only", and the
// reverse mistake is worse.
package account

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Account is the authenticated caller: whoever the bearer token resolved
// to. Only the id is carried, because every route here acts on the
// caller's own row and none of them prints a name.
type Account struct {
	ID uuid.UUID
}

// ErrUnauthenticated reports a bearer token that did not resolve to an
// account: missing, malformed, expired, or valid for nobody provisioned
// here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("account: unauthenticated")

// Authenticator resolves a bearer token to an Account. Defined here per
// the boundary rules — the consumer names its dependency — and wired in
// cmd to the identity module's JWT verification.
//
// Deliberately NOT the editorial module's EditorAuthenticator: that one
// also requires the editor role, and requiring it here would mean a reader
// could not record that they had seen the reader's own tour.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Account, error)
}

// ctxKey keys the authenticated Account in a request context.
type ctxKey struct{}

// requireAccount is the gate in front of every route in this module. It
// asks one question — is somebody signed in — and answers 401 when the
// answer is no. There is no 403 here: every route acts on the caller's own
// row, so there is no role that could be insufficient.
func (h *Handler) requireAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="account"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		acct, err := h.auth.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="account", error="invalid_token"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid or belongs to no account")
			return
		case err != nil:
			h.log.ErrorContext(r.Context(), "authenticating account request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, acct)))
	})
}

// accountFrom returns the Account requireAccount stored in the request
// context. Every route sits behind that middleware, so a handler reached
// at all has one.
func accountFrom(ctx context.Context) Account {
	acct, _ := ctx.Value(ctxKey{}).(Account)
	return acct
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. The scheme comparison is case-insensitive per RFC 9110.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
