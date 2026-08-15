package content

import (
	"strings"
	"unicode"
)

// maxExtractRunes bounds the derived extract (research D9): extract-and-link
// must stay defensibly "extract", so the bound is part of the rule, not a
// presentation choice. Counted in runes - Greek text is not shorter for
// taking more bytes.
const maxExtractRunes = 300

// DeriveExtract derives the reader-facing extract for an untranslated
// (same-language) article from the retrieved raw body, per research D9: the
// first sentences of the text, up to 300 characters, cut at a sentence
// boundary. (The feed-summary preference in D9 is ingestion's side of the
// rule; at read time the stored body is all there is.)
//
// The rule is deliberately deterministic and dumb: whitespace runs collapse
// to single spaces, a sentence ends at a terminator (. ! ? ; …) followed by
// a space or the end of text, and when not even one sentence fits the text
// is cut at the last word boundary that leaves room for an ellipsis. The
// semicolon is a terminator because it is the Greek question mark.
func DeriveExtract(rawBody string) string {
	text := strings.Join(strings.Fields(rawBody), " ")
	runes := []rune(text)
	if len(runes) <= maxExtractRunes {
		return text
	}

	// The last sentence boundary within the bound wins: as many whole
	// sentences as fit.
	end := 0
	for i := 0; i < maxExtractRunes; i++ {
		if isSentenceTerminator(runes[i]) && (i+1 == len(runes) || runes[i+1] == ' ') {
			end = i + 1
		}
	}
	if end > 0 {
		return strings.TrimRight(string(runes[:end]), " ")
	}

	// No sentence boundary in reach: cut at the last word boundary that
	// keeps the ellipsis inside the bound.
	cut := maxExtractRunes - 1
	for cut > 0 && !unicode.IsSpace(runes[cut]) {
		cut--
	}
	if cut == 0 {
		// One unbroken 300-rune word; a hard cut is all that is left.
		cut = maxExtractRunes - 1
	}
	return strings.TrimRight(string(runes[:cut]), " ") + "…"
}

// isSentenceTerminator reports whether r can end a sentence. The semicolon
// covers both U+003B (the character Greek keyboards produce for the Greek
// question mark) and U+037E (the dedicated erotimatiko codepoint).
func isSentenceTerminator(r rune) bool {
	switch r {
	case '.', '!', '?', ';', '…', ';':
		return true
	}
	return false
}
