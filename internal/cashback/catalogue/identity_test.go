// The tests for identity.go. A slug is permanent, public and unique, so what
// is pinned here is not "it looks tidy" but the three properties a link
// depends on: it matches what the column accepts, it is the same tomorrow as
// today, and two retailers never quietly become one.

package catalogue_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
)

// merchantSlugFormat is merchant_slug_format from migration 0011, copied
// verbatim. Every slug this package produces is held to it here rather than
// discovered at INSERT time as a constraint violation with a name in it.
var merchantSlugFormat = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestSlugMatchesWhatTheColumnAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "an ordinary retailer", in: "Gartenhaus", want: "gartenhaus"},
		{name: "two words", in: "John Lewis", want: "john-lewis"},
		{name: "an accent is folded, not dropped", in: "Gärtner", want: "gartner"},
		{name: "several accents", in: "Café Crème", want: "cafe-creme"},
		{name: "punctuation separates", in: "Marks & Spencer", want: "marks-spencer"},
		{name: "runs of punctuation collapse", in: "A  --  B", want: "a-b"},
		{name: "leading and trailing noise is trimmed", in: "  !Zalando!  ", want: "zalando"},
		{name: "digits are kept", in: "Store 24", want: "store-24"},
		{name: "an ampersand does not join two words", in: "B&Q", want: "b-q"},
		// Both of these read "" and "zara" until T259. Greek now has a
		// table (transliterate.go); the scripts that still have none are in
		// TestAScriptWithNoTableStillFoldsToNothing.
		{name: "Greek transliterates rather than folding to nothing", in: "Καταστήματα", want: "katastimata"},
		{name: "mixed scripts keep both parts", in: "Zara Ελλάδα", want: "zara-ellada"},
		{name: "nothing at all", in: "", want: ""},
		{name: "only punctuation", in: "!!!", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := catalogue.Slug(tt.in)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got != "" && !merchantSlugFormat.MatchString(got) {
				t.Errorf("Slug(%q) = %q, which merchant_slug_format would refuse", tt.in, got)
			}
		})
	}
}

// TestALongNameIsCutAtAGroupBoundary: a slug is a URL a member is expected to
// share, and a truncation that ended in a hyphen would be refused by the
// column rather than merely look untidy.
func TestALongNameIsCutAtAGroupBoundary(t *testing.T) {
	t.Parallel()

	// Fifteen letters and a space is sixteen, which divides eighty: the
	// eightieth character of the slug is therefore a separator exactly, and
	// a truncation that did not look for a group boundary would leave the
	// slug ending in a hyphen - which merchant_slug_format refuses. A word
	// length that did not divide the bound would be cut mid-word and pass
	// whatever the code did, which is the test this one replaces.
	long := strings.TrimSpace(strings.Repeat("abcdefghijklmno ", 9))
	got := catalogue.Slug(long)

	if got == "" {
		t.Fatal("a long name produced no slug at all")
	}
	if len(got) > 80 {
		t.Errorf("Slug() = %q (%d characters), want no more than 80", got, len(got))
	}
	if !merchantSlugFormat.MatchString(got) {
		t.Errorf("Slug() = %q, which merchant_slug_format would refuse", got)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("Slug() = %q, which ends in a separator", got)
	}
}

// TestTheFallbackIsStableAcrossImports is the property that makes a fallback
// slug safe to put in a URL. A counter-based suffix would depend on which
// retailer was imported first and would move when one of them left the
// network; this one is derived from a pair the schema already keeps unique.
func TestTheFallbackIsStableAcrossImports(t *testing.T) {
	t.Parallel()

	first, err := catalogue.FallbackSlug("awin", "4471")
	if err != nil {
		t.Fatalf("FallbackSlug(): %v", err)
	}
	again, err := catalogue.FallbackSlug("awin", "4471")
	if err != nil {
		t.Fatalf("FallbackSlug() on a second import: %v", err)
	}
	if first != again {
		t.Errorf("the same retailer got %q and then %q", first, again)
	}
	if !merchantSlugFormat.MatchString(first) {
		t.Errorf("FallbackSlug() = %q, which merchant_slug_format would refuse", first)
	}

	// And two retailers at one network never collide, which is what the
	// merchant_network unique constraint already guarantees of the inputs.
	other, err := catalogue.FallbackSlug("awin", "4472")
	if err != nil {
		t.Fatalf("FallbackSlug(): %v", err)
	}
	if other == first {
		t.Errorf("two retailers both got %q", first)
	}
}

// TestAFallbackThatCouldNotFormASlugIsRefused covers the branch the port's
// own validation makes unreachable, because a slug built from an empty string
// would be a merchant nobody could ever link to.
func TestAFallbackThatCouldNotFormASlugIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := catalogue.FallbackSlug("", ""); !errors.Is(err, catalogue.ErrNoSlug) {
		t.Fatalf("FallbackSlug(\"\", \"\") = %v, want one wrapping ErrNoSlug", err)
	}
}

// TestTheSlugsThatMustNotMove is the fence around T259, and it is the test
// that is not in the spec.
//
// Transliteration was allowed to change exactly one class of name: the ones
// carrying a character that produced NOTHING before, either the empty slug
// or a silently broken word. Everything else - plain ASCII, and the Latin
// accents the fold has always decomposed - had to come out byte-identical,
// because a slug is a permanent public URL and a merchant already in the
// table keeps whatever it was given.
//
// So the wanted values below are not what this file thinks is right. They
// are what `origin/main` actually produced, captured by running Slug over
// this corpus before transliterate.go existed and written down verbatim.
// Any drift in the fold, the filter, the lower-casing or the truncation
// breaks this, whatever it does to the cases above.
func TestTheSlugsThatMustNotMove(t *testing.T) {
	t.Parallel()

	frozen := map[string]string{
		"Zara":            "zara",
		"Public":          "public",
		"e-shop.gr":       "e-shop-gr",
		"MediaMarkt":      "mediamarkt",
		"Douglas":         "douglas",
		"Zalando":         "zalando",
		"Otto":            "otto",
		"Tchibo":          "tchibo",
		"Hej Sverige":     "hej-sverige",
		"Coca-Cola  ":     "coca-cola",
		"  leading space": "leading-space",

		// The accent folding, which T259 was explicitly not allowed to
		// touch. Its doc comment chose "gartner" over "gaertner" on
		// purpose; the German characters that DO now change are the ones
		// NFD cannot decompose, and they are in transliterate_test.go.
		"Gärtner":         "gartner",
		"Café Crème":      "cafe-creme",
		"Élysée":          "elysee",
		"Nestlé":          "nestle",
		"Häagen-Dazs":     "haagen-dazs",
		"L'Oréal":         "l-oreal",
		"Müller Drogerie": "muller-drogerie",
		"Görtz":           "gortz",
		"Jägermeister":    "jagermeister",
		"Ähre & Korn":     "ahre-korn",
		"número 42":       "numero-42",
		"Ångström":        "angstrom",

		// Every Latin-1 accented capital in one string, so a fold that
		// stopped decomposing one of them fails here rather than on the one
		// retailer whose name carries it.
		"ÀÁÂÃÄÅÇÈÉÊËÌÍÎÏÑÒÓÔÕÖÙÚÛÜÝ": "aaaaaaceeeeiiiinooooouuuuy",

		// Separator handling, which transliteration inserts itself directly
		// in front of.
		"trailing---hyphens---": "trailing-hyphens",
	}

	for in, want := range frozen {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := catalogue.Slug(in); got != want {
				t.Errorf("Slug(%q) = %q, want %q - this slug shipped in a URL and must not move", in, got, want)
			}
		})
	}
}
