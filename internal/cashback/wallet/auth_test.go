package wallet_test

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

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// mockAuth implements wallet.MemberAuthenticator for auth middleware testing.
type mockAuth struct {
	fn func(ctx context.Context, token string) (wallet.Member, error)
}

func (m mockAuth) AuthenticateMember(ctx context.Context, token string) (wallet.Member, error) {
	if m.fn != nil {
		return m.fn(ctx, token)
	}
	return wallet.Member{}, wallet.ErrUnauthenticated
}

func TestRequireMemberMiddleware(t *testing.T) {
	t.Parallel()

	validMemberID := uuid.New()

	tests := []struct {
		name               string
		authHeader         string
		authFn             func(ctx context.Context, token string) (wallet.Member, error)
		wantStatus         int
		wantWWWAuth        string
		wantBodyContains   string
	}{
		{
			name:             "missing authorization header",
			authHeader:       "",
			wantStatus:       http.StatusUnauthorized,
			wantWWWAuth:      `Bearer realm="cashback"`,
			wantBodyContains: "a bearer token is required",
		},
		{
			name:             "non-bearer scheme (Basic)",
			authHeader:       "Basic dXNlcjpwYXNz",
			wantStatus:       http.StatusUnauthorized,
			wantWWWAuth:      `Bearer realm="cashback"`,
			wantBodyContains: "a bearer token is required",
		},
		{
			name:             "bearer prefix without token",
			authHeader:       "Bearer ",
			wantStatus:       http.StatusUnauthorized,
			wantWWWAuth:      `Bearer realm="cashback"`,
			wantBodyContains: "a bearer token is required",
		},
		{
			name:             "bearer prefix with whitespace only token",
			authHeader:       "Bearer    ",
			wantStatus:       http.StatusUnauthorized,
			wantWWWAuth:      `Bearer realm="cashback"`,
			wantBodyContains: "a bearer token is required",
		},
		{
			name:       "unauthenticated token returning ErrUnauthenticated",
			authHeader: "Bearer invalid_token",
			authFn: func(ctx context.Context, token string) (wallet.Member, error) {
				return wallet.Member{}, wallet.ErrUnauthenticated
			},
			wantStatus:       http.StatusUnauthorized,
			wantWWWAuth:      `Bearer realm="cashback", error="invalid_token"`,
			wantBodyContains: "the bearer token is invalid or belongs to no account",
		},
		{
			name:       "authenticator returns unexpected error",
			authHeader: "Bearer token_cause_error",
			authFn: func(ctx context.Context, token string) (wallet.Member, error) {
				return wallet.Member{}, errors.New("database connection failed")
			},
			wantStatus:  http.StatusInternalServerError,
			wantWWWAuth: "",
		},
		{
			name:       "valid token standard Bearer scheme",
			authHeader: "Bearer valid_token",
			authFn: func(ctx context.Context, token string) (wallet.Member, error) {
				if token == "valid_token" {
					return wallet.Member{ID: validMemberID}, nil
				}
				return wallet.Member{}, wallet.ErrUnauthenticated
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid token lowercase bearer scheme",
			authHeader: "bearer valid_token",
			authFn: func(ctx context.Context, token string) (wallet.Member, error) {
				if token == "valid_token" {
					return wallet.Member{ID: validMemberID}, nil
				}
				return wallet.Member{}, wallet.ErrUnauthenticated
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid token uppercase BEARER scheme",
			authHeader: "BEARER valid_token",
			authFn: func(ctx context.Context, token string) (wallet.Member, error) {
				if token == "valid_token" {
					return wallet.Member{ID: validMemberID}, nil
				}
				return wallet.Member{}, wallet.ErrUnauthenticated
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wService := wallets(t, &fakeBalances{}, &fakePayouts{member: validMemberID}, money.Amount{Minor: 100, Currency: "EUR"})
			handler := wallet.NewHandler(
				slog.New(slog.DiscardHandler),
				wService,
				history(t, &fakeEntries{}),
				nil,
				nil,
				mockAuth{fn: tt.authFn},
			)

			req := httptest.NewRequest(http.MethodGet, wallet.Prefix, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status code = %d, want %d", rec.Code, tt.wantStatus)
			}

			if gotWWWAuth := rec.Header().Get("WWW-Authenticate"); gotWWWAuth != tt.wantWWWAuth {
				t.Errorf("WWW-Authenticate header = %q, want %q", gotWWWAuth, tt.wantWWWAuth)
			}

			if tt.wantBodyContains != "" {
				body, _ := io.ReadAll(rec.Body)
				if !strings.Contains(string(body), tt.wantBodyContains) {
					t.Errorf("body %q does not contain expected substring %q", string(body), tt.wantBodyContains)
				}
			}
		})
	}
}
