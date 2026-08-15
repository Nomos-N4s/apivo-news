package translation

import (
	"errors"
	"fmt"
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

func TestSpendIsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spend Spend
		want  bool
	}{
		{name: "nothing spent", spend: Spend{}, want: true},
		{name: "a priced call", spend: Spend{CostMicroUSD: 126}},
		{name: "a free call that cost nothing", spend: Spend{CostMicroUSD: 0}, want: true},
		{name: "work billed at an unknown price", spend: Spend{UnmeteredAttempts: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.spend.IsZero(); got != tc.want {
				t.Errorf("Spend%+v.IsZero() = %t, want %t", tc.spend, got, tc.want)
			}
		})
	}
}

// TestSpendErrorKeepsItsClassification: the spend must ride along without
// hiding why the translation failed - a caller still has to tell a rate
// limit from a rejected key.
func TestSpendErrorKeepsItsClassification(t *testing.T) {
	t.Parallel()

	underlying := fmt.Errorf("openaicompat: provider answered 200 with an unusable answer: %w", ErrInvalidResponse)
	err := error(&SpendError{Spend: Spend{CostMicroUSD: 126, UnmeteredAttempts: 1}, Err: underlying})

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false; the sentinel must survive the wrapping")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("errors.Is(err, underlying) = false; Unwrap must expose the failure")
	}

	var spent *SpendError
	if !errors.As(err, &spent) {
		t.Fatal("errors.As did not recover the SpendError")
	}
	if spent.CostMicroUSD != 126 || spent.UnmeteredAttempts != 1 {
		t.Errorf("recovered spend = %+v, want the cost and the unmetered count intact", spent.Spend)
	}

	message := err.Error()
	for _, want := range []string{"126", "1", "unusable answer"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not mention %q", message, want)
		}
	}
}

// TestSpendErrorWithoutACauseStillReads: Error() is called from logging,
// where a panic would be worse than a vague sentence.
func TestSpendErrorWithoutACauseStillReads(t *testing.T) {
	t.Parallel()

	err := &SpendError{Spend: Spend{CostMicroUSD: 42}}
	if got := err.Error(); !strings.Contains(got, "42") {
		t.Errorf("Error() = %q, want it to report the spend", got)
	}
	if err.Unwrap() != nil {
		t.Error("Unwrap() should report no cause when none was given")
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
