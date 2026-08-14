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

// lastSentenceEnd returns the index of the last sentence-closing rune within
// runes[:limit], or -1. A closer only counts when whitespace follows it, so
// a decimal point or an abbreviation glued to its next word ("z.B.") does
// not end a sentence mid-token. Callers guarantee len(runes) > limit, so
// runes[i+1] always exists.
func lastSentenceEnd(runes []rune, limit int) int {
	for i := limit - 1; i >= 0; i-- {
		if strings.ContainsRune(sentenceEnds, runes[i]) && unicode.IsSpace(runes[i+1]) {
			return i
		}
	}
	return -1
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
