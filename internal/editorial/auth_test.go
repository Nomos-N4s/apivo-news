package editorial

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

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

type mockEditorAuthenticator struct {
	authFunc func(ctx context.Context, token string) (Editor, error)
}

func (m mockEditorAuthenticator) AuthenticateEditor(ctx context.Context, token string) (Editor, error) {
	if m.authFunc != nil {
		return m.authFunc(ctx, token)
	}
	return Editor{}, ErrUnauthenticated
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		authHeader string
		wantToken  string
		wantOK     bool
	}{
		{
			name:       "missing header",
			authHeader: "",
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "non-bearer scheme",
			authHeader: "Basic dXNlcjpwYXNz",
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "prefix only",
			authHeader: "Bearer ",
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "bearer prefix with spaces only",
			authHeader: "Bearer    ",
			wantToken:  "",
			wantOK:     false,
		},
		{
			name:       "valid uppercase Bearer",
			authHeader: "Bearer secret-token-123",
			wantToken:  "secret-token-123",
			wantOK:     true,
		},
		{
			name:       "valid lowercase bearer",
			authHeader: "bearer secret-token-456",
			wantToken:  "secret-token-456",
			wantOK:     true,
		},
		{
			name:       "valid mixed-case bEaReR with extra spaces",
			authHeader: "bEaReR    secret-token-789   ",
			wantToken:  "secret-token-789",
			wantOK:     true,
		},
		{
			name:       "short length header",
			authHeader: "Bear",
			wantToken:  "",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			token, ok := bearerToken(req)
			if token != tt.wantToken || ok != tt.wantOK {
				t.Errorf("bearerToken() = (%q, %v), want (%q, %v)", token, ok, tt.wantToken, tt.wantOK)
			}
		})
	}
}

func TestEditorFrom(t *testing.T) {
	t.Parallel()

	t.Run("empty context", func(t *testing.T) {
		t.Parallel()
		got := editorFrom(context.Background())
		if got != (Editor{}) {
			t.Errorf("editorFrom() = %+v, want empty Editor", got)
		}
	})

	t.Run("context with invalid value type", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), ctxKey{}, "not-an-editor")
		got := editorFrom(ctx)
		if got != (Editor{}) {
			t.Errorf("editorFrom() = %+v, want empty Editor", got)
		}
	})

	t.Run("context with Editor value", func(t *testing.T) {
		t.Parallel()
		expected := Editor{
			ID:          uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			Email:       "editor@example.com",
			DisplayName: "Test Editor",
		}
		ctx := context.WithValue(context.Background(), ctxKey{}, expected)
		got := editorFrom(ctx)
		if got != expected {
			t.Errorf("editorFrom() = %+v, want %+v", got, expected)
		}
	})
}

func TestRequireEditorMiddleware(t *testing.T) {
	t.Parallel()

	validEditor := Editor{
		ID:          uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Email:       "editor@example.test",
		DisplayName: "Jane Editor",
	}

	testCases := []struct {
		name              string
		authHeader        string
		authFunc          func(ctx context.Context, token string) (Editor, error)
		wantStatus        int
		wantWWWAuth       string
		wantProblemDetail string
		wantNextCalled    bool
		wantEditorInCtx   Editor
	}{
		{
			name:              "missing bearer token header",
			authHeader:        "",
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="editorial"`,
			wantProblemDetail: "a bearer token is required",
			wantNextCalled:    false,
		},
		{
			name:              "non-bearer scheme",
			authHeader:        "Basic token123",
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="editorial"`,
			wantProblemDetail: "a bearer token is required",
			wantNextCalled:    false,
		},
		{
			name:              "blank bearer token",
			authHeader:        "Bearer    ",
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="editorial"`,
			wantProblemDetail: "a bearer token is required",
			wantNextCalled:    false,
		},
		{
			name:       "unauthenticated token (ErrUnauthenticated)",
			authHeader: "Bearer invalid-token",
			authFunc: func(_ context.Context, _ string) (Editor, error) {
				return Editor{}, ErrUnauthenticated
			},
			wantStatus:        http.StatusUnauthorized,
			wantWWWAuth:       `Bearer realm="editorial", error="invalid_token"`,
			wantProblemDetail: "the bearer token is invalid or belongs to no account",
			wantNextCalled:    false,
		},
		{
			name:       "non-editor caller (ErrNotEditor)",
			authHeader: "Bearer reader-token",
			authFunc: func(_ context.Context, _ string) (Editor, error) {
				return Editor{}, ErrNotEditor
			},
			wantStatus:        http.StatusForbidden,
			wantWWWAuth:       "",
			wantProblemDetail: "the editor role is required",
			wantNextCalled:    false,
		},
		{
			name:       "authenticator unexpected internal error",
			authHeader: "Bearer any-token",
			authFunc: func(_ context.Context, _ string) (Editor, error) {
				return Editor{}, errors.New("database connection failed")
			},
			wantStatus:        http.StatusInternalServerError,
			wantWWWAuth:       "",
			wantProblemDetail: "",
			wantNextCalled:    false,
		},
		{
			name:       "successful authentication as editor",
			authHeader: "Bearer valid-editor-token",
			authFunc: func(_ context.Context, token string) (Editor, error) {
				if token == "valid-editor-token" {
					return validEditor, nil
				}
				return Editor{}, ErrUnauthenticated
			},
			wantStatus:      http.StatusOK,
			wantWWWAuth:     "",
			wantNextCalled:  true,
			wantEditorInCtx: validEditor,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := &Handler{
				log:  testLogger(),
				auth: mockEditorAuthenticator{authFunc: tc.authFunc},
			}

			var nextCalled bool
			var editorCaptured Editor

			nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				editorCaptured = editorFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			middleware := h.requireEditor(nextHandler)

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()

			middleware.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if gotWWW := rec.Header().Get("WWW-Authenticate"); gotWWW != tc.wantWWWAuth {
				t.Errorf("WWW-Authenticate header = %q, want %q", gotWWW, tc.wantWWWAuth)
			}

			if nextCalled != tc.wantNextCalled {
				t.Errorf("next handler called = %v, want %v", nextCalled, tc.wantNextCalled)
			}

			if tc.wantNextCalled {
				if editorCaptured != tc.wantEditorInCtx {
					t.Errorf("editor in context = %+v, want %+v", editorCaptured, tc.wantEditorInCtx)
				}
			} else {
				if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
					t.Errorf("Content-Type = %q, want application/problem+json", ct)
				}

				var prob platformhttp.ProblemDetails
				if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
					t.Fatalf("unmarshalling problem response: %v", err)
				}
				if prob.Status != tc.wantStatus {
					t.Errorf("problem status = %d, want %d", prob.Status, tc.wantStatus)
				}
				if tc.wantProblemDetail != "" && !strings.Contains(prob.Detail, tc.wantProblemDetail) {
					t.Errorf("problem detail = %q, want it to contain %q", prob.Detail, tc.wantProblemDetail)
				}
			}
		})
	}
}
