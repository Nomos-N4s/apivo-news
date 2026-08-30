package ops

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Operator is the authenticated operator: the named human whose account
// holds the operator role. Handlers record its id and its name on every
// decision they write, which is the first half of what FR-061 asks for -
// the reason is the second, and it comes from the request.
type Operator struct {
	// ID is the caller's account id, and what a resolution records as
	// resolved_by.
	ID uuid.UUID
	// Email is the caller's email.
	Email string
	// DisplayName is the name that appears on operator decisions.
	DisplayName string
}

// ErrUnauthenticated reports a request whose bearer token did not resolve
// to an account: missing, malformed, expired, or valid for nobody
// provisioned here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("ops: unauthenticated")

// ErrNotOperator reports an authenticated caller whose account lacks the
// operator role. The middleware maps it to 403.
var ErrNotOperator = errors.New("ops: caller is not an operator")

// OperatorAuthenticator resolves a bearer token to an Operator, requiring
// the operator role. It is defined here per the boundary rules - the
// consumer names its dependency - and the composition root wires the
// identity module's JWT verification and role lookup behind it.
// Implementations report ErrUnauthenticated or ErrNotOperator (via
// errors.Is) for the two auth verdicts; any other error is a failure, not
// a verdict.
type OperatorAuthenticator interface {
	AuthenticateOperator(ctx context.Context, token string) (Operator, error)
}

// ctxKey keys the authenticated Operator in a request context. Unexported:
// only this package's middleware writes it and only its handlers read it.
type ctxKey struct{}

// requireOperator is the auth gate in front of every route here: it
// extracts the bearer token, authenticates it, requires the operator role,
// and stores the Operator in the request context.
//
// It wraps the whole mux rather than each route, so a route added later
// cannot be left unauthenticated by omission - and a probe of a path
// nobody serves is refused before it learns whether the path exists.
func (h *Handler) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback-ops"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		op, err := h.auth.AuthenticateOperator(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback-ops", error="invalid_token"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid or belongs to no account")
			return
		case errors.Is(err, ErrNotOperator):
			platformhttp.Problem(w, http.StatusForbidden, "the operator role is required")
			return
		case err != nil:
			h.log.ErrorContext(r.Context(), "authenticating operator request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, op)))
	})
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
