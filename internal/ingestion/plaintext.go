package ingestion

import (
	"html"
	"strings"
)

// blockTags are the HTML elements that separate blocks of text. They are
// replaced by a space so two paragraphs do not run together into one word,
// while inline elements ("<b>", "<a>", "<em>") are removed without a space
// so "z.B.<b>fett</b>" stays a single token rather than gaining a boundary
// its author never wrote.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "caption": true, "dd": true, "div": true, "dl": true,
	"dt": true, "figcaption": true, "figure": true, "footer": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hr": true, "li": true, "main": true, "nav": true,
	"ol": true, "p": true, "pre": true, "section": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"tr": true, "ul": true,
}

// droppedTags are elements whose CONTENT is not prose and must not reach an
// extract: stripping only the tags would leave script or stylesheet source
// in reader-facing text.
var droppedTags = map[string]bool{"script": true, "style": true}

// plainText reduces feed markup to the prose an extract is made of: tags
// removed, character entities decoded, whitespace collapsed to single
// spaces. The retrieved body keeps its original markup as evidence
// (source_item.raw_body); this is the display and translation text.
//
// Deriving the extract from prose rather than from raw markup is what keeps
// the D9 boundary rules honest: sentence and word cuts operate on sentences
// and words, and no cut can land inside a tag and emit an unbalanced
// fragment.
//
// It is deliberately a small, dependency-free scanner rather than a full
// HTML parser: it must be deterministic and total, never failing on the
// malformed markup real feeds carry.
func plainText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != '<' {
			b.WriteByte(s[i])
			i++
			continue
		}
		name, end := scanTag(s, i)
		if end < 0 {
			// A '<' that opens no tag is literal text ("a < b").
			b.WriteByte(s[i])
			i++
			continue
		}
		if droppedTags[name] {
			// Skip the element's content as well as its tags.
			if closed := indexClosingTag(s, end, name); closed >= 0 {
				i = closed
				continue
			}
			// Unclosed: nothing after it is trustworthy prose.
			break
		}
		if blockTags[name] {
			b.WriteByte(' ')
		}
		i = end
	}

	// Entities are decoded AFTER tag removal, never before: a feed that
	// escaped "&lt;p&gt;" wrote literal text about a tag, and decoding
	// first would turn it into markup this scanner then deleted.
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

// scanTag reads the tag starting at s[i] == '<'. It returns the lower-cased
// element name and the index just past the tag's '>', or ("", -1) when this
// is not a tag. Quoted attribute values may contain '>' and are respected.
func scanTag(s string, i int) (string, int) {
	j := i + 1
	if j < len(s) && s[j] == '/' { // closing tag
		j++
	}
	nameStart := j
	for j < len(s) && isTagNameByte(s[j]) {
		j++
	}
	if j == nameStart {
		return "", -1 // "< " or "<3": not a tag
	}
	name := strings.ToLower(s[nameStart:j])

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
	return "", -1 // unterminated
}

// indexClosingTag returns the index just past "</name>" at or after from, or
// -1 when the element is never closed.
func indexClosingTag(s string, from int, name string) int {
	for i := from; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		found, end := scanTag(s, i)
		if end >= 0 && found == name && i+1 < len(s) && s[i+1] == '/' {
			return end
		}
	}
	return -1
}

func isTagNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
