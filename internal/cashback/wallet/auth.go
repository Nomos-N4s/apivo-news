// Who is asking (T078).
//
// The gate is the same shape as the click-out module's and for the same
// reason, but it is written here rather than shared: the two modules answer
// different surfaces, and a shared gate is one refactor away from a change
// made for one surface applying to the other. The consumer names its
// dependency; the composition root wires the identity module behind both.

package wallet

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Member is the signed-in caller whose wallet is being read.
//
// There is no role here and there must not be one. Every route in this
// module reads the caller's OWN money, so a reader owns theirs exactly as an
// operator owns theirs - and a role check would be the beginning of an
// endpoint that could read somebody else's.
type Member struct {
	// ID is the caller's account id, and the only member any route here
	// will ever be answered about.
	ID uuid.UUID
}

// ErrUnauthenticated reports a request whose bearer token did not resolve to
// an account: missing, malformed, expired, or valid for nobody provisioned
// here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("wallet: unauthenticated")

// MemberAuthenticator resolves a bearer token to the member behind it.
// Implementations report ErrUnauthenticated (via errors.Is) for the one auth
// verdict there is; any other error is a failure, not a verdict.
type MemberAuthenticator interface {
	AuthenticateMember(ctx context.Context, token string) (Member, error)
}

// ctxKey keys the authenticated Member in a request context. Unexported:
// only this package's middleware writes it and only its handlers read it.
type ctxKey struct{}

// requireMember is the auth gate in front of every route here.
//
// It wraps the whole mux rather than each route, so a route added later
// cannot be left open by omission. On this surface an open route is somebody
// else's balance: the member is taken from the TOKEN and never from the
// path or a query parameter, which is what makes reading another member's
// wallet unrepresentable rather than merely refused.
func (h *Handler) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		member, err := h.auth.AuthenticateMember(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback", error="invalid_token"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid or belongs to no account")
			return
		case err != nil:
			h.log.ErrorContext(r.Context(), "authenticating a wallet request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, member)))
	})
}

// memberFrom returns the member requireMember stored in the request context.
// Every route sits behind that middleware, so a handler reached at all has
// one.
func memberFrom(ctx context.Context) Member {
	member, _ := ctx.Value(ctxKey{}).(Member)
	return member
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
