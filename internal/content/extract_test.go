package content_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Nomos-N4s/apivo-news/internal/content"
)

func TestDeriveExtract(t *testing.T) {
	t.Parallel()

	// Building blocks sized so the arithmetic is visible: sentence150 is
	// exactly 150 runes including its full stop.
	sentence150a := strings.Repeat("a", 149) + "."
	sentence150b := strings.Repeat("b", 149) + "."
	sentence149b := strings.Repeat("b", 148) + "."
	tail := strings.Repeat("c", 50) + "."

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty body", in: "", want: ""},
		{name: "whitespace-only body", in: " \n\t ", want: ""},
		{
			name: "short body returned whole",
			in:   "Μία σύντομη πρόταση.",
			want: "Μία σύντομη πρόταση.",
		},
		{
			name: "short body without any terminator returned whole",
			in:   "no sentence boundary here at all",
			want: "no sentence boundary here at all",
		},
		{
			name: "whitespace runs collapse to single spaces",
			in:   "Πρώτη  πρόταση.\n\nΔεύτερη\tπρόταση.",
			want: "Πρώτη πρόταση. Δεύτερη πρόταση.",
		},
		{
			name: "cut at the last sentence boundary that fits",
			// 150 + 1 + 150 = 301 runes for two sentences: only the first fits.
			in:   sentence150a + " " + sentence150b + " " + tail,
			want: sentence150a,
		},
		{
			name: "sentence boundary exactly at the 300-rune bound",
			// 150 + 1 + 149 = 300 runes: both sentences fit to the rune.
			in:   sentence150a + " " + sentence149b + " " + tail,
			want: sentence150a + " " + sentence149b,
		},
		{
			name: "no boundary in reach cuts at a word boundary with ellipsis",
			in:   "alpha beta " + strings.Repeat("x", 400),
			want: "alpha beta…",
		},
		{
			name: "terminator inside a number is not a boundary",
			in:   "About 1.5 " + strings.Repeat("z", 400),
			want: "About 1.5…",
		},
		{
			name: "one unbroken overlong word is hard-cut to the bound",
			in:   strings.Repeat("y", 350),
			want: strings.Repeat("y", 299) + "…",
		},
		{
			name: "greek question mark (erotimatiko, U+037E) ends a sentence",
			in:   "Τι ώρα είναι; " + strings.Repeat("κ", 400),
			want: "Τι ώρα είναι;",
		},
		{
			name: "semicolon as typed on greek keyboards ends a sentence",
			in:   "Τι ώρα είναι; " + strings.Repeat("κ", 400),
			want: "Τι ώρα είναι;",
		},
		{
			name: "exclamation ends a sentence",
			in:   "Achtung! " + strings.Repeat("m", 400),
			want: "Achtung!",
		},
		{
			name: "bound counts runes not bytes",
			// 250 Greek runes are 500 bytes; the whole text still fits.
			in:   strings.Repeat("ω", 250),
			want: strings.Repeat("ω", 250),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := content.DeriveExtract(tt.in)
			if got != tt.want {
				t.Errorf("DeriveExtract() = %q, want %q", got, tt.want)
			}
			if n := utf8.RuneCountInString(got); n > 300 {
				t.Errorf("DeriveExtract() is %d runes, must never exceed 300", n)
			}
		})
	}
}
