// Transliterating the characters that would otherwise contribute NOTHING to
// a slug (T259).
//
// A file of its own, beside identity.go rather than inside it, because this
// is a table of a national standard and the slug rules are a paragraph of
// judgement. Reading either should not mean scrolling the other.
//
// THE SCOPE IS DELIBERATELY NARROW. Every rune here is one that
// [Slug]'s filter drops today - it is not a letter below unicode.MaxASCII -
// and therefore acts as a SEPARATOR. So the characters this file speaks for
// are exactly the ones that currently either produce nothing at all (Greek:
// "Καταστήματα" slugs to "") or silently break a word in half (German:
// "Weißenhaus" slugs to "wei-enhaus" on a deployment that has not run this).
//
// It does NOT touch the accent folding in identity.go. That folding
// deliberately chose "Gärtner" -> "gartner" over "gaertner", and a slug is a
// permanent public URL: quietly rewriting every one a German retailer already
// holds is a bigger change than the one this file makes.

package catalogue

import "strings"

// transliterate rewrites the runes that a slug would otherwise lose, and
// leaves every other rune exactly as it found it.
//
// RUN BEFORE THE FOLD, not after. Running after would halve this table -
// the fold strips Greek tonos, so "ά" would already be "α" - and it would
// also be wrong. Greek writes a diaeresis precisely to say that two vowels
// are NOT a digraph, and the fold removes it: "αϋ" would arrive here as "αυ"
// and be read as the digraph it was explicitly marked not to be. The
// diaeresis has to still be present at the moment the digraph is decided.
//
// The output is lower-case Latin. [Slug] lower-cases anyway, so this only
// means the table needs no upper-case column.
func transliterate(s string) string {
	// Runes, not bytes: every rule below is a rune rule, and one of them
	// needs to look at the rune after next.
	in := []rune(s)
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(in); i++ {
		if latin, ok := digraph(in, i); ok {
			b.WriteString(latin)
			i++ // The pair is consumed; the loop's i++ takes the second.
			continue
		}
		if latin, ok := transliterated[in[i]]; ok {
			b.WriteString(latin)
			continue
		}
		b.WriteRune(in[i])
	}
	return b.String()
}

// digraph reports the Latin form of the two-rune sequence starting at i, if
// that pair is one ELOT 743 names.
//
// The vowel pairs are the reason this is not a plain rune map: αυ, ευ and ηυ
// each transliterate with a v or an f depending on what FOLLOWS them, so the
// rule needs the rune after the pair as well.
func digraph(in []rune, i int) (string, bool) {
	if i+1 >= len(in) {
		return "", false
	}
	first, second := in[i], in[i+1]

	// The consonant pairs, which need no lookahead.
	switch {
	case isGamma(first) && isGamma(second):
		return "ng", true
	case isGamma(first) && (second == 'ξ' || second == 'Ξ'):
		return "nx", true
	case isGamma(first) && (second == 'χ' || second == 'Χ'):
		return "nch", true
	}

	// ου is unconditional; αυ, ευ and ηυ are not.
	//
	// υ with a diaeresis (ϋ, ΰ) is NOT the second half of a digraph - that
	// is what the diaeresis is for - so it is excluded here and falls
	// through to the single-rune table, which is exactly right.
	if !isUpsilon(second) {
		return "", false
	}
	voiced := i+2 < len(in) && isVoiced(in[i+2])
	switch {
	case isOmicron(first):
		return "ou", true
	case isAlpha(first):
		return voicedForm(voiced, "av", "af"), true
	case isEpsilon(first):
		return voicedForm(voiced, "ev", "ef"), true
	case isEta(first):
		return voicedForm(voiced, "iv", "if"), true
	}
	return "", false
}

// voicedForm is the v-or-f choice, named so the three call sites above read
// as the one rule they are. (Not "pick" - copy.go already has one, for
// choosing a language.)
func voicedForm(voiced bool, before, otherwise string) string {
	if voiced {
		return before
	}
	return otherwise
}

// The digraph predicates. Each accepts both cases and the accented forms,
// because this runs before the fold and an accented vowel is still accented:
// "Εύβοια" carries its tonos on the epsilon-upsilon pair itself.
func isAlpha(r rune) bool   { return r == 'α' || r == 'Α' || r == 'ά' || r == 'Ά' }
func isEpsilon(r rune) bool { return r == 'ε' || r == 'Ε' || r == 'έ' || r == 'Έ' }
func isEta(r rune) bool     { return r == 'η' || r == 'Η' || r == 'ή' || r == 'Ή' }
func isOmicron(r rune) bool { return r == 'ο' || r == 'Ο' || r == 'ό' || r == 'Ό' }
func isGamma(r rune) bool   { return r == 'γ' || r == 'Γ' }

// isUpsilon accepts the plain and accented upsilon and REFUSES the ones
// carrying a diaeresis, which mark the vowel as standing alone.
func isUpsilon(r rune) bool { return r == 'υ' || r == 'Υ' || r == 'ύ' || r == 'Ύ' }

// isVoiced reports whether a rune makes the preceding αυ/ευ/ηυ take a v
// rather than an f: any vowel, or one of the voiced consonants β γ δ ζ λ μ ν
// ρ. Anything else - an unvoiced consonant, a space, a full stop, the end of
// the name - takes the f.
func isVoiced(r rune) bool {
	switch r {
	case 'α', 'ε', 'η', 'ι', 'ο', 'υ', 'ω',
		'Α', 'Ε', 'Η', 'Ι', 'Ο', 'Υ', 'Ω',
		'ά', 'έ', 'ή', 'ί', 'ό', 'ύ', 'ώ', 'ϊ', 'ϋ', 'ΐ', 'ΰ',
		'Ά', 'Έ', 'Ή', 'Ί', 'Ό', 'Ύ', 'Ώ', 'Ϊ', 'Ϋ':
		return true
	case 'β', 'γ', 'δ', 'ζ', 'λ', 'μ', 'ν', 'ρ',
		'Β', 'Γ', 'Δ', 'Ζ', 'Λ', 'Μ', 'Ν', 'Ρ':
		return true
	}
	return false
}

// transliterated is every single rune with a Latin form, lower-cased.
//
// GREEK is ELOT 743 / ISO 843 - the standard a Greek passport is written in,
// so a member reading a slug sees their retailer spelled the way their own
// documents spell their name. The accented vowels are here as well as the
// plain ones because this runs before the fold.
//
// Two of the standard's rules are deliberately ABSENT: μπ -> b and ντ -> d,
// which ELOT 743 applies only at the start of a word and which need
// word-boundary detection to get right. A slug is permanent, so a rule that
// is occasionally wrong is worse than one that is plainly literal: μπ stays
// "mp" here. If a real retailer name makes that read badly, the rule can be
// added with the word-boundary logic it needs and a test that pins it.
//
// The LATIN entries are the other half of the same class - characters NFD
// does not decompose, so the fold leaves them alone and the filter then drops
// them. ß is the one that matters commercially: it is not rare in German
// retail names and it currently splits one word into two.
var transliterated = map[rune]string{
	// Greek, plain.
	'α': "a", 'β': "v", 'γ': "g", 'δ': "d", 'ε': "e", 'ζ': "z", 'η': "i",
	'θ': "th", 'ι': "i", 'κ': "k", 'λ': "l", 'μ': "m", 'ν': "n", 'ξ': "x",
	'ο': "o", 'π': "p", 'ρ': "r", 'σ': "s", 'τ': "t", 'υ': "y", 'φ': "f",
	'χ': "ch", 'ψ': "ps", 'ω': "o",
	'Α': "a", 'Β': "v", 'Γ': "g", 'Δ': "d", 'Ε': "e", 'Ζ': "z", 'Η': "i",
	'Θ': "th", 'Ι': "i", 'Κ': "k", 'Λ': "l", 'Μ': "m", 'Ν': "n", 'Ξ': "x",
	'Ο': "o", 'Π': "p", 'Ρ': "r", 'Σ': "s", 'Τ': "t", 'Υ': "y", 'Φ': "f",
	'Χ': "ch", 'Ψ': "ps", 'Ω': "o",

	// Final sigma, which is the same letter and a different rune.
	'ς': "s",

	// Greek, accented. Same Latin form as the plain letter - the tonos marks
	// stress, not a different sound, and a slug carries no stress.
	'ά': "a", 'έ': "e", 'ή': "i", 'ί': "i", 'ό': "o", 'ύ': "y", 'ώ': "o",
	'ϊ': "i", 'ϋ': "y", 'ΐ': "i", 'ΰ': "y",
	'Ά': "a", 'Έ': "e", 'Ή': "i", 'Ί': "i", 'Ό': "o", 'Ύ': "y", 'Ώ': "o",
	'Ϊ': "i", 'Ϋ': "y",

	// Latin characters NFD does not decompose.
	'ß': "ss", 'ẞ': "ss",
	'ø': "o", 'Ø': "o",
	'æ': "ae", 'Æ': "ae",
	'œ': "oe", 'Œ': "oe",
	'ł': "l", 'Ł': "l",
	'đ': "d", 'Đ': "d",
	'þ': "th", 'Þ': "th",
	'ð': "d", 'Ð': "d",
}
