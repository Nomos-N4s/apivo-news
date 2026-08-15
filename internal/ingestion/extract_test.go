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

// TestDescriptionOnlyFeedIsNotReproducedInFull walks the licensing case end
// to end, the shape the bound exists for: a feed whose items carry only a
// long description. normalise copies that description into Body as the
// retrieved evidence, so summary and body are the same long text and there
// is nothing shorter to fall back to. The extract must still be an extract.
func TestDescriptionOnlyFeedIsNotReproducedInFull(t *testing.T) {
	t.Parallel()

	article := strings.TrimSpace(strings.Repeat("Το πλήρες άρθρο συνεχίζεται. ", 500))
	feed := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<rss version="2.0"><channel><title>t</title><link>https://example.test/</link>` +
		`<description>d</description><item><title>Άρθρο</title>` +
		`<link>https://example.test/a</link><description>` + article +
		`</description></item></channel></rss>`

	items, err := ingestion.ParseFeed(strings.NewReader(feed))
	if err != nil {
		t.Fatalf("ParseFeed() error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ParseFeed() returned %d items, want 1", len(items))
	}

	// The evidence keeps the whole retrieved text; only the extract is
	// bounded.
	if got := utf8.RuneCountInString(items[0].Body); got != utf8.RuneCountInString(article) {
		t.Fatalf("Body holds %d runes, want the whole retrieved text (%d)", got, utf8.RuneCountInString(article))
	}
	extract := ingestion.DeriveExtract(items[0].Summary, items[0].Body)
	if n := utf8.RuneCountInString(extract); n > 300 {
		t.Fatalf("extract is %d runes: a description-only feed must not be reproduced in full", n)
	}
	if extract == "" {
		t.Fatal("extract is empty; the rule must still derive one")
	}
}

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
			// The licensing bound: a description-only feed carries the
			// whole article in its summary, and returning it verbatim
			// would reproduce the article in full under a licence that
			// permits an extract and a link.
			name:    "summary longer than the window is bounded like any other text",
			summary: strings.Repeat("s", 400),
			body:    "irrelevant",
			want:    strings.Repeat("s", 300),
		},
		{
			name:    "long summary cut at its last sentence boundary",
			summary: strings.Repeat("σ", 250) + ". " + strings.Repeat("τ", 200),
			body:    "irrelevant",
			want:    strings.Repeat("σ", 250) + ".",
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
			// The closer fits, its closing quote does not: the sentence
			// boundary still counts, cut at the closer.
			name: "trailing quote falls outside the window",
			body: strings.Repeat("α", 299) + `." ` + strings.Repeat("β", 100),
			want: strings.Repeat("α", 299) + ".",
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
			name: "closing tag between sentences",
			body: strings.Repeat("α", 240) + ".</p><p>" + strings.Repeat("β", 100),
			want: strings.Repeat("α", 240) + ".",
		},
		{
			// A void tag separates sentences at least as often as a
			// closing tag does; stripping markup makes both a space.
			name: "void break tag between sentences",
			body: strings.Repeat("α", 240) + ".<br>" + strings.Repeat("β", 100),
			want: strings.Repeat("α", 240) + ".",
		},
		{
			name: "self-closing break tag between sentences",
			body: strings.Repeat("α", 240) + ".<br />" + strings.Repeat("β", 100),
			want: strings.Repeat("α", 240) + ".",
		},
		{
			name: "opening paragraph tag between sentences",
			body: strings.Repeat("α", 240) + ".<p>" + strings.Repeat("β", 100),
			want: strings.Repeat("α", 240) + ".",
		},
		{
			name: "heading tag between sentences",
			body: strings.Repeat("α", 240) + ".<h2>" + strings.Repeat("β", 100),
			want: strings.Repeat("α", 240) + ".",
		},
		{
			name: "closing quote between the sentence end and markup",
			body: strings.Repeat("γ", 200) + `."</blockquote>` + strings.Repeat("δ", 200),
			want: strings.Repeat("γ", 200) + `."`,
		},
		{
			name: "greek closing quote before whitespace",
			body: strings.Repeat("ε", 180) + ".» " + strings.Repeat("ζ", 200),
			want: strings.Repeat("ε", 180) + ".»",
		},
		{
			// No space before the tag: the bracket-then-markup path is
			// what closes this sentence, not the whitespace rule.
			name: "closing bracket before markup",
			body: strings.Repeat("h", 210) + ".)<br/>" + strings.Repeat("i", 200),
			want: strings.Repeat("h", 210) + ".)",
		},
		{
			// Inline markup carries no whitespace, so "z.B." stays one
			// token and its period closes nothing; the cut falls back to
			// the word boundary before it.
			name: "abbreviation glued to inline markup does not end a sentence",
			body: strings.Repeat("x", 280) + " z.B.<b>fett</b> " + strings.Repeat("y", 100),
			want: strings.Repeat("x", 280) + " z.B.fett",
		},
		{
			// The fallbacks operate on prose, so no cut can land inside a
			// tag and emit an unbalanced fragment as the extract.
			name: "word-boundary fallback on an html body emits no markup",
			body: "<p>" + strings.Repeat("ω", 280) + ` <a href="https://example.test/very/long/target">` +
				strings.Repeat("ψ", 100) + "</a></p>",
			want: strings.Repeat("ω", 280),
		},
		{
			name: "entities are decoded and markup removed",
			body: "<p>Caf&eacute; &amp; Bar &lt;p&gt; ist ge&ouml;ffnet.</p>",
			want: "Café & Bar <p> ist geöffnet.",
		},
		{
			name: "script content never reaches the extract",
			body: "<p>Vor dem Skript.</p><script>var x = 'geheim';</script><p>Danach.</p>",
			want: "Vor dem Skript. Danach.",
		},
		{
			name: "whitespace across markup collapses to single spaces",
			body: "<div>\n  Erste Zeile.\n</div>\n<div>\n  Zweite Zeile.\n</div>",
			want: "Erste Zeile. Zweite Zeile.",
		},
		{
			// A '<' that opens no element is literal text, not markup.
			name: "bare less-than is kept as text",
			body: "<p>2 < 3 und 5 > 4.</p>",
			want: "2 < 3 und 5 > 4.",
		},
		{
			// '>' inside a quoted attribute does not end the tag.
			name: "quoted attribute containing a bracket",
			body: `<p><a title="a > b" href="https://e.test/x">Text</a> danach.</p>`,
			want: "Text danach.",
		},
		{
			name: "unterminated tag is not markup",
			body: "<p>Vorher.</p><a href=\"unclosed",
			want: `Vorher. <a href="unclosed`,
		},
		{
			// Nothing after an unclosed script is trustworthy prose.
			name: "unclosed script drops the remainder",
			body: "<p>Sichtbar.</p><script>var x = 1;",
			want: "Sichtbar.",
		},
		{
			name: "style content never reaches the extract",
			body: "<style>.a { color: red; }</style><p>Nur der Text.</p>",
			want: "Nur der Text.",
		},
		{
			name: "markup-only body derives nothing",
			body: "<p></p><br/><div></div>",
			want: "",
		},
		{
			name:    "summary is stripped too, not only the body",
			summary: "<p>Η <b>περίληψη</b> της ροής.</p>",
			body:    "irrelevant",
			want:    "Η περίληψη της ροής.",
		},
		{
			// A summary that strips to nothing is no summary: the body
			// still has to produce the extract.
			name:    "markup-only summary falls through to the body",
			summary: "<p><br/></p>",
			body:    "Ein kurzer Text.",
			want:    "Ein kurzer Text.",
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
			// The licensing bound holds for EVERY extract, whichever text
			// it was derived from: an extract-and-link licence permits an
			// extract, never a full reproduction.
			if n := utf8.RuneCountInString(got); n > 300 {
				t.Fatalf("derived extract is %d runes, want <= 300", n)
			}
			// No extract ever carries markup: a cut that landed inside a
			// tag would emit an unbalanced fragment to readers.
			if strings.ContainsAny(got, "<>") && !strings.Contains(tt.want, "<") {
				t.Fatalf("derived extract carries markup: %q", got)
			}
		})
	}
}
