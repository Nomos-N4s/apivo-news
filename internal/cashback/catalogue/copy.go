// Which language a merchant is described in, and saying so when it is not
// the one that was asked for (T101, US5 scenario 2).
//
// The rule this file exists for is one sentence of the spec: a merchant with
// no copy in the reader's language is shown in the SOURCE language and
// LABELLED, never blank and never machine-invented. Each of those three is a
// different failure and only the first is obvious.
//
// A blank is the failure a naive lookup produces: ask for Greek, find
// nothing, render "". The member sees a card with no name on it and no way
// to tell whether the merchant is broken or the site is.
//
// An invented name is the failure a helpful system produces. Nothing here
// translates, transliterates, or falls back to a slug dressed up as a name -
// a slug is an identifier this system chose, and showing it as a merchant's
// name tells the member a company is called something it is not.
//
// An UNLABELLED fallback is the subtle one, and it is why [Copy] carries the
// language it is actually in rather than the one that was asked for. Greek
// copy and German copy shown identically leave a Greek reader to work out
// for themselves why one card is in a language they may not read - and a
// consumer that only sees strings cannot label what it was not told about.

package catalogue

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoCopy reports a merchant with no copy at all, in any language.
//
// The schema permits it - merchant_copy has no row-per-merchant requirement
// - and it is a merchant nothing can honestly show. It is an error rather
// than an empty [Copy] because the alternative is exactly the blank name
// this file exists to prevent: a caller that got a zero value and did not
// check would render it.
var ErrNoCopy = errors.New("catalogue: this merchant has no copy in any language")

// Copy is one merchant's description, in whichever language it turned out to
// be available in.
type Copy struct {
	// Name is what the merchant is called. Never blank: the column refuses
	// it and [Resolve] refuses a row that reached it anyway.
	Name string
	// Summary and Terms are optional in the schema and stay optional here.
	// An empty one is an absence, not a fallback candidate: a merchant with
	// Greek copy that has no summary is shown its Greek name and no
	// summary, NOT a German summary under a Greek name. Mixing languages
	// inside one card is the fallback being unlabelled at field level.
	Summary string
	Terms   string
	// Language is the language this copy is actually in, which is not
	// necessarily the one that was asked for.
	Language string
	// Fallback reports that Language is not what the caller asked for, so a
	// surface can say so. Derived rather than left to the caller to
	// compare, because the comparison is where the label gets forgotten.
	Fallback bool
}

// Available is one row of merchant_copy as [Resolve] needs it: which
// language, and what it says.
type Available struct {
	Language string
	Name     string
	Summary  string
	Terms    string
}

// Resolve picks the copy to show a reader who asked for want, from the copy
// a merchant has and the language it was sourced in.
//
// The order is: the reader's language, then the merchant's source language,
// then nothing. There is deliberately no third step - no English default, no
// "any copy at all", no closest-match by language family. A merchant sourced
// in German with copy in German and French, read by a Greek reader, shows
// German: the source language is the one the merchant itself published, and
// picking French instead would be this system deciding a member reads French
// on no evidence.
//
// Matching is case-insensitive on the BCP-47 primary subtag, so a reader
// asking for "EL" and a row stored as "el" are the same language. Nothing
// broader: "pt" does not match "pt-BR", because a Brazilian reader shown
// European Portuguese should be told it is a fallback rather than have the
// difference hidden by a prefix match.
func Resolve(want, sourceLanguage string, available []Available) (Copy, error) {
	asked := normaliseLanguage(want)
	source := normaliseLanguage(sourceLanguage)

	if found, ok := pick(asked, available); ok {
		return copyOf(found, false)
	}
	if found, ok := pick(source, available); ok {
		// Labelled even when the reader asked for nothing: a caller with no
		// language to offer is still showing copy in one, and a surface
		// that knows it is the source language can say so.
		return copyOf(found, true)
	}
	return Copy{}, fmt.Errorf("%w: asked for %q, sourced in %q", ErrNoCopy, want, sourceLanguage)
}

// pick finds the copy in one language, if there is any.
func pick(language string, available []Available) (Available, bool) {
	if language == "" {
		return Available{}, false
	}
	for _, one := range available {
		if normaliseLanguage(one.Language) == language {
			return one, true
		}
	}
	return Available{}, false
}

// copyOf renders one row, refusing a blank name.
//
// merchant_copy_name_not_blank already refuses one, so this fires only on a
// row that did not come from that column - a fake in a test, or a caller
// assembling Available by hand. It is a refusal rather than a pass-through
// because a blank name reaching a member's screen is the exact failure this
// file exists to prevent, and the check costs nothing.
func copyOf(one Available, fallback bool) (Copy, error) {
	name := strings.TrimSpace(one.Name)
	if name == "" {
		return Copy{}, fmt.Errorf("%w: the %s copy has no name", ErrNoCopy, one.Language)
	}
	return Copy{
		Name:     name,
		Summary:  strings.TrimSpace(one.Summary),
		Terms:    strings.TrimSpace(one.Terms),
		Language: normaliseLanguage(one.Language),
		Fallback: fallback,
	}, nil
}

// normaliseLanguage reduces a language tag to the form this comparison uses:
// trimmed and lowercased, and nothing else. Tags reach here from a URL, a
// database column and a network's feed, and "EL" from one of them is the
// same language as "el" from another.
func normaliseLanguage(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}
