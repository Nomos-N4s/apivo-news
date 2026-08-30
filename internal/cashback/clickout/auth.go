package clickout

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Member is the signed-in caller a click is recorded against.
//
// There is no role here and there must not be one. Every route in this
// module acts on the caller's own click, so a reader owns theirs exactly as
// an editor owns theirs - the only thing that matters is that somebody is
// named, which is FR-023.
type Member struct {
	// ID is the caller's account id, and what the click row records.
	ID uuid.UUID
}

// ErrUnauthenticated reports a request whose bearer token did not resolve to
// an account: missing, malformed, expired, or valid for nobody provisioned
// here. The middleware maps it to 401.
//
// It is the whole of FR-023 at the HTTP boundary. An anonymous click is
// never created, so it can never later be credited to an account - and the
// guarantee is that the handler behind this gate cannot be reached without a
// member, not that it remembers to check for one.
var ErrUnauthenticated = errors.New("clickout: unauthenticated")

// MemberAuthenticator resolves a bearer token to the member behind it. It is
// defined here per the boundary rules - the consumer names its dependency -
// and the composition root wires the identity module behind it.
// Implementations report ErrUnauthenticated (via errors.Is) for the one auth
// verdict there is; any other error is a failure, not a verdict.
type MemberAuthenticator interface {
	AuthenticateMember(ctx context.Context, token string) (Member, error)
}

// ctxKey keys the authenticated Member in a request context. Unexported:
// only this package's middleware writes it and only its handlers read it.
type ctxKey struct{}

// requireMember is the auth gate in front of every route here. It wraps the
// whole mux rather than each route, so a route added later cannot be left
// open by omission - and on this surface an open route is an anonymous click,
// which FR-023 exists to make unrepresentable.
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
			h.log.ErrorContext(r.Context(), "authenticating a click-out request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, member)))
	})
}

// memberFrom returns the member requireMember stored in the request context.
// Every route sits behind that middleware, so a handler reached at all has
// one; the zero value would fail the click row's account foreign key rather
// than pass unnoticed.
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
