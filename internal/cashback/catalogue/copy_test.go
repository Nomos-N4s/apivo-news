// The fallback is labelled, and it is never blank and never invented
// (T101, T108, US5 scenario 2).
//
// Three failures, one test file. Only the blank is obvious; the other two
// are the ones a system trying to be helpful produces.

package catalogue_test

import (
	"errors"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
)

// aGermanMerchant is copy as a German-sourced merchant with a Greek
// translation would have it.
func aGermanMerchant() []catalogue.Available {
	return []catalogue.Available{
		{Language: "de", Name: "Möbelhaus Nord", Summary: "Möbel und Küchen.", Terms: "Ausgenommen Geschenkkarten."},
		{Language: "el", Name: "Έπιπλα Βορρά", Summary: "Έπιπλα και κουζίνες.", Terms: "Εξαιρούνται οι δωροκάρτες."},
	}
}

// TestAReaderGetsTheirOwnLanguageAndIsNotToldItIsAFallback.
func TestAReaderGetsTheirOwnLanguageAndIsNotToldItIsAFallback(t *testing.T) {
	t.Parallel()
	got, err := catalogue.Resolve("el", "de", aGermanMerchant())
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if got.Name != "Έπιπλα Βορρά" {
		t.Errorf("name = %q, want the Greek one", got.Name)
	}
	if got.Language != "el" {
		t.Errorf("language = %q, want el", got.Language)
	}
	if got.Fallback {
		t.Error("copy in the reader's own language is labelled a fallback")
	}
}

// TestAMissingTranslationShowsTheSourceLanguageAndSaysSo is US5 scenario 2.
// The label is the whole point: German copy and Greek copy rendered
// identically leave a Greek reader to work out for themselves why one card
// is in a language they may not read.
func TestAMissingTranslationShowsTheSourceLanguageAndSaysSo(t *testing.T) {
	t.Parallel()
	german := aGermanMerchant()[:1]

	got, err := catalogue.Resolve("el", "de", german)
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if got.Name != "Möbelhaus Nord" {
		t.Errorf("name = %q, want the German one", got.Name)
	}
	if got.Language != "de" {
		t.Errorf("language = %q, want de - the language it is actually in", got.Language)
	}
	if !got.Fallback {
		t.Error("the source language was shown to a Greek reader and not labelled a fallback")
	}
}

// TestNothingIsEverInvented. A merchant with no copy at all is an error, not
// an empty Copy: a caller that got a zero value and did not check would
// render a blank name, which is the failure this package exists to prevent.
// Nothing translates, transliterates, or dresses a slug up as a name.
func TestNothingIsEverInvented(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		want, source string
		available    []catalogue.Available
	}{
		"no copy at all": {"el", "de", nil},
		// Copy exists, but in neither the reader's language nor the source
		// one. Picking it would be this system deciding a member reads
		// French on no evidence.
		"copy in some third language": {"el", "de", []catalogue.Available{
			{Language: "fr", Name: "Meubles du Nord"},
		}},
		// The column refuses this, so it only arrives from a caller
		// assembling rows by hand - and a blank name on a member's screen
		// is exactly what must not happen.
		"a name that is only spaces": {"de", "de", []catalogue.Available{
			{Language: "de", Name: "   "},
		}},
	} {
		if got, err := catalogue.Resolve(c.want, c.source, c.available); !errors.Is(err, catalogue.ErrNoCopy) {
			t.Errorf("%s = %+v, %v; want an error wrapping %v", name, got, err, catalogue.ErrNoCopy)
		}
	}
}

// TestALanguageTagIsMatchedHoweverItIsCased. Tags reach here from a URL, a
// database column and a network's feed, and "EL" from one is the same
// language as "el" from another.
func TestALanguageTagIsMatchedHoweverItIsCased(t *testing.T) {
	t.Parallel()
	got, err := catalogue.Resolve("  EL  ", "DE", []catalogue.Available{
		{Language: "El", Name: "Έπιπλα Βορρά"},
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if got.Fallback {
		t.Error("a differently cased tag was treated as a different language")
	}
	if got.Language != "el" {
		t.Errorf("language = %q, want the normalised el", got.Language)
	}
}

// TestARegionalTagIsNotTheSameLanguage. A Brazilian reader shown European
// Portuguese should be TOLD it is a fallback, rather than have the
// difference hidden by a prefix match.
func TestARegionalTagIsNotTheSameLanguage(t *testing.T) {
	t.Parallel()
	got, err := catalogue.Resolve("pt-BR", "pt", []catalogue.Available{
		{Language: "pt", Name: "Móveis do Norte"},
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if !got.Fallback {
		t.Error("European Portuguese was shown to a pt-BR reader unlabelled")
	}
}

// TestOneCardIsInOneLanguage. A merchant with Greek copy that has no summary
// shows its Greek name and NO summary - not a German summary under a Greek
// name. Mixing languages inside one card is the fallback being unlabelled at
// field level, where no label can reach it.
func TestOneCardIsInOneLanguage(t *testing.T) {
	t.Parallel()
	got, err := catalogue.Resolve("el", "de", []catalogue.Available{
		{Language: "de", Name: "Möbelhaus Nord", Summary: "Möbel und Küchen.", Terms: "Ausgenommen Geschenkkarten."},
		{Language: "el", Name: "Έπιπλα Βορρά"},
	})
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if got.Summary != "" {
		t.Errorf("summary = %q, want none: it would have come from the German copy", got.Summary)
	}
	if got.Terms != "" {
		t.Errorf("terms = %q, want none: they would have come from the German copy", got.Terms)
	}
	if got.Name != "Έπιπλα Βορρά" {
		t.Errorf("name = %q, want the Greek one", got.Name)
	}
}

// TestACallerWithNoLanguageStillGetsALabel. A reader arriving with no
// language preference is still being shown copy in some language, and a
// surface that knows it is the source language can say so.
func TestACallerWithNoLanguageStillGetsALabel(t *testing.T) {
	t.Parallel()
	got, err := catalogue.Resolve("", "de", aGermanMerchant())
	if err != nil {
		t.Fatalf("Resolve(): %v", err)
	}
	if got.Language != "de" || !got.Fallback {
		t.Errorf("got %s labelled fallback=%v, want the source language labelled", got.Language, got.Fallback)
	}
}
