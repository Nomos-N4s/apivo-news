// The auth gate in front of the catalogue.
//
// There is no anonymous cashback surface (FR-023, contracts/http-api.md),
// and browsing is part of that surface even though nothing it returns
// depends on who is asking: a catalogue readable without a token is a rate
// card published to anyone who finds the path, complete with the terms and
// the exclusions of every commercial arrangement behind it.
//
// The gate therefore proves a caller exists and then forgets them. No member
// id reaches a handler here, because no read here has any use for one - the
// language comes from the query and the place will too. A gate that stored
// the caller would invite a later read to scope by them, which is how a
// public catalogue quietly becomes a personalised one.

package catalogue

import (
	"context"
	"errors"
	"net/http"
	"strings"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// ErrUnauthenticated reports a request whose bearer token did not resolve to
// an account: missing, malformed, expired, or valid for nobody provisioned
// here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("catalogue: unauthenticated")

// MemberAuthenticator resolves a bearer token to an account, and answers
// nothing about which one. It is defined here per the boundary rules - the
// consumer names its dependency - and the composition root wires the
// identity module behind it. Implementations report ErrUnauthenticated (via
// errors.Is) for the one auth verdict there is; any other error is a
// failure, not a verdict.
type MemberAuthenticator interface {
	AuthenticateReader(ctx context.Context, token string) error
}

// requireMember is the auth gate in front of every route here. It wraps the
// whole mux rather than each route, so a route added later cannot be left
// open by omission.
func (h *Handler) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		switch err := h.auth.AuthenticateReader(r.Context(), token); {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="cashback", error="invalid_token"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid or belongs to no account")
			return
		case err != nil:
			h.log.ErrorContext(r.Context(), "authenticating a catalogue request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r)
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
