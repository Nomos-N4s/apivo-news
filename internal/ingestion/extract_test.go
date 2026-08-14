package ingestion_test

// The D9 extract rule is exercised at its boundaries: summary preference,
// the 300-character window (counted in runes, so Greek text measures in
// letters), the sentence cut, and both fallbacks.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
)

func TestDeriveExtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary string
		body    string
		want    string
	}{
		{
			name:    "feed summary preferred over body",
			summary: "  Η περίληψη της ροής.  ",
			body:    strings.Repeat("κ", 500),
			want:    "Η περίληψη της ροής.",
		},
		{
			name:    "summary longer than the window stays verbatim",
			summary: strings.Repeat("s", 400),
			body:    "irrelevant",
			want:    strings.Repeat("s", 400),
		},
		{
			name:    "whitespace-only summary falls through to the body",
			summary: "   \n\t ",
			body:    "Ein kurzer Text.",
			want:    "Ein kurzer Text.",
		},
		{
			name: "neither summary nor body derives nothing",
			want: "",
		},
		{
			name: "short body returned whole",
			body: "  Μικρό σώμα κειμένου.  ",
			want: "Μικρό σώμα κειμένου.",
		},
		{
			name: "body of exactly 300 runes returned whole",
			body: strings.Repeat("αβγ", 100),
			want: strings.Repeat("αβγ", 100),
		},
		{
			name: "long body cut at the last sentence end in the window",
			body: strings.Repeat("α", 250) + ". " + strings.Repeat("β", 100),
			want: strings.Repeat("α", 250) + ".",
		},
		{
			name: "sentence end on the very last rune of the window",
			body: strings.Repeat("a", 299) + ". " + strings.Repeat("b", 50),
			want: strings.Repeat("a", 299) + ".",
		},
		{
			name: "greek question mark closes a sentence",
			body: strings.Repeat("γ", 200) + "; " + strings.Repeat("δ", 200),
			want: strings.Repeat("γ", 200) + ";",
		},
		{
			name: "erotimatiko closes a sentence",
			body: strings.Repeat("ε", 150) + "; " + strings.Repeat("ζ", 300),
			want: strings.Repeat("ε", 150) + ";",
		},
		{
			name: "decimal point does not end a sentence",
			body: strings.Repeat("x", 295) + " 3.14159 and more trailing text",
			want: strings.Repeat("x", 295),
		},
		{
			name: "no sentence end falls back to the last word boundary",
			body: strings.Repeat("ω", 280) + " " + strings.Repeat("ψ", 100),
			want: strings.Repeat("ω", 280),
		},
		{
			name: "unbroken text hard-cuts at 300 runes",
			body: strings.Repeat("ω", 400),
			want: strings.Repeat("ω", 300),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ingestion.DeriveExtract(tt.summary, tt.body)
			if got != tt.want {
				t.Fatalf("DeriveExtract() = %q, want %q", got, tt.want)
			}
			// A body-derived extract never exceeds the D9 window; only a
			// feed-authored summary may.
			if strings.TrimSpace(tt.summary) == "" {
				if n := utf8.RuneCountInString(got); n > 300 {
					t.Fatalf("derived extract is %d runes, want <= 300", n)
				}
			}
		})
	}
}
