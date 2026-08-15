package ingestion

import (
	"strings"
	"unicode"
)

// extractMaxRunes bounds a body-derived extract (research D9): at most 300
// characters, counted as runes so Greek text is measured in letters, not
// UTF-8 bytes. Changing this bound is a founder decision, not a tuning knob.
const extractMaxRunes = 300

// sentenceEnds are the rune classes that close a sentence for the D9 cut.
// The ASCII semicolon is included deliberately: Greek writes its question
// mark as a semicolon (and U+037E normalises to it), so excluding it would
// run Greek questions together. For German text a semicolon is a clause
// boundary - an acceptable, still-deterministic cut.
const sentenceEnds = ".!?…;;"

// DeriveExtract implements the D9 extract-derivation rule as one
// deterministic, pure function:
//
//  1. the feed's own summary, whenever it provides one, verbatim;
//  2. otherwise the first sentences of the body, up to 300 characters, cut
//     at the last sentence boundary inside that window;
//  3. when no sentence ends inside the window, the cut falls back to the
//     last word boundary, and only for unbroken text to a hard 300-rune cut.
//
// It operates on the text it is given and never reaches for more: an item
// with neither summary nor body derives an empty extract. The source-link
// suffix that D9 requires is presentation, applied where the extract is
// rendered, so it is not baked into the derived text here.
func DeriveExtract(summary, body string) string {
	if s := strings.TrimSpace(summary); s != "" {
		return s
	}
	text := strings.TrimSpace(body)
	runes := []rune(text)
	if len(runes) <= extractMaxRunes {
		return text
	}
	if end := lastSentenceEnd(runes, extractMaxRunes); end >= 0 {
		return strings.TrimSpace(string(runes[:end+1]))
	}
	if space := lastSpace(runes[:extractMaxRunes]); space >= 0 {
		return strings.TrimSpace(string(runes[:space]))
	}
	return string(runes[:extractMaxRunes])
}

// sentenceTrailers are the runes that may sit between a sentence closer and
// whatever follows it, and still belong to the sentence: the closing quotes
// and brackets Greek, German and English feeds use.
const sentenceTrailers = `"'”’»)]`

// lastSentenceEnd returns the index of the last rune belonging to the last
// complete sentence within runes[:limit], or -1. The returned index includes
// any closing quote or bracket that trails the closer, so the extract keeps
// the punctuation the author wrote.
//
// A closer only counts when the sentence genuinely ends there: whitespace or
// a closing markup tag must follow, optionally past those trailers. Feed
// bodies routinely carry HTML, where a sentence ends as ".</p>" with no
// space at all, so requiring bare whitespace would miss most real boundaries
// and fall back to a word cut. Requiring a CLOSING tag keeps an abbreviation
// glued to inline markup ("z.B.<b>fett</b>") from ending a sentence, as does
// the existing rule for a decimal point or "z.B. " mid-token.
func lastSentenceEnd(runes []rune, limit int) int {
	for i := limit - 1; i >= 0; i-- {
		if !strings.ContainsRune(sentenceEnds, runes[i]) {
			continue
		}
		end, ok := sentenceEndsAt(runes, i)
		if !ok {
			continue
		}
		if end >= limit {
			// The closer is inside the window but its trailing quote or
			// bracket is not: cut at the closer rather than discarding a
			// real sentence boundary for a character that does not fit.
			return i
		}
		return end
	}
	return -1
}

// sentenceEndsAt reports whether the closer at index i ends a sentence, and
// returns the index of the last rune belonging to that sentence (the closer
// itself, or the last trailing quote or bracket after it).
func sentenceEndsAt(runes []rune, i int) (int, bool) {
	end := i
	for j := i + 1; j < len(runes); j++ {
		switch {
		case unicode.IsSpace(runes[j]):
			return end, true
		case runes[j] == '<':
			// A closing tag ends the sentence; an inline opening tag
			// ("z.B.<b>") continues it.
			return end, j+1 < len(runes) && runes[j+1] == '/'
		case strings.ContainsRune(sentenceTrailers, runes[j]):
			end = j
		default:
			return end, false
		}
	}
	// The text ends here, so the sentence does too.
	return end, true
}

// lastSpace returns the index of the last whitespace rune, or -1.
func lastSpace(runes []rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if unicode.IsSpace(runes[i]) {
			return i
		}
	}
	return -1
}
