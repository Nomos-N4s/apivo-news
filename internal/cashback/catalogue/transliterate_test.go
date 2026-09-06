package catalogue_test

// ELOT 743 / ISO 843, exercised through [catalogue.Slug] rather than against
// transliterate directly - transliterate is unexported and this is the
// package's external test package, but that is not the reason. The reason is
// that transliterate's output is never seen by anything: what is written to
// cashback.merchant.slug is what Slug returns, and a table that was right in
// isolation and wrong after the fold and the filter would pass a direct test
// and ship a broken URL.

import (
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
)

// TestGreekTransliteratesToItsPassportSpelling. The standard is the one a
// Greek passport is written in, so a member reading a slug sees their
// retailer spelled the way their own documents spell their name - and, for
// the four real programmes below, the way the retailer spells itself in
// Latin script.
func TestGreekTransliteratesToItsPassportSpelling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Real Greek programme names from research.md, every one of which
		// produced "" before T259. The wanted values are the retailers' own
		// Latin spellings, which is the check that matters: ELOT 743 is only
		// the right standard if it lands on what the shop calls itself.
		{name: "a real programme, as the shop spells itself", in: "Πλαίσιο", want: "plaisio"},
		{name: "another, with an omicron-upsilon", in: "Σκρουτζ", want: "skroutz"},
		//nolint:misspell // "Germanos" is the retailer's own Latin name, not "Germans".
		{name: "and one ending in a final sigma", in: "Γερμανός", want: "germanos"},
		{name: "and one with a beta", in: "Κωτσόβολος", want: "kotsovolos"},

		{name: "the name that used to freeze the catalogue", in: "Καταστήματα", want: "katastimata"},
		{name: "mixed scripts now keep both parts", in: "Zara Ελλάδα", want: "zara-ellada"},

		// The multi-letter singles.
		{name: "theta is two letters", in: "Θεσσαλονίκη", want: "thessaloniki"},
		{name: "psi is two letters", in: "Ψυχή", want: "psychi"},
		{name: "chi is two letters", in: "Χίος", want: "chios"},
		{name: "xi is one", in: "Ξένος", want: "xenos"},

		// Final sigma is a different rune from medial sigma and the same
		// letter. A table that forgot it would drop the last letter of a
		// large share of Greek names.
		{name: "final sigma is still an s", in: "Στάσις", want: "stasis"},

		// The tonos marks stress, not a different sound, and a slug carries
		// no stress. Both composed and decomposed forms arrive from the wire.
		{name: "an accented vowel is the same letter", in: "Αθήνα", want: "athina"},
		{name: "the same name arriving decomposed", in: "Αθήνα", want: "athina"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := catalogue.Slug(tt.in)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !merchantSlugFormat.MatchString(got) {
				t.Errorf("Slug(%q) = %q, which merchant_slug_format would refuse", tt.in, got)
			}
		})
	}
}

// TestTheGreekVowelDigraphsChooseVOrF is the one rule in ELOT 743 that needs
// more than a rune map: αυ, ευ and ηυ take a v before a vowel or a voiced
// consonant and an f before anything else. Getting it backwards spells
// Nafplio "navplio", which is not how the town, or a shop named after it, is
// written anywhere.
func TestTheGreekVowelDigraphsChooseVOrF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "alpha-upsilon before a voiced consonant", in: "Αυγό", want: "avgo"},
		{name: "alpha-upsilon before an unvoiced one", in: "Ναύπλιο", want: "nafplio"},
		{name: "alpha-upsilon before a vowel", in: "Αύγουστος", want: "avgoustos"},
		{name: "epsilon-upsilon before a voiced consonant", in: "Ευρώπη", want: "evropi"},
		{name: "epsilon-upsilon before an unvoiced one", in: "Ευκάλυπτος", want: "efkalyptos"},
		{name: "epsilon-upsilon before a vowel", in: "Εύβοια", want: "evvoia"},
		// Nothing follows the pair at all, which is the unvoiced case and
		// the one an index-out-of-range would find.
		{name: "a digraph at the very end of the name", in: "Ταυ", want: "taf"},

		// The consonant digraphs, which need no lookahead.
		{name: "double gamma is ng", in: "Άγγελος", want: "angelos"},
		{name: "gamma-chi is nch", in: "Έλεγχος", want: "elenchos"},
		{name: "gamma-xi is nx", in: "Έλεγξη", want: "elenxi"},

		// THE DIAERESIS CASE, and the reason transliterate runs before the
		// fold rather than after it. A diaeresis says these two vowels are
		// NOT a digraph; the fold strips it, so a table applied afterwards
		// would read "προϋπ" as the ου digraph and spell it "proup".
		{name: "a diaeresis breaks the omicron-upsilon pair", in: "Προϋπολογισμός", want: "proypologismos"},
		{name: "a diaeresis breaks an iota pair too", in: "Μαϊου", want: "maiou"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := catalogue.Slug(tt.in); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestTheLatinCharactersThatDoNotDecompose is the half of T259 that is easy
// to miss, because it does not produce an empty slug - it produces a
// plausible-looking wrong one, and the fallback never fires.
//
// Every `before` below is what a deployment without this change writes to
// cashback.merchant.slug, permanently. "Ærø" reducing to "r" is the one to
// look at twice: the column is globally unique, so a one-letter slug is a
// collision waiting for the second retailer whose name reduces the same way.
func TestTheLatinCharactersThatDoNotDecompose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		before string
		want   string
	}{
		{name: "sharp s joins the word it used to split", in: "Weißenhaus", before: "wei-enhaus", want: "weissenhaus"},
		{name: "and at the end of one", in: "Straße", before: "stra-e", want: "strasse"},
		{name: "a slashed o", in: "Smørrebrød", before: "sm-rrebr-d", want: "smorrebrod"},
		{name: "ash and slashed o together left one letter", in: "Ærø", before: "r", want: "aero"},
		{name: "an oe ligature", in: "Œuvre", before: "uvre", want: "oeuvre"},
		{name: "a barred l", in: "Łódź", before: "odz", want: "lodz"},
		{name: "a barred d", in: "Đakovo", before: "akovo", want: "dakovo"},
		{name: "a thorn", in: "Þingvellir", before: "ingvellir", want: "thingvellir"},
		{name: "an eth", in: "Ðanmark", before: "anmark", want: "danmark"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := catalogue.Slug(tt.in)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got == tt.before {
				t.Errorf("Slug(%q) still returns the pre-T259 value %q", tt.in, tt.before)
			}
		})
	}
}

// TestATransliteratedSlugIsStillAValidSlug. Transliteration is the first
// thing that makes one rune produce more than one byte - θ becomes "th", ψ
// becomes "ps" - so the 80-BYTE bound and the group-boundary truncation are
// worth re-proving over an alphabet that expands.
func TestATransliteratedSlugIsStillAValidSlug(t *testing.T) {
	t.Parallel()

	// Ψυχή is four Greek runes and eight Unicode bytes, and transliterates
	// to five ASCII ones. Repeated past the bound it is a name that expands
	// on every axis at once.
	long := strings.TrimSpace(strings.Repeat("Ψυχή Θεσσαλονίκη ", 12))
	got := catalogue.Slug(long)

	if got == "" {
		t.Fatal("a long Greek name produced no slug at all")
	}
	if len(got) > 80 {
		t.Errorf("Slug() = %q (%d bytes), want no more than 80", got, len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("Slug() = %q, which ends in a separator", got)
	}
	if !merchantSlugFormat.MatchString(got) {
		t.Errorf("Slug() = %q, which merchant_slug_format would refuse", got)
	}
}

// TestAScriptWithNoTableStillFoldsToNothing. T259 added Greek and eight
// Latin characters; it did not add Cyrillic or Han, and the fallback path
// those depend on must still work. A change that made Slug never return ""
// would leave FallbackSlug unreachable and its refusal untested.
func TestAScriptWithNoTableStillFoldsToNothing(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"Магазин", "商店", "متجر"} {
		if got := catalogue.Slug(in); got != "" {
			t.Errorf("Slug(%q) = %q, want \"\" so the caller falls back", in, got)
		}
	}
}
