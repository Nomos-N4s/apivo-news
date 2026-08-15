package content

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxExtractRunes bounds the derived extract (research D9): extract-and-link
	// must stay defensibly "extract", so the bound is part of the rule, not a
	// presentation choice. Counted in runes - Greek text is not shorter for
	// taking more bytes. Every return path below is capped by it; a derivation
	// that could hand back a whole article would be a licensing problem, not
	// merely an ugly one.
	maxExtractRunes = 300
	// scanRunes is one past the bound: enough normalised text to apply the
	// sentence rule across the whole window AND to know whether anything
	// followed it. Nothing beyond this is ever normalised, so a multi-megabyte
	// body costs the same as a short one.
	scanRunes = maxExtractRunes + 1
	// maxEntityBytes caps how far a '&' is allowed to look for its ';'. Real
	// character references are far shorter; the cap keeps a stray ampersand
	// from scanning the rest of the body.
	maxEntityBytes = 32
)

// Where a pending space came from. Whitespace the source really contained is
// always reproduced; one synthesised at a markup boundary yields to
// punctuation.
const (
	spaceNone = iota
	spaceText
	spaceTag
)

// isClosingPunctuation reports whether r is punctuation that never takes a
// space before it, so a tag boundary immediately before it emits none.
func isClosingPunctuation(r rune) bool {
	switch r {
	case '.', ',', ';', ':', '!', '?', ')', ']', '}', '…', '»', '”', '’', ';':
		return true
	}
	return false
}

// DeriveExtract derives the reader-facing extract for an untranslated
// (same-language) article from the retrieved raw body, per research D9: the
// first sentences of the text, up to 300 characters, cut at a sentence
// boundary. (The feed-summary preference in D9 is ingestion's side of the
// rule; at read time the stored body is all there is.)
//
// The rule is deliberately deterministic and dumb: markup is dropped,
// character references are decoded, whitespace runs collapse to single
// spaces, a sentence ends at a terminator (. ! ? ; …) followed by a space or
// the end of text, and when not even one sentence fits the text is cut at the
// last word boundary that leaves room for an ellipsis. The semicolon is a
// terminator because it is the Greek question mark.
//
// Markup is dropped rather than sliced: raw_body holds the item body as the
// feed served it, which routinely carries HTML, and cutting a rune window out
// of that would emit mid-tag fragments like `<a href="htt` into a reader
// payload. Dropping tags makes the extract what the name promises - the
// article's words - and makes the 300-rune bound a bound on prose rather than
// on markup.
//
// This is the read-time half of D9. Ingestion derives the same extract at
// write time (for translation input and for editorial review), and the two
// must agree: same ceiling, same sentence rule, same plain-text output. A
// change to either belongs in both.
func DeriveExtract(rawBody string) string {
	runes := normalisePrefix(rawBody, scanRunes)
	if len(runes) <= maxExtractRunes {
		// The whole body fits - and is therefore already within the bound.
		return string(runes)
	}

	// The last sentence boundary within the bound wins: as many whole
	// sentences as fit. runes[i+1] is always in range because len(runes) is
	// scanRunes here, one past the window being scanned.
	end := 0
	for i := range maxExtractRunes {
		if isSentenceTerminator(runes[i]) && runes[i+1] == ' ' {
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

// normalisePrefix walks rawBody once and returns at most limit runes of plain
// text: markup dropped, character references decoded, whitespace runs
// collapsed to single spaces, no leading or trailing space. It stops as soon
// as it has limit runes.
//
// Prose therefore costs the limit, not the body. Markup does not: finding the
// terminator of a comment or of a script or style element means scanning to
// it, and an unterminated one means scanning to the end. That scan is linear
// overall - every byte it passes is a byte the walk then skips, so no byte is
// examined more than a constant number of times - and it buys the guarantee
// that no code ever reaches a reader. A body that opens with an unterminated
// script is the worst case: one pass over a column already fetched.
func normalisePrefix(rawBody string, limit int) []rune {
	out := make([]rune, 0, limit)
	// Whitespace is emitted lazily: a run of it becomes at most one space,
	// and only once a non-space rune follows. That drops leading and
	// trailing space without a second pass. The source of the pending space
	// matters, so the two are tracked apart - see emit.
	pending := spaceNone

	emit := func(r rune) bool {
		if unicode.IsSpace(r) {
			if len(out) > 0 {
				pending = spaceText
			}
			return true
		}
		// A space the source actually contained is always honoured. One
		// synthesised at a tag boundary is dropped before punctuation:
		// "<a>Der Text</a>." reads "Der Text.", not "Der Text .".
		if pending == spaceText || (pending == spaceTag && !isClosingPunctuation(r)) {
			out = append(out, ' ')
			if len(out) == limit {
				pending = spaceNone
				return false
			}
		}
		pending = spaceNone
		out = append(out, r)
		return len(out) < limit
	}

	for i := 0; i < len(rawBody); {
		if isMarkupStart(rawBody, i) {
			i = skipMarkup(rawBody, i)
			// A tag separates words: <p>a</p><p>b</p> reads as "a b".
			if len(out) > 0 && pending == spaceNone {
				pending = spaceTag
			}
			continue
		}
		if rawBody[i] == '&' {
			if decoded, next, ok := decodeEntity(rawBody, i); ok {
				full := true
				for _, r := range decoded {
					if !emit(r) {
						full = false
						break
					}
				}
				if !full {
					return out
				}
				i = next
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(rawBody[i:])
		if !emit(r) {
			return out
		}
		i += size
	}
	return out
}

// isMarkupStart reports whether a tag, comment or processing instruction
// begins at i. A bare '<' that cannot start one - as in "a < b" - is content,
// not markup, and is left alone.
func isMarkupStart(s string, i int) bool {
	if s[i] != '<' || i+1 >= len(s) {
		return false
	}
	c := s[i+1]
	return c == '/' || c == '!' || c == '?' || isASCIILetter(c)
}

// skipMarkup returns the index just past the markup starting at i. For script
// and style it also skips the element's text, which is code, not prose, and
// has no business in a reader extract.
func skipMarkup(s string, i int) int {
	if strings.HasPrefix(s[i:], "<!--") {
		if end := strings.Index(s[i+4:], "-->"); end >= 0 {
			return i + 4 + end + 3
		}
		return len(s)
	}

	// Whether this is an end tag decides whether the script/style rule below
	// applies: without the check, </style> would be read as another style
	// element and swallow everything after it.
	isEndTag := i+1 < len(s) && s[i+1] == '/'
	name, after := tagName(s, i)
	// Unterminated markup: the rest of the body is markup, so nothing more
	// is content.
	if after == len(s) {
		return len(s)
	}
	if !isEndTag && (name == "script" || name == "style") {
		if end := indexEndTag(s[after:], name); end >= 0 {
			// The recursion lands on the end tag, which the check above
			// sends straight down the ordinary path.
			return skipMarkup(s, after+end)
		}
		// No terminator anywhere: the remainder is script or style text,
		// and none of it is content.
		return len(s)
	}
	return after
}

// tagName returns the lower-cased element name of the tag starting at i and
// the index just past the tag's '>'. Quoted attribute values may contain '>',
// so the scan tracks quoting.
func tagName(s string, i int) (string, int) {
	j := i + 1
	if j < len(s) && s[j] == '/' {
		j++
	}
	start := j
	for j < len(s) && (isASCIILetter(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	name := strings.ToLower(s[start:j])

	var quote byte
	for ; j < len(s); j++ {
		switch {
		case quote != 0:
			if s[j] == quote {
				quote = 0
			}
		case s[j] == '"' || s[j] == '\'':
			quote = s[j]
		case s[j] == '>':
			return name, j + 1
		}
	}
	return name, len(s)
}

// decodeEntity decodes the character reference starting at the '&' at i. It
// reports failure for anything that is not a recognised reference, so a bare
// ampersand stays a bare ampersand.
func decodeEntity(s string, i int) (string, int, bool) {
	end := len(s)
	if i+maxEntityBytes < end {
		end = i + maxEntityBytes
	}
	semi := strings.IndexByte(s[i:end], ';')
	if semi < 0 {
		return "", 0, false
	}
	token := s[i : i+semi+1]
	decoded := html.UnescapeString(token)
	if decoded == token {
		return "", 0, false
	}
	return decoded, i + semi + 1, true
}

// indexFold is strings.Index for an ASCII-lowercase needle, matched
// case-insensitively - "</SCRIPT>" closes a script element too.
func indexFold(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if strings.EqualFold(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// indexEndTag returns the offset of the end tag closing element name, or -1.
//
// The name must be followed by a character that can actually terminate it:
// per the HTML syntax an end tag ends at whitespace, '/' or '>'. Matching the
// name as a bare substring would read "</scriptx>" - legal text inside a
// script - as the close, and everything after it, real script code, would be
// emitted to the reader as prose.
func indexEndTag(haystack, name string) int {
	needle := "</" + name
	for i := 0; ; {
		rel := indexFold(haystack[i:], needle)
		if rel < 0 {
			return -1
		}
		at := i + rel
		next := at + len(needle)
		if next >= len(haystack) {
			// Truncated end tag: nothing usable follows it either way.
			return at
		}
		if c := haystack[next]; c == '>' || c == '/' || c == ' ' || c == '\t' ||
			c == '\n' || c == '\r' || c == '\f' {
			return at
		}
		i = next
	}
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
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
