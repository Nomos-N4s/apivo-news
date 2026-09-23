package account

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubAuthenticator struct {
	account Account
	err     error
}

func (a stubAuthenticator) Authenticate(_ context.Context, _ string) (Account, error) {
	if a.err != nil {
		return Account{}, a.err
	}
	return a.account, nil
}

func TestRequireAccount(t *testing.T) {
	t.Parallel()

	expectedAccountID := uuid.New()

	tests := []struct {
		name              string
		authHeader        string
		hasHeader         bool
		authenticatorErr  error
		authenticatorAcct Account
		wantStatus        int
		wantWWWAuth       string
		wantNextCalled    bool
		wantAccountInCtx  Account
		wantBodySubstring string
	}{
		{
			name:              "missing authorization header",
			hasHeader:         false,
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="account"`,
			wantNextCalled:    false,
			wantBodySubstring: "a bearer token is required",
		},
		{
			name:              "empty bearer token",
			authHeader:        "Bearer ",
			hasHeader:         true,
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="account"`,
			wantNextCalled:    false,
			wantBodySubstring: "a bearer token is required",
		},
		{
			name:              "non-bearer authorization header",
			authHeader:        "Basic dXNlcjpwYXNz",
			hasHeader:         true,
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="account"`,
			wantNextCalled:    false,
			wantBodySubstring: "a bearer token is required",
		},
		{
			name:              "unauthenticated token error",
			authHeader:        "Bearer invalid-token",
			hasHeader:         true,
			authenticatorErr:  ErrUnauthenticated,
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="account", error="invalid_token"`,
			wantNextCalled:    false,
			wantBodySubstring: "the bearer token is invalid or belongs to no account",
		},
		{
			name:              "internal authenticator error",
			authHeader:        "Bearer valid-format-token",
			hasHeader:         true,
			authenticatorErr:  errors.New("db connection failure"),
			wantStatus:        http.StatusInternalServerError,
			wantWWWAuth:       "",
			wantNextCalled:    false,
			wantBodySubstring: "",
		},
		{
			name:              "valid token passes through to next handler with account in context",
			authHeader:        "Bearer valid-token",
			hasHeader:         true,
			authenticatorAcct: Account{ID: expectedAccountID},
			wantStatus:        http.StatusOK,
			wantWWWAuth:       "",
			wantNextCalled:    true,
			wantAccountInCtx:  Account{ID: expectedAccountID},
			wantBodySubstring: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var nextCalled bool
			var ctxAccount Account

			nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				ctxAccount = accountFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			handler := &Handler{
				log: discardLogger(),
				auth: stubAuthenticator{
					account: tc.authenticatorAcct,
					err:     tc.authenticatorErr,
				},
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/account/tours", nil)
			if tc.hasHeader {
				req.Header.Set("Authorization", tc.authHeader)
			}

			rec := httptest.NewRecorder()

			middleware := handler.requireAccount(nextHandler)
			middleware.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if gotWWWAuth := rec.Header().Get("WWW-Authenticate"); gotWWWAuth != tc.wantWWWAuth {
				t.Errorf("WWW-Authenticate = %q, want %q", gotWWWAuth, tc.wantWWWAuth)
			}

			if tc.wantStatus != http.StatusOK {
				if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
					t.Errorf("Content-Type = %q, want application/problem+json", ct)
				}
			}

			if tc.wantBodySubstring != "" {
				if !strings.Contains(rec.Body.String(), tc.wantBodySubstring) {
					t.Errorf("body = %q, want substring %q", rec.Body.String(), tc.wantBodySubstring)
				}
			}

			if tc.wantNextCalled != nextCalled {
				t.Errorf("next handler called = %v, want %v", nextCalled, tc.wantNextCalled)
			}

			if tc.wantNextCalled && ctxAccount != tc.wantAccountInCtx {
				t.Errorf("context account = %+v, want %+v", ctxAccount, tc.wantAccountInCtx)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		authHeader string
		hasHeader  bool
		wantToken  string
		wantOK     bool
	}{
		{
			name:      "missing header",
			hasHeader: false,
			wantToken: "",
			wantOK:    false,
		},
		{
			name:       "exact lowercase bearer",
			authHeader: "bearer token123",
			hasHeader:  true,
			wantToken:  "token123",
			wantOK:     true,
		},
		{
			name:       "uppercase bearer",
			authHeader: "BEARER token123",
			hasHeader:  true,
			wantToken:  "token123",
			wantOK:     true,
		},
		{
			name:       "mixed case bearer",
			authHeader: "BeArEr token123",
			hasHeader:  true,
			wantToken:  "token123",
			wantOK:     true,
		},
		{
			name:       "extra whitespace around token",
			authHeader: "Bearer   token123   ",
			hasHeader:  true,
			wantToken:  "token123",
			wantOK:     true,
		},
		{
			name:       "empty token after bearer prefix",
			authHeader: "Bearer ",
			hasHeader:  true,
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "whitespace only after bearer prefix",
			authHeader: "Bearer    ",
			hasHeader:  true,
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "too short header",
			authHeader: "Bear",
			hasHeader:  true,
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "different scheme",
			authHeader: "Basic dXNlcjpwYXNz",
			hasHeader:  true,
			wantToken:  "",
			wantOK:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.hasHeader {
				req.Header.Set("Authorization", tc.authHeader)
			}

			token, ok := bearerToken(req)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if token != tc.wantToken {
				t.Errorf("token = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestAccountFromDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	acct := accountFrom(ctx)
	if acct != (Account{}) {
		t.Errorf("accountFrom empty context = %+v, want zero value Account", acct)
	}
}
