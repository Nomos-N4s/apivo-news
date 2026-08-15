package translation

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Translator is the single seam between the pipeline and any machine
// translation provider (research D5). Implementations are adapters under
// providers/, selected by configuration in the composition root: swapping
// one host for another is a config change, never a change to a consumer.
//
// The interface is owned here, by the module that defines what a
// translation is, so no provider vocabulary reaches the rest of the system.
type Translator interface {
	// Translate turns one retrieved item into a translated headline and
	// extract, together with the lineage the translation row records.
	//
	// Failures are classified with this package's sentinel errors -
	// ErrInvalidRequest, ErrAuth, ErrRateLimited, ErrInvalidResponse,
	// ErrUnavailable, ErrTimeout - so a caller can tell "fix the
	// configuration" from "back off" from "retry this item later" without
	// knowing which provider answered.
	Translate(ctx context.Context, req Request) (Result, error)
}

// Request is everything a provider needs to translate one item: the source
// headline and text, the language pair, and which registered prompt to use.
// It is pure data; nothing in it is provider-specific.
type Request struct {
	// SourceTitle is the item's original headline. Required.
	SourceTitle string

	// SourceText is the original text the extract is drawn from - the
	// item's summary or body as retrieved. Required.
	SourceText string

	// SourceLanguage is the ISO 639-1 code of the original, e.g. "el".
	// Required.
	SourceLanguage string

	// TargetLanguage is the ISO 639-1 code to translate into, e.g. "de".
	// Required.
	TargetLanguage string

	// PromptVersion names the registered prompt to build the provider call
	// from; see PromptByVersion. It is recorded with the translation for
	// lineage (FR-005), so it is required and must be a released version.
	PromptVersion string
}

// Validate reports the first hole in the request. Adapters call it before
// spending a provider round trip - and real money - on a call that cannot
// produce a recordable translation.
func (r Request) Validate() error {
	switch {
	case strings.TrimSpace(r.SourceTitle) == "":
		return fmt.Errorf("%w: source title is blank", ErrInvalidRequest)
	case strings.TrimSpace(r.SourceText) == "":
		return fmt.Errorf("%w: source text is blank", ErrInvalidRequest)
	case strings.TrimSpace(r.SourceLanguage) == "":
		return fmt.Errorf("%w: source language is blank", ErrInvalidRequest)
	case strings.TrimSpace(r.TargetLanguage) == "":
		return fmt.Errorf("%w: target language is blank", ErrInvalidRequest)
	case strings.TrimSpace(r.PromptVersion) == "":
		return fmt.Errorf("%w: prompt version is blank", ErrInvalidRequest)
	}
	return nil
}

// Result is one successful translation: the translated content plus the
// lineage persisted alongside it (FR-005, FR-006).
type Result struct {
	// Headline is the translated headline, destined for
	// translation.headline.
	Headline string

	// Extract is the translated extract, at most MaxExtractChars
	// characters, destined for translation.extract. It is an extract next
	// to a link, never a full-text translation (FR-005).
	Extract string

	// Model identifies the model that produced this translation, as the
	// provider reported it. Destined for translation.model.
	Model string

	// PromptVersion echoes Request.PromptVersion. Destined for
	// translation.prompt_version.
	PromptVersion string

	// CostMicroUSD is what this translation cost, in micro-USD, computed
	// from the provider's reported token usage and the configured prices.
	// Destined for translation.cost_microusd, which has no default: an
	// unrecorded cost is a rejected insert, never a silent zero (FR-006).
	// A genuine zero - a self-hosted server, or included quota - is legal
	// and meaningful.
	CostMicroUSD int64
}

// ErrInvalidRequest reports a call that is wrong rather than unlucky:
// blank required fields, an unregistered prompt version, or a request the
// provider itself rejected as malformed (a 4xx that is neither an
// authentication nor a rate-limit failure - unknown model, unsupported
// parameter, oversized input). Retrying it unchanged cannot help.
var ErrInvalidRequest = errors.New("translation: request is not translatable")

// ErrAuth reports that the provider rejected our credentials: the key is
// wrong, expired, or lacks access to the configured model. Retrying is
// pointless; the configuration needs fixing, and an operator needs to know.
var ErrAuth = errors.New("translation: provider rejected credentials")

// ErrRateLimited reports that the provider kept rate-limiting the call
// until the adapter's retry budget ran out. The pipeline should slow down
// rather than hammer on; the item itself is fine.
var ErrRateLimited = errors.New("translation: provider rate limit exhausted")

// ErrInvalidResponse reports a reply we refuse to record: no content, a
// truncated answer, content that is not the requested JSON object, blank
// or over-long fields, or missing token usage. Without usage the cost is
// unknown, and an unknown cost must never silently become zero (FR-006).
var ErrInvalidResponse = errors.New("translation: provider response unusable")

// ErrUnavailable reports that the provider could not be reached, or kept
// failing on its own side, until the retry budget ran out. Nothing about
// the request was wrong; the item can be tried again later.
var ErrUnavailable = errors.New("translation: provider unavailable")

// ErrTimeout reports that a call exceeded its deadline - the adapter's own
// per-request timeout, or a deadline the caller imposed. Distinguished
// from ErrUnavailable because a persistent timeout usually means the model
// or the prompt is too slow, not that the host is down.
var ErrTimeout = errors.New("translation: provider call timed out")
