package translation

import (
	"errors"
	"strings"
	"testing"
)

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	valid := Request{
		SourceTitle:    "Δήμαρχος ανακοινώνει νέο πάρκο",
		SourceText:     "Το πάρκο ανοίγει τον Μάιο.",
		SourceLanguage: "el",
		TargetLanguage: "de",
		PromptVersion:  CurrentPromptVersion,
	}

	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{name: "complete request", mutate: func(*Request) {}},
		{name: "blank title", mutate: func(r *Request) { r.SourceTitle = "  " }, wantErr: "source title"},
		{name: "blank text", mutate: func(r *Request) { r.SourceText = "" }, wantErr: "source text"},
		{name: "blank source language", mutate: func(r *Request) { r.SourceLanguage = "\t" }, wantErr: "source language"},
		{name: "blank target language", mutate: func(r *Request) { r.TargetLanguage = "" }, wantErr: "target language"},
		{name: "blank prompt version", mutate: func(r *Request) { r.PromptVersion = "" }, wantErr: "prompt version"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := valid
			tc.mutate(&req)
			err := req.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Validate() error is not ErrInvalidRequest: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestSentinelErrorsAreDistinct: the classification only helps a caller if
// no sentinel matches another - "back off" must never read as "fix the
// credentials".
func TestSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := map[string]error{
		"ErrInvalidRequest":  ErrInvalidRequest,
		"ErrAuth":            ErrAuth,
		"ErrRateLimited":     ErrRateLimited,
		"ErrInvalidResponse": ErrInvalidResponse,
		"ErrUnavailable":     ErrUnavailable,
		"ErrTimeout":         ErrTimeout,
	}
	for name, err := range sentinels {
		for otherName, other := range sentinels {
			if name == otherName {
				continue
			}
			if errors.Is(err, other) {
				t.Errorf("%s matches %s; the two failures must stay distinguishable", name, otherName)
			}
		}
		if !strings.HasPrefix(err.Error(), "translation: ") {
			t.Errorf("%s message %q should name the module it came from", name, err.Error())
		}
	}
}
