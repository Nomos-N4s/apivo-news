package editorial

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

type dummyPayload struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantOK     bool
		wantDetail string
		wantDst    dummyPayload
	}{
		{
			name:    "valid json document",
			body:    `{"name":"Alice","age":30}`,
			wantOK:  true,
			wantDst: dummyPayload{Name: "Alice", Age: 30},
		},
		{
			name:    "valid json with trailing whitespace",
			body:    `{"name":"Bob","age":25}` + "\n\t \r\n",
			wantOK:  true,
			wantDst: dummyPayload{Name: "Bob", Age: 25},
		},
		{
			name:       "malformed json syntax",
			body:       `{"name":"Alice",`,
			wantOK:     false,
			wantDetail: "request body is not valid JSON",
		},
		{
			name:       "unknown field in json",
			body:       `{"name":"Alice","age":30,"extra":"field"}`,
			wantOK:     false,
			wantDetail: "request body is not valid JSON",
		},
		{
			name:       "multiple json documents",
			body:       `{"name":"Alice","age":30}{"name":"Bob","age":25}`,
			wantOK:     false,
			wantDetail: "request body must contain a single JSON document",
		},
		{
			name:       "trailing stray closing bracket",
			body:       `{"name":"Alice","age":30}]`,
			wantOK:     false,
			wantDetail: "request body must contain a single JSON document",
		},
		{
			name:       "trailing stray closing brace",
			body:       `{"name":"Alice","age":30}}`,
			wantOK:     false,
			wantDetail: "request body must contain a single JSON document",
		},
		{
			name:       "trailing stray comma",
			body:       `{"name":"Alice","age":30},`,
			wantOK:     false,
			wantDetail: "request body must contain a single JSON document",
		},
		{
			name:       "trailing bare token",
			body:       `{"name":"Alice","age":30}garbage`,
			wantOK:     false,
			wantDetail: "request body must contain a single JSON document",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			var dst dummyPayload
			gotOK := decodeJSON(rec, req, &dst)

			if gotOK != tc.wantOK {
				t.Fatalf("decodeJSON() = %v, want %v", gotOK, tc.wantOK)
			}

			if tc.wantOK {
				if dst != tc.wantDst {
					t.Errorf("dst = %+v, want %+v", dst, tc.wantDst)
				}
			} else {
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
					t.Errorf("Content-Type = %q, want application/problem+json", ct)
				}
				var p platformhttp.ProblemDetails
				if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
					t.Fatalf("unmarshalling problem details: %v", err)
				}
				if p.Status != http.StatusBadRequest {
					t.Errorf("problem.status = %d, want %d", p.Status, http.StatusBadRequest)
				}
				if tc.wantDetail != "" && !strings.Contains(p.Detail, tc.wantDetail) {
					t.Errorf("problem.detail = %q, want it to contain %q", p.Detail, tc.wantDetail)
				}
			}
		})
	}
}

func TestDecodeJSONBodyExceedsMaxBytes(t *testing.T) {
	t.Parallel()

	// Construct a body larger than maxBodyBytes (1 << 20 bytes).
	overflowSize := maxBodyBytes + 1024
	largeValue := strings.Repeat("a", overflowSize)
	largeBody := `{"name":"` + largeValue + `","age":1}`

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(largeBody))
	rec := httptest.NewRecorder()

	var dst dummyPayload
	gotOK := decodeJSON(rec, req, &dst)

	if gotOK {
		t.Fatal("decodeJSON() = true, want false for oversized body")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var p platformhttp.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshalling problem details: %v", err)
	}
	if !strings.Contains(p.Detail, "request body is not valid JSON") && !strings.Contains(p.Detail, "http: request body too large") {
		t.Errorf("problem.detail = %q, expected error regarding size limit or JSON decoding", p.Detail)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	h := &Handler{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	payload := map[string]string{"status": "ok", "message": "hello"}
	h.writeJSON(rec, req, http.StatusCreated, payload)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling response: %v", err)
	}
	if got["status"] != "ok" || got["message"] != "hello" {
		t.Errorf("got body = %+v, want status=ok message=hello", got)
	}
}

func TestInternalError(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := &Handler{
		log: logger,
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	internalErr := errors.New("sensitive database connection error")
	h.internalError(rec, req, "doing database operation", internalErr)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var p platformhttp.ProblemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshalling problem details: %v", err)
	}
	if p.Status != http.StatusInternalServerError {
		t.Errorf("problem.status = %d, want %d", p.Status, http.StatusInternalServerError)
	}
	if strings.Contains(p.Detail, "sensitive database connection error") {
		t.Errorf("internal error detail leaked to client response: %q", p.Detail)
	}

	// Verify internal details were logged correctly.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "doing database operation") {
		t.Errorf("logger output = %q, want it to contain operation context", logOutput)
	}
	if !strings.Contains(logOutput, "sensitive database connection error") {
		t.Errorf("logger output = %q, want it to contain internal error details", logOutput)
	}
}

func TestBlank(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"\t\n\r ", true},
		{"a", false},
		{"  a  ", false},
		{"  hello world  ", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := blank(tc.input)
			if got != tc.want {
				t.Errorf("blank(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
