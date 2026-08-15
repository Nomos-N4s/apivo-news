package editorial

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Editor is the authenticated editorial caller: the named human whose
// account holds the editor role. Handlers record its ID as the approver or
// withdrawer of editorial decisions (I-1).
type Editor struct {
	// ID is the caller's account id.
	ID uuid.UUID
	// Email is the caller's email.
	Email string
	// DisplayName is the name that appears on editorial decisions.
	DisplayName string
}

// ErrUnauthenticated reports a request whose bearer token did not resolve
// to an account: missing, malformed, expired, or valid for nobody
// provisioned here. The middleware maps it to 401.
var ErrUnauthenticated = errors.New("editorial: unauthenticated")

// ErrNotEditor reports an authenticated caller whose account lacks the
// editor role. The middleware maps it to 403 - the polite early answer to
// the rule the database enforces again on write.
var ErrNotEditor = errors.New("editorial: caller is not an editor")

// EditorAuthenticator resolves a bearer token to an Editor, requiring the
// editor role. It is defined here per the boundary rules - the consumer
// names its dependency - and the composition root in cmd wires the identity
// module's JWT verification and role lookup behind it. Implementations
// report ErrUnauthenticated or ErrNotEditor (via errors.Is) for the two
// auth verdicts; any other error is a failure, not a verdict.
type EditorAuthenticator interface {
	AuthenticateEditor(ctx context.Context, token string) (Editor, error)
}

// ctxKey keys the authenticated Editor in a request context. Unexported:
// only this package's middleware writes it and only its handlers read it.
type ctxKey struct{}

// requireEditor is the auth gate in front of every editorial route: it
// extracts the bearer token, authenticates it, requires the editor role,
// and stores the Editor in the request context. Failures answer problem+json
// per the contract: 401 without a valid token, 403 for non-editors.
func (h *Handler) requireEditor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="editorial"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		ed, err := h.auth.AuthenticateEditor(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="editorial", error="invalid_token"`)
			platformhttp.Problem(w, http.StatusUnauthorized, "the bearer token is invalid or belongs to no account")
			return
		case errors.Is(err, ErrNotEditor):
			platformhttp.Problem(w, http.StatusForbidden, "the editor role is required")
			return
		case err != nil:
			h.log.ErrorContext(r.Context(), "authenticating editorial request", "error", err)
			platformhttp.Problem(w, http.StatusInternalServerError, "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ed)))
	})
}

// editorFrom returns the authenticated editor requireEditor stored in the
// request context. Every route sits behind that middleware, so a handler
// reached at all has an editor; the zero value would fail the database's
// approver foreign key rather than pass unnoticed.
func editorFrom(ctx context.Context) Editor {
	editor, _ := ctx.Value(ctxKey{}).(Editor)
	return editor
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
