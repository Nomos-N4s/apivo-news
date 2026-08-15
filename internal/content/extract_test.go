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
		{
			name: "tags are dropped and block boundaries become spaces",
			in:   "<p>Πρώτη πρόταση.</p><p>Δεύτερη πρόταση.</p>",
			want: "Πρώτη πρόταση. Δεύτερη πρόταση.",
		},
		{
			name: "attributes never leak into the extract",
			in:   `<a href="https://example.test/x" title="ignored">Der Text</a>.`,
			want: "Der Text.",
		},
		{
			name: "a '>' inside a quoted attribute does not end the tag early",
			in:   `<a title="a>b">Text</a>`,
			want: "Text",
		},
		{
			name: "character references are decoded",
			in:   "Caf&eacute; &amp; Bar &lt;offen&gt;.",
			want: "Café & Bar <offen>.",
		},
		{
			name: "non-breaking space collapses like any other space",
			in:   "Erste&nbsp;&nbsp;Zeile.",
			want: "Erste Zeile.",
		},
		{
			name: "a bare ampersand survives",
			in:   "Fish & chips.",
			want: "Fish & chips.",
		},
		{
			name: "a bare less-than is content, not a tag",
			in:   "a < b und b > c.",
			want: "a < b und b > c.",
		},
		{
			name: "comments are dropped",
			in:   "Vorher<!-- ein Kommentar -->nachher.",
			want: "Vorher nachher.",
		},
		{
			name: "script and style content never reaches the reader",
			in:   "<style>.a{color:red}</style>Echte Worte.<script>alert(1)</script>",
			want: "Echte Worte.",
		},
		{
			name: "unterminated markup swallows the rest rather than leaking a fragment",
			in:   "Sichtbar. <a href=\"https://example.test/unterminated",
			want: "Sichtbar.",
		},
		{
			// "</scriptx>" is ordinary text inside a script, not its end
			// tag: reading it as one would emit the real code that follows.
			name: "a near-miss end tag does not close a script",
			in:   "Vorher. <script>var s = '</scriptx>'; alert('geheim');</script> Nachher.",
			want: "Vorher. Nachher.",
		},
		{
			name: "a near-miss end tag does not close a style",
			in:   "Vorher. <style>.a::after{content:'</stylex>'}</style> Nachher.",
			want: "Vorher. Nachher.",
		},
		{
			// The end tag may carry attributes-like whitespace or a slash.
			name: "end tag closing on whitespace still closes the script",
			in:   "Vorher. <script>alert(1)</script > Nachher.",
			want: "Vorher. Nachher.",
		},
		{
			name: "an unterminated script never leaks its code",
			in:   "Sichtbar. <script>var geheim = 1; alert(geheim);",
			want: "Sichtbar.",
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

// TestDeriveExtractNeverEmitsMarkupOrExceedsTheBound is the licensing guard,
// stated as a property rather than as examples: whatever a feed puts in the
// body, the extract stays a bounded piece of prose. An unbounded extract
// would republish the article (extract-and-link permits no such thing), and a
// rune-sliced one would emit mid-tag fragments into a reader payload. The
// same two properties bind ingestion's write-time derivation of this rule;
// the two implementations must not drift apart.
func TestDeriveExtractNeverEmitsMarkupOrExceedsTheBound(t *testing.T) {
	t.Parallel()

	longProse := strings.Repeat("Πολύ μεγάλο άρθρο με πολλές προτάσεις. ", 200)
	bodies := map[string]string{
		"whole article in one paragraph": "<p>" + longProse + "</p>",
		"article as many paragraphs":     strings.Repeat("<p>"+longProse+"</p>", 20),
		"markup dense enough to land a tag on the boundary": strings.Repeat(
			`<a href="https://example.test/some/fairly/long/link/target">Wort</a> `, 400),
		"body that is entirely markup": strings.Repeat(`<div class="x">`, 500),
		"unterminated tag at the end":  longProse + `<a href="https://example.test/`,
		"script-heavy body":            strings.Repeat("<script>alert(1)</script>", 300) + "Worte.",
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := content.DeriveExtract(body)

			if n := utf8.RuneCountInString(got); n > 300 {
				t.Errorf("extract is %d runes; the 300-rune ceiling is the licensing bound, not a hint", n)
			}
			for _, marker := range []string{"<", ">", "href=", "alert(", "class="} {
				if strings.Contains(got, marker) {
					t.Errorf("extract contains %q - markup reached a reader payload: %q", marker, got)
				}
			}
		})
	}
}
