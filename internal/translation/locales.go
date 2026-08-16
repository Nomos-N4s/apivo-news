package translation

import "slices"

// AlphaReaderLocales are the languages the alpha's readers read in, as
// primary language subtags (language and place are independent axes; there
// is deliberately no combined locale tag). The pipeline translates INTO
// each of them, from items whose source language differs.
//
// It is a var only because Go has no const slices; nothing may mutate it.
// English feeds exist as sources (the language table seeds el, de, en) but
// the alpha has no English readers, so "en" is absent here: an English
// item is translated for Greek and German readers, and nothing is
// translated into English.
var AlphaReaderLocales = []string{"el", "de"}

// TargetLocales returns the locales an item in sourceLanguage is
// translated into: every reader locale except the item's own language, in
// the readers' order. An item already in a reader's language needs no
// translation for that reader - the original is what they read.
//
// Duplicate reader locales collapse to one target: two entries could only
// pay a provider twice for a result the unique index then refuses to
// store a second time.
func TargetLocales(sourceLanguage string, readerLocales []string) []string {
	var targets []string
	for _, locale := range readerLocales {
		if locale == sourceLanguage || slices.Contains(targets, locale) {
			continue
		}
		targets = append(targets, locale)
	}
	return targets
}
