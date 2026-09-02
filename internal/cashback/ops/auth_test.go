package ops_test

// The gate in front of the whole operator surface. It is asserted here
// rather than through any one endpoint because it wraps the mux, not the
// routes: what it refuses, it refuses before a caller learns whether the
// path they asked for exists.

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

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// discardLogger keeps the refusal paths, which log, from writing to the
// test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubAuth is the composition root's adapter, canned: one verdict for
// every token.
type stubAuth struct {
	op  ops.Operator
	err error
}

func (s stubAuth) AuthenticateOperator(context.Context, string) (ops.Operator, error) {
	return s.op, s.err
}

// anOperator is an authenticated caller the gate lets through.
var anOperator = ops.Operator{ID: uuid.New(), Email: "ops@example.test", DisplayName: "Ops Person"}

// unservedPath is what every case here asks for. It is deliberately a path
// no route claims: the gate's answers must not depend on whether the
// address exists, and asking for one that does would let a passing test
// hide that they did.
const unservedPath = ops.Prefix + "no-such-queue"

// probe sends one GET at unservedPath through a handler built over the
// given authenticator and returns the recorder. Every case here is about
// the gate, which runs before the path or the method is looked at.
func probe(t *testing.T, auth ops.OperatorAuthenticator, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, unservedPath, strings.NewReader(""))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{}, unreachableSettler{}, unreachableReconciliation{}, auth).ServeHTTP(rec, req)
	return rec
}

// problemOf decodes a problem+json body, failing the test if the answer is
// not one - which is itself the assertion that the module's single error
// convention held.
func problemOf(t *testing.T, rec *httptest.ResponseRecorder) (status int, detail string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json (body %q)", ct, rec.Body.String())
	}
	var body struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer is not problem+json: %v (body %q)", err, rec.Body.String())
	}
	return body.Status, body.Detail
}

func TestAnUnauthenticatedRequestIsRefusedBeforeAnythingElse(t *testing.T) {
	t.Parallel()

	// A token that authenticates would still 404 at unservedPath; the
	// point is that the gate answers first, so an unauthenticated caller
	// cannot map the surface by reading status codes.
	cases := []struct {
		name          string
		authorization string
	}{
		{name: "no header at all"},
		{name: "the wrong scheme", authorization: "Basic aGk6dGhlcmU="},
		{name: "bearer with nothing after it", authorization: "Bearer"},
		{name: "bearer with only spaces after it", authorization: "Bearer    "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := probe(t, stubAuth{op: anOperator}, tc.authorization)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
			if status, detail := problemOf(t, rec); status != http.StatusUnauthorized || detail == "" {
				t.Errorf("problem = (%d, %q), want a 401 that says why", status, detail)
			}
		})
	}
}

// TestTheBearerSchemeIsMatchedCaseInsensitively pins RFC 9110's rule: the
// scheme is a token, not a literal, and a client sending "bearer" is
// authenticating, not probing.
func TestTheBearerSchemeIsMatchedCaseInsensitively(t *testing.T) {
	t.Parallel()

	rec := probe(t, stubAuth{op: anOperator}, "bEaReR token")
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d: a lowercase scheme was read as no token at all", rec.Code)
	}
}

func TestAnInvalidTokenIsAChallengeAndAnAccountWithoutTheRoleIsNot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
		// wantChallenge is whether a WWW-Authenticate header belongs on the
		// answer: a 401 invites another attempt with a better token, a 403
		// says this account will never do.
		wantChallenge bool
		wantDetail    string
	}{
		{
			name: "a token that resolves to nobody", err: ops.ErrUnauthenticated,
			wantStatus: http.StatusUnauthorized, wantChallenge: true,
			wantDetail: "invalid",
		},
		{
			name: "an account that is not an operator", err: ops.ErrNotOperator,
			wantStatus: http.StatusForbidden, wantChallenge: false,
			wantDetail: "operator role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := probe(t, stubAuth{err: tc.err}, "Bearer t")

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); (got != "") != tc.wantChallenge {
				t.Errorf("WWW-Authenticate = %q, want present = %v", got, tc.wantChallenge)
			}
			if _, detail := problemOf(t, rec); !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.wantDetail)
			}
		})
	}
}

// TestAFailedLookupIsNotAVerdict covers the third outcome the gate has to
// keep apart from the other two: the question went unanswered. Reporting
// that as 403 would tell an operator their authority was revoked when the
// role table was merely unreachable, and the body must carry none of the
// reason - internals are for the log.
func TestAFailedLookupIsNotAVerdict(t *testing.T) {
	t.Parallel()

	const secret = "dial tcp 10.0.0.4:5432: connection refused"
	rec := probe(t, stubAuth{err: errors.New(secret)}, "Bearer t")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the answer leaks the failure: %q", rec.Body.String())
	}
}

// TestAnAuthenticatedProbeOfAnUnservedPathIs404 is what proves the gate
// runs first: the same path answered 401 above without a token, and only a
// caller who got through learns the path does not exist.
func TestAnAuthenticatedProbeOfAnUnservedPathIs404(t *testing.T) {
	t.Parallel()

	rec := probe(t, stubAuth{op: anOperator}, "Bearer t")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if _, detail := problemOf(t, rec); !strings.Contains(detail, "no such endpoint") {
		t.Errorf("detail = %q, want the catch-all's answer", detail)
	}
}

// TestEveryPatternSitsUnderThePrefix keeps the route table and the mounted
// subtree together. A pattern outside Prefix is registered on a mux the
// composition root never routes to, so it would be unreachable in
// production while every unit test that calls the handler directly passed.
func TestEveryPatternSitsUnderThePrefix(t *testing.T) {
	t.Parallel()

	for _, pattern := range ops.Patterns() {
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Errorf("pattern %q is not %q", pattern, "METHOD /path")
			continue
		}
		if !strings.HasPrefix(path, ops.Prefix) {
			t.Errorf("%q is outside the mounted prefix %q, so nothing would reach it", pattern, ops.Prefix)
		}
	}
}
