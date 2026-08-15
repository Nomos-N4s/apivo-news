package translation

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// releasedPromptDigests pins the exact text of every released prompt.
//
// This is the mechanism that makes a prompt change require a version bump.
// translation.prompt_version is the only record of how a stored
// translation was produced (FR-005), so editing a released prompt would
// silently falsify every row already written under that version. Editing
// the text here breaks TestReleasedPromptsAreFrozen; the only way to
// change a prompt is to register a new version and pin it below, which
// leaves the old version's text intact for the rows that reference it.
//
// Deliberately a literal table and not a computed one: a digest derived
// from the registry would move with it and enforce nothing.
var releasedPromptDigests = map[string]string{
	PromptVersionV1: "fb45bdd7416baa3b075ea6292c6f129c686715bccfe565a2f7a895c20bed64d8",
}

// promptDigest hashes the whole prompt - version, instruction and input
// template - so any edit to any part of it moves the digest.
func promptDigest(p Prompt) string {
	sum := sha256.Sum256([]byte(p.Version + "\x00" + p.System + "\x00" + p.UserTemplate))
	return hex.EncodeToString(sum[:])
}

func TestReleasedPromptsAreFrozen(t *testing.T) {
	t.Parallel()

	for version := range releasedPromptDigests {
		if _, ok := prompts[version]; !ok {
			t.Errorf("prompt version %q is pinned here but no longer registered: a released version must stay available, the rows that reference it cannot be rewritten", version)
		}
	}

	for version, prompt := range prompts {
		want, pinned := releasedPromptDigests[version]
		if !pinned {
			t.Errorf("prompt version %q is registered but not pinned: add %q: %q to releasedPromptDigests", version, version, promptDigest(prompt))
			continue
		}
		if got := promptDigest(prompt); got != want {
			t.Errorf("prompt %q changed (digest %s, pinned %s): released prompt text is lineage (FR-005) and is frozen - register a NEW version instead of editing this one", version, got, want)
		}
	}
}

func TestRegistryKeysMatchPromptVersions(t *testing.T) {
	t.Parallel()

	for key, prompt := range prompts {
		if prompt.Version != key {
			t.Errorf("prompt registered under %q reports version %q: the key is what callers pass and what gets recorded, so the two must agree", key, prompt.Version)
		}
	}
}

// TestPromptsAskForExtractNotFullText guards the shape of every released
// prompt against the one mistake that would breach FR-005 / FR-004: asking
// a model for a translation of the whole article rather than an extract.
func TestPromptsAskForExtractNotFullText(t *testing.T) {
	t.Parallel()

	for version, prompt := range prompts {
		system := strings.ToLower(prompt.System)
		if !strings.Contains(system, "extract") {
			t.Errorf("prompt %q never mentions an extract: translations are limited to headline and extract (FR-005)", version)
		}
		if !strings.Contains(prompt.System, strconv.Itoa(MaxExtractChars)) {
			t.Errorf("prompt %q does not state the %d-character bound: the model must be told the limit the adapter enforces", version, MaxExtractChars)
		}
		if !strings.Contains(system, "never produce a full-text translation") {
			t.Errorf("prompt %q does not rule out a full-text translation: extract-and-link is the licence basis we publish under (FR-004)", version)
		}
		if !strings.Contains(system, "json") {
			t.Errorf("prompt %q does not demand JSON: the adapter parses a JSON object and rejects anything else", version)
		}
		for _, field := range []string{`"headline"`, `"extract"`} {
			if !strings.Contains(prompt.System, field) {
				t.Errorf("prompt %q does not name the %s field the adapter reads", version, field)
			}
		}
	}
}

func TestUserTemplateCarriesEveryPlaceholder(t *testing.T) {
	t.Parallel()

	placeholders := []string{
		PlaceholderSourceLanguage,
		PlaceholderTargetLanguage,
		PlaceholderHeadline,
		PlaceholderText,
	}
	for version, prompt := range prompts {
		for _, placeholder := range placeholders {
			if !strings.Contains(prompt.UserTemplate, placeholder) {
				t.Errorf("prompt %q template is missing %s: the model would never see that part of the request", version, placeholder)
			}
		}
	}
}

func TestUserMessageSubstitutesRequestValues(t *testing.T) {
	t.Parallel()

	prompt, err := PromptByVersion(CurrentPromptVersion)
	if err != nil {
		t.Fatalf("PromptByVersion(%q): %v", CurrentPromptVersion, err)
	}
	req := Request{
		SourceTitle:    "Δήμαρχος ανακοινώνει νέο πάρκο",
		SourceText:     "Το πάρκο ανοίγει τον Μάιο.",
		SourceLanguage: "el",
		TargetLanguage: "de",
		PromptVersion:  CurrentPromptVersion,
	}

	message := prompt.UserMessage(req)
	for _, want := range []string{req.SourceTitle, req.SourceText, req.SourceLanguage, req.TargetLanguage} {
		if !strings.Contains(message, want) {
			t.Errorf("rendered message is missing %q:\n%s", want, message)
		}
	}
	for _, placeholder := range []string{
		PlaceholderSourceLanguage, PlaceholderTargetLanguage, PlaceholderHeadline, PlaceholderText,
	} {
		if strings.Contains(message, placeholder) {
			t.Errorf("rendered message still contains %s:\n%s", placeholder, message)
		}
	}
}

// TestUserMessageDoesNotReExpandItemText: a news item that contains a
// placeholder is content, not a template. Substitution runs in one pass,
// so an item quoting "{{text}}" cannot make the prompt expand anything.
func TestUserMessageDoesNotReExpandItemText(t *testing.T) {
	t.Parallel()

	prompt := prompts[PromptVersionV1]
	message := prompt.UserMessage(Request{
		SourceTitle:    PlaceholderText,
		SourceText:     "body mentioning " + PlaceholderHeadline,
		SourceLanguage: "el",
		TargetLanguage: "de",
	})

	if !strings.Contains(message, "Headline:\n"+PlaceholderText) {
		t.Errorf("a headline that is itself a placeholder was not passed through verbatim:\n%s", message)
	}
	if !strings.Contains(message, "body mentioning "+PlaceholderHeadline) {
		t.Errorf("a body quoting a placeholder was not passed through verbatim:\n%s", message)
	}
}

func TestPromptByVersionRejectsUnknownVersion(t *testing.T) {
	t.Parallel()

	if _, err := PromptByVersion("v0"); err == nil {
		t.Fatal("PromptByVersion(\"v0\") succeeded; an unknown version must never fall back to another prompt")
	} else if !strings.Contains(err.Error(), "v0") {
		t.Errorf("error does not name the rejected version: %v", err)
	}
}

func TestCurrentPromptVersionIsRegistered(t *testing.T) {
	t.Parallel()

	if _, err := PromptByVersion(CurrentPromptVersion); err != nil {
		t.Fatalf("CurrentPromptVersion %q is not registered: %v", CurrentPromptVersion, err)
	}
}

func TestPromptVersionsListsTheRegistry(t *testing.T) {
	t.Parallel()

	versions := PromptVersions()
	if len(versions) != len(prompts) {
		t.Fatalf("PromptVersions() returned %d versions, registry holds %d", len(versions), len(prompts))
	}
	for _, version := range versions {
		if _, ok := prompts[version]; !ok {
			t.Errorf("PromptVersions() reported unregistered version %q", version)
		}
	}
}

// TestMaxExtractCharsIsCountedInRunes documents why the bound is a rune
// count: Greek text measured in UTF-8 bytes would be cut at roughly half
// the intended length.
func TestMaxExtractCharsIsCountedInRunes(t *testing.T) {
	t.Parallel()

	greek := strings.Repeat("α", MaxExtractChars)
	if got := utf8.RuneCountInString(greek); got != MaxExtractChars {
		t.Fatalf("rune count = %d, want %d", got, MaxExtractChars)
	}
	if len(greek) <= MaxExtractChars {
		t.Fatal("expected Greek text to take more bytes than runes; the bound must be applied to runes")
	}
}
