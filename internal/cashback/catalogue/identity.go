// How an imported retailer gets an identity: [Slug], the URL-safe name the
// merchant table is keyed by, and [FallbackSlug], the one used when a name
// cannot produce one. One file, because a slug is a permanent, public,
// unique fact about a retailer and every rule that shapes it belongs where
// they can be read together.

package catalogue

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// ErrNoSlug reports a name and a fallback that between them produce nothing
// the merchant table would accept. It is not reachable from a validated
// [networks.ReportedMerchant] - the port already refuses a blank name and a
// blank external id - and exists because a slug built from an empty string
// would be a merchant nobody could ever link to.
var ErrNoSlug = errors.New("catalogue: nothing in the retailer's name or id can form a slug")

// slugMaxLength bounds what goes into the column. The schema sets no limit,
// but a slug appears in a URL a member is expected to share, and a
// two-hundred-character retailer name is a link nobody sends.
const slugMaxLength = 80

// Slug turns a retailer's name into the identifier the merchant table is
// keyed by, matching merchant_slug_format: lower-case alphanumerics in
// hyphen-separated groups.
//
// Accents are folded rather than dropped, so "Gärtner" becomes "gartner" and
// not "grtner". That is a transliteration and it is deliberately a lossy one:
// the slug is a URL, the name a member reads comes from merchant_copy, and
// the two have different jobs. Anything else that is not a letter or a digit
// becomes a separator, runs of separators collapse, and the result is
// trimmed to [slugMaxLength] at a group boundary so a truncated slug never
// ends in a hyphen.
//
// Greek is TRANSLITERATED first, along with the Latin characters that have
// no decomposition of their own - "Καταστήματα" becomes "katastimata" and
// "Weißenhaus" becomes "weissenhaus" (T259). Before that it produced,
// respectively, nothing at all and "wei-enhaus": every rune above
// unicode.MaxASCII is a separator to the loop below, so a character with no
// Latin form does not merely vanish, it breaks a word in half. See
// transliterate.go for the table and for why it runs before the fold.
//
// A script this still has no table for - Cyrillic, Han - folds to nothing,
// which is not an error here: the caller falls back to [FallbackSlug].
// Refusing the retailer instead would fail the whole import, and an import
// that fails is one whose absent routes cannot be reconciled.
func Slug(name string) string {
	// Composed first, so that the rune rules in transliterate see one rune
	// per character. A name can arrive decomposed - a Greek letter followed
	// by a separate combining tonos - and those rules read runes, including
	// the rune after next.
	composed, _, err := transform.String(norm.NFC, name)
	if err != nil {
		composed = name
	}

	folded, _, err := transform.String(
		transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC),
		transliterate(composed))
	if err != nil {
		// Chain only fails on malformed input, which a Go string built from
		// decoded JSON cannot be. Falling back to the untransliterated name
		// keeps the accented characters, which the loop below then drops - a
		// worse slug, never a wrong one.
		folded = name
	}

	var b strings.Builder
	b.Grow(len(folded))
	separated := false
	for _, r := range strings.ToLower(folded) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if separated && b.Len() > 0 {
				b.WriteByte('-')
			}
			separated = false
			b.WriteRune(r)
		default:
			// Everything else separates, including a non-Latin letter: it
			// has no place in a slug and it must not join two words that
			// were apart.
			separated = true
		}
	}
	return truncateSlug(b.String())
}

// truncateSlug bounds a slug without leaving it ending in a hyphen, cutting
// at the last group boundary that fits rather than mid-word.
func truncateSlug(slug string) string {
	if len(slug) <= slugMaxLength {
		return slug
	}
	cut := slug[:slugMaxLength]
	if i := strings.LastIndexByte(cut, '-'); i > 0 {
		return cut[:i]
	}
	return cut
}

// FallbackSlug is the identifier for a retailer whose name produces no slug,
// and the tie-breaker for two retailers whose names produce the same one.
//
// It is built from the network and the network's own id for the retailer,
// which is the one pair guaranteed to exist and to be unique
// (merchant_network_unique_per_network). So it is stable across imports: a
// retailer that got a fallback slug on Monday gets the same one on Tuesday,
// where a counter-based "-2" suffix would depend on which retailer was
// imported first and would move when one of them left.
func FallbackSlug(network, externalID string) (string, error) {
	slug := Slug(network + "-" + externalID)
	if slug == "" {
		return "", fmt.Errorf("%w: network %s, retailer %s",
			ErrNoSlug, strconv.Quote(network), strconv.Quote(externalID))
	}
	return slug, nil
}
