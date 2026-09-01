// Who is asking (T091).
//
// The same shape as the wallet module's gate and for the same reason, and
// written here rather than shared for the same reason too: the two modules
// answer different surfaces, and a shared gate is one refactor away from a
// change made for one applying to the other. The consumer names its
// dependency; the composition root wires the identity module behind both.

package payout

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Member is the signed-in caller asking to be paid.
//
// There is no role here and there must not be one. Every route in this
// module acts on the caller's OWN money, so a reader owns theirs exactly as
// an operator owns theirs - and a role check would be the beginning of an
// endpoint that could spend somebody else's. Approving a withdrawal IS an
// operator's act, and it lives on the operator surface behind that surface's
// own gate (C-4).
type Member struct {
	// ID is the caller's account id, and the only member any route here
	// will ever act for.
	ID uuid.UUID
}

// ErrUnauthenticated reports a request whose bearer token did not resolve to
// an account: missing, malformed, expired, or valid for nobody provisioned
// here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("payout: unauthenticated")

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
// else's money leaving: the member is taken from the TOKEN and never from
// the path or a query parameter, which is what makes withdrawing another
// member's balance unrepresentable rather than merely refused.
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
			h.log.ErrorContext(r.Context(), "authenticating a withdrawal request", "error", err)
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
