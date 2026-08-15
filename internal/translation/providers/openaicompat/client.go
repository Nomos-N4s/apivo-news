// Package openaicompat adapts any service speaking the OpenAI
// chat-completions API to the translation.Translator interface: a base
// URL, a model name and a key are all that separate one host from another
// (research D5, founder direction 2026-08-14). The same code therefore
// covers budget inference hosts, a self-hosted vLLM server and OpenAI
// itself, and moving between them is a configuration change.
//
// Nothing here is exported into the wider system: consumers depend on
// translation.Translator, and the composition root is the only place that
// names a host at all.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// Defaults applied to a Config's zero values. They suit a news pipeline:
// one item at a time, a short answer, and a few attempts before giving the
// item back to the caller rather than stalling the run.
const (
	// DefaultTimeout bounds a single attempt, including reading the body.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxAttempts is the total number of attempts, retries included.
	DefaultMaxAttempts = 3
	// DefaultBaseBackoff is the wait before the second attempt; each
	// further wait doubles it.
	DefaultBaseBackoff = 500 * time.Millisecond
	// DefaultMaxOutputTokens caps the completion. A headline plus a
	// 300-character extract needs a few hundred tokens even in Greek, and
	// the cap is what stops a looping model from eating the per-article
	// cost ceiling (FR-006).
	DefaultMaxOutputTokens = 600
)

const (
	// maxBackoff caps every wait, including one a provider asks for in
	// Retry-After: a host demanding minutes gets a bounded wait and then
	// the item is handed back, rather than a pipeline worker sleeping.
	maxBackoff = 30 * time.Second
	// maxResponseBytes bounds what we read from a response, so a
	// misbehaving or hostile host cannot exhaust memory here.
	maxResponseBytes = 1 << 20
	// errorBodySnippet bounds how much of a failed response is quoted back
	// in the error message.
	errorBodySnippet = 256
	// completionsPath is the OpenAI chat-completions path, appended to the
	// configured base URL.
	completionsPath = "/chat/completions"
	// costOverflowsAt is 2^63, the first value an int64 cannot hold. The
	// comparison must be against this and not against math.MaxInt64:
	// converting MaxInt64 (2^63-1) to float64 rounds it UP to exactly
	// 2^63, so a cost of 2^63 would pass a `> math.MaxInt64` test and
	// then wrap to a large negative cost on conversion.
	costOverflowsAt = float64(1 << 63)
)

// Config describes one OpenAI-compatible host. It is plain data so that
// selecting a provider is configuration and nothing else.
type Config struct {
	// BaseURL is the API root, without the /chat/completions suffix - for
	// example https://api.groq.com/openai/v1, https://api.together.ai/v1,
	// https://api.deepinfra.com/v1/openai, https://api.openai.com/v1, or a
	// self-hosted server's http://vllm.internal:8000/v1. Required.
	BaseURL string

	// Model is the model identifier as the host names it. Required. The
	// production choice comes from the pilot evaluation, not from here.
	Model string

	// APIKey is sent as a bearer token. Optional: a self-hosted server
	// started without a key expects no Authorization header, and an empty
	// key omits it rather than sending "Bearer ".
	APIKey string

	// InputPricePerMillionUSD and OutputPricePerMillionUSD are the host's
	// published prices in US dollars per million tokens - the unit every
	// one of these hosts publishes. Hosts report tokens, not money, so
	// these are what turn a usage block into the cost recorded with the
	// translation (FR-006). Both must be finite and strictly positive
	// unless FreeOfCharge says otherwise.
	InputPricePerMillionUSD  float64
	OutputPricePerMillionUSD float64

	// FreeOfCharge declares that this host charges nothing per token - a
	// self-hosted server, or quota already paid for - and is the only way
	// to configure zero prices.
	//
	// Without it a forgotten price line would be indistinguishable from a
	// genuinely free host, and every paid translation would be recorded
	// as free: the row would say zero, the monthly ledger would never
	// advance, and the cap could not trip however much was spent
	// (FR-006). Being free is a claim an operator makes deliberately, not
	// a default that a missing line produces.
	FreeOfCharge bool

	// Timeout bounds one attempt. Zero means DefaultTimeout.
	Timeout time.Duration

	// MaxAttempts is the total number of attempts, the first included.
	// Zero means DefaultMaxAttempts; one disables retrying.
	MaxAttempts int

	// BaseBackoff is the wait before the second attempt, doubling for each
	// attempt after that. Zero means DefaultBaseBackoff.
	BaseBackoff time.Duration

	// MaxOutputTokens caps the completion. Zero means
	// DefaultMaxOutputTokens.
	MaxOutputTokens int

	// HTTPClient performs the calls. Nil means a client dedicated to this
	// adapter. A supplied client's own Timeout, if any, applies on top of
	// the per-attempt deadline.
	HTTPClient *http.Client
}

// Client translates through one configured OpenAI-compatible host.
type Client struct {
	cfg      Config
	endpoint string
	http     *http.Client
	// sleep waits out a backoff, or returns early if the context ends.
	// Replaced in tests so retry behaviour is exercised without real time.
	sleep func(ctx context.Context, d time.Duration) error
}

// Client satisfies the interface the translation module owns; a change to
// either side fails to compile rather than at wiring time.
var _ translation.Translator = (*Client)(nil)

// New validates the configuration, applies defaults and returns a ready
// client. Configuration is checked once, here, rather than being
// rediscovered as a provider error on the first article of a run.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("openaicompat: base URL is required")
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: base URL %q is not a URL: %w", cfg.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("openaicompat: base URL %q must be http or https", cfg.BaseURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("openaicompat: base URL %q has no host", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("openaicompat: model is required")
	}
	if err := checkPrice("input", cfg.InputPricePerMillionUSD, cfg.FreeOfCharge); err != nil {
		return nil, err
	}
	if err := checkPrice("output", cfg.OutputPricePerMillionUSD, cfg.FreeOfCharge); err != nil {
		return nil, err
	}
	if cfg.Timeout < 0 || cfg.BaseBackoff < 0 || cfg.MaxAttempts < 0 || cfg.MaxOutputTokens < 0 {
		return nil, errors.New("openaicompat: timeout, backoff, attempts and output cap must not be negative")
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = DefaultMaxOutputTokens
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &Client{
		cfg:      cfg,
		endpoint: parsed.String() + completionsPath,
		http:     httpClient,
		sleep:    sleepContext,
	}, nil
}

// checkPrice rejects a price that cannot produce a recordable cost, and
// holds the free host and the priced host to opposite rules so neither can
// be reached by forgetting something.
func checkPrice(side string, price float64, freeOfCharge bool) error {
	if math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
		return fmt.Errorf("openaicompat: %s price per million tokens must be finite and non-negative, got %v", side, price)
	}
	if freeOfCharge && price != 0 {
		return fmt.Errorf("openaicompat: FreeOfCharge is set but the %s price is %v: a host is free or it is priced, not both", side, price)
	}
	if !freeOfCharge && price == 0 {
		return fmt.Errorf("openaicompat: %s price per million tokens is required: set the host's published price, or set FreeOfCharge to declare that it charges nothing", side)
	}
	return nil
}

// Translate implements translation.Translator: it renders the requested
// prompt version, calls the host, and returns the translation together
// with the model, prompt version and cost the translation row records.
//
// Retries cover rate limits, host-side failures and timeouts; every other
// failure is returned immediately, classified with the translation
// package's sentinel errors.
func (c *Client) Translate(ctx context.Context, req translation.Request) (translation.Result, error) {
	if err := req.Validate(); err != nil {
		return translation.Result{}, err
	}
	prompt, err := translation.PromptByVersion(req.PromptVersion)
	if err != nil {
		return translation.Result{}, err
	}

	body, err := json.Marshal(chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: prompt.System},
			{Role: "user", Content: prompt.UserMessage(req)},
		},
		// Translation is not a creative task and the same item should not
		// cost twice for two different answers.
		Temperature: 0,
		MaxTokens:   c.cfg.MaxOutputTokens,
		// Honoured by OpenAI, Groq, Together, DeepInfra and vLLM; hosts
		// that ignore it are handled by the defensive parsing below.
		ResponseFormat: responseFormat{Type: "json_object"},
	})
	if err != nil {
		return translation.Result{}, fmt.Errorf("openaicompat: encoding request: %w: %w", err, translation.ErrInvalidRequest)
	}

	// spent accumulates what this translation has cost so far, including
	// attempts that produced nothing. In practice at most one attempt is
	// ever metered - a priced 200 is never retried - but a discarded
	// attempt's cost belongs in the total either way.
	var (
		lastErr error
		spent   translation.Spend
	)
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		out := c.attempt(ctx, body, req.PromptVersion)
		spent.CostMicroUSD += out.spend.CostMicroUSD
		spent.UnmeteredAttempts += out.spend.UnmeteredAttempts

		if out.err == nil {
			result := out.result
			result.Spend = spent
			return result, nil
		}
		lastErr = out.err
		if attempt == c.cfg.MaxAttempts || !retryable(out.err) || ctx.Err() != nil {
			break
		}
		if err := c.sleep(ctx, backoff(attempt, c.cfg.BaseBackoff, out.retryAfter)); err != nil {
			return translation.Result{}, withSpend(spent, waitInterrupted(err, lastErr))
		}
	}
	return translation.Result{}, withSpend(spent, lastErr)
}

// waitInterrupted builds the failure for a context that ended while we
// were waiting to retry.
//
// It keeps everything a caller needs in one chain. lastErr carries the
// sentinel that classified the provider's behaviour, and losing it here
// would be losing it at exactly the moment it matters: a pipeline
// deciding whether to slow down needs to know it was being rate limited,
// not merely that a context ended. The caller's own deadline is a
// timeout, and is classified as one for the same reason the transport
// path does it - where the deadline was noticed is an implementation
// detail, not a different kind of failure. Pure cancellation stays
// cancellation.
func waitInterrupted(waitErr, lastErr error) error {
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return fmt.Errorf("openaicompat: the caller's deadline passed while waiting to retry: %w: %w: %w", waitErr, translation.ErrTimeout, lastErr)
	}
	return fmt.Errorf("openaicompat: the call was abandoned while waiting to retry: %w: %w", waitErr, lastErr)
}

// withSpend attaches spend to a failure, and only when there is spend to
// report: a failure that cost nothing stays a plain error, so callers do
// not have to unwrap one to learn that.
func withSpend(spent translation.Spend, err error) error {
	if spent.IsZero() {
		return err
	}
	return &translation.SpendError{Spend: spent, Err: err}
}

// attemptResult is everything one attempt yields: a translation when it
// worked, the spend it incurred whether or not it worked, the wait a
// rate-limited host asked for, and why it failed.
type attemptResult struct {
	result     translation.Result
	spend      translation.Spend
	retryAfter time.Duration
	err        error
}

// attempt performs one call.
func (c *Client) attempt(ctx context.Context, body []byte, promptVersion string) attemptResult {
	// A context that has already ended means the request never leaves, so
	// nothing can have been billed for it. Checked here rather than left
	// to the transport so that outcome is unambiguous.
	if err := ctx.Err(); err != nil {
		return attemptResult{err: contextError(err)}
	}

	attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return attemptResult{err: fmt.Errorf("openaicompat: building request: %w: %w", err, translation.ErrInvalidRequest)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return c.transportOutcome(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return c.transportOutcome(ctx, err)
	}

	if resp.StatusCode != http.StatusOK {
		// A provider that refuses a request generates no completion, so
		// there is nothing for it to bill.
		return attemptResult{
			retryAfter: retryAfter(resp.Header),
			err:        statusError(resp.StatusCode, payload),
		}
	}

	result, spend, err := c.decode(payload, promptVersion)
	return attemptResult{result: result, spend: spend, err: err}
}

// transportOutcome classifies an exchange that never completed, and
// decides whether the provider may already have billed for it.
func (c *Client) transportOutcome(ctx context.Context, err error) attemptResult {
	out := attemptResult{err: c.transportError(ctx, err)}
	// The request was in flight when this failed, so the host may have
	// generated - and charged for - a completion we will never see. The
	// amount is unknowable here; the fact is not. Counting it errs
	// towards believing we have spent more than the ledger shows, which
	// is the safe direction for a spend cap.
	if errors.Is(out.err, translation.ErrTimeout) || errors.Is(out.err, context.Canceled) {
		out.spend.UnmeteredAttempts = 1
	}
	return out
}

// transportError classifies a failure to complete the exchange, keeping
// the caller's own cancellation distinct from our per-attempt deadline.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("openaicompat: no answer within %s: %w", c.cfg.Timeout, translation.ErrTimeout)
	}
	return fmt.Errorf("openaicompat: calling the provider: %w: %w", err, translation.ErrUnavailable)
}

// contextError renders the caller's own deadline or cancellation.
func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("openaicompat: caller deadline reached: %w: %w", err, translation.ErrTimeout)
	}
	return fmt.Errorf("openaicompat: translation cancelled: %w", err)
}

// statusError maps a non-200 status onto the translation module's
// vocabulary, so callers act on what happened rather than on a number.
func statusError(status int, payload []byte) error {
	snippet := snippetOf(payload)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired:
		return fmt.Errorf("openaicompat: provider answered %d: %w: %s", status, translation.ErrAuth, snippet)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("openaicompat: provider answered %d: %w: %s", status, translation.ErrRateLimited, snippet)
	case status >= 500:
		return fmt.Errorf("openaicompat: provider answered %d: %w: %s", status, translation.ErrUnavailable, snippet)
	default:
		return fmt.Errorf("openaicompat: provider answered %d: %w: %s", status, translation.ErrInvalidRequest, snippet)
	}
}

// decode turns a 200 response into a Result, refusing anything it cannot
// record honestly - and reporting what the attempt cost either way.
//
// Pricing comes first deliberately. A 200 was generated and billed
// whatever we go on to think of its content, so establishing the cost
// before judging the answer is what lets a refusal carry its spend
// instead of dropping it (FR-006). An answer whose cost cannot even be
// established is an unmetered attempt: billed, probably, but unknowably.
func (c *Client) decode(payload []byte, promptVersion string) (translation.Result, translation.Spend, error) {
	unmetered := translation.Spend{UnmeteredAttempts: 1}

	var resp chatResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return translation.Result{}, unmetered, fmt.Errorf("openaicompat: response is not JSON: %w: %w: %s", err, translation.ErrInvalidResponse, snippetOf(payload))
	}
	reported, err := c.reportedUsage(resp.Usage)
	if err != nil {
		return translation.Result{}, unmetered, err
	}
	cost, err := costMicroUSD(reported, c.cfg.InputPricePerMillionUSD, c.cfg.OutputPricePerMillionUSD)
	if err != nil {
		return translation.Result{}, unmetered, err
	}

	// From here the attempt has a known price, and it stands whether or
	// not the content survives the checks below.
	spend := translation.Spend{CostMicroUSD: cost}

	if len(resp.Choices) == 0 {
		return translation.Result{}, spend, fmt.Errorf("openaicompat: response carries no choices: %w: %s", translation.ErrInvalidResponse, snippetOf(payload))
	}
	choice := resp.Choices[0]
	if choice.FinishReason == "length" {
		return translation.Result{}, spend, fmt.Errorf("openaicompat: answer was cut off at the output cap (%d tokens), so it is incomplete: %w", c.cfg.MaxOutputTokens, translation.ErrInvalidResponse)
	}

	content, err := parseContent(choice.Message.Content)
	if err != nil {
		return translation.Result{}, spend, err
	}

	model := strings.TrimSpace(resp.Model)
	if model == "" {
		// A host that does not echo the model still produced this text with
		// the one we asked for; the configured name is the true lineage.
		model = c.cfg.Model
	}
	return translation.Result{
		Headline:      content.Headline,
		Extract:       content.Extract,
		Model:         model,
		PromptVersion: promptVersion,
		Spend:         spend,
	}, spend, nil
}

// reportedUsage returns the token counts a priced call must carry, and
// refuses the response otherwise.
//
// Testing the usage block for presence is not enough. Only the OpenAI
// field names are decoded, so a response reporting totals only
// ({"total_tokens": 365}), an empty block from a gateway or content
// filter ({}), or another vendor's names behind an OpenAI-compatible
// gateway (input_tokens/output_tokens, promptTokenCount) all decode to a
// present block with both counters at zero - and would price at zero.
// That is the exact failure translation.cost_microusd exists to prevent:
// the row says free, the monthly ledger never advances, and the cap
// cannot trip however many articles are translated (FR-006).
//
// A completion that returned content necessarily consumed prompt tokens
// and produced completion tokens, so on a priced host a zero counter is
// unreported usage, not free work. A host that genuinely charges nothing
// is identified by its zero PRICES - declared with FreeOfCharge - and
// its counts are not load-bearing there, so they are left alone.
func (c *Client) reportedUsage(reported *usage) (usage, error) {
	if reported == nil {
		return usage{}, fmt.Errorf("openaicompat: response reports no token usage, so its cost is unknown: %w", translation.ErrInvalidResponse)
	}
	if c.cfg.FreeOfCharge {
		return *reported, nil
	}
	if reported.PromptTokens <= 0 || reported.CompletionTokens <= 0 {
		return usage{}, fmt.Errorf(
			"openaicompat: response reports %d prompt and %d completion tokens, which a priced call cannot have consumed: its usage was not reported in the fields this API defines, so its cost is unknown: %w",
			reported.PromptTokens, reported.CompletionTokens, translation.ErrInvalidResponse,
		)
	}
	return *reported, nil
}

// translated is the JSON object the prompt asks the model for.
type translated struct {
	Headline string `json:"headline"`
	Extract  string `json:"extract"`
}

// parseContent recovers the requested JSON object from the model's answer.
//
// Hosts and models wrap it in prose ("Here is the translation:"), in
// ```json code fences, or in both, and some ignore response_format
// entirely. Scanning for balanced top-level objects handles all of those
// without guessing: each candidate is tried in turn, and the first that
// parses into a usable translation wins. Nothing else is accepted -
// storing a half-parsed answer would put the model's commentary on the
// front page.
func parseContent(raw string) (translated, error) {
	if strings.TrimSpace(raw) == "" {
		return translated{}, fmt.Errorf("openaicompat: model returned empty content: %w", translation.ErrInvalidResponse)
	}
	candidates := jsonObjects(raw)
	if len(candidates) == 0 {
		return translated{}, fmt.Errorf("openaicompat: model answered with no JSON object: %w: %s", translation.ErrInvalidResponse, snippetOf([]byte(raw)))
	}

	var lastErr error
	for _, candidate := range candidates {
		var parsed translated
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
			lastErr = fmt.Errorf("openaicompat: model answered with unparsable JSON: %w: %w: %s", err, translation.ErrInvalidResponse, snippetOf([]byte(candidate)))
			continue
		}
		parsed.Headline = strings.TrimSpace(parsed.Headline)
		parsed.Extract = strings.TrimSpace(parsed.Extract)
		if parsed.Headline == "" || parsed.Extract == "" {
			lastErr = fmt.Errorf("openaicompat: model returned a JSON object without both a headline and an extract: %w: %s", translation.ErrInvalidResponse, snippetOf([]byte(candidate)))
			continue
		}
		if n := utf8.RuneCountInString(parsed.Extract); n > translation.MaxExtractChars {
			// Truncating here would publish a sentence the model never
			// wrote and hide a prompt that has stopped working. What we
			// publish is an extract beside a link (FR-004, FR-005), so an
			// over-long answer is refused.
			//
			// Refused, but not fatal to the whole response: this is one
			// candidate among several, and an oversized example object in
			// the model's preamble must not condemn the real translation
			// that follows it. Every other check here treats a bad
			// candidate the same way.
			lastErr = fmt.Errorf("openaicompat: model returned a %d-character extract, over the %d-character limit: %w", n, translation.MaxExtractChars, translation.ErrInvalidResponse)
			continue
		}
		return parsed, nil
	}
	return translated{}, lastErr
}

// jsonObjects returns every top-level brace-balanced substring of s, in
// order. Braces inside JSON strings, and escapes inside those strings, do
// not open or close an object - otherwise a translated headline containing
// "}" would truncate the object.
func jsonObjects(s string) []string {
	var (
		objects  []string
		depth    int
		start    int
		inString bool
		escaped  bool
	)
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case inString && r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case inString:
			// Braces inside a string are text.
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				objects = append(objects, s[start:i+1])
			}
		}
	}
	return objects
}

// costMicroUSD converts reported token counts into micro-USD.
//
// The arithmetic is exact by construction: a price is dollars per million
// tokens, and a micro-USD is a millionth of a dollar, so the two factors
// of a million cancel and the cost in micro-USD is simply tokens times
// price. The result is rounded to the whole micro-USD the column stores.
func costMicroUSD(u usage, inputPrice, outputPrice float64) (int64, error) {
	if u.PromptTokens < 0 || u.CompletionTokens < 0 {
		return 0, fmt.Errorf("openaicompat: provider reported negative token usage (%d in, %d out): %w", u.PromptTokens, u.CompletionTokens, translation.ErrInvalidResponse)
	}
	micro := float64(u.PromptTokens)*inputPrice + float64(u.CompletionTokens)*outputPrice
	rounded := math.Round(micro)
	if math.IsNaN(rounded) || rounded < 0 || rounded >= costOverflowsAt {
		return 0, fmt.Errorf("openaicompat: reported usage (%d in, %d out) yields an unrepresentable cost: %w", u.PromptTokens, u.CompletionTokens, translation.ErrInvalidResponse)
	}
	return int64(rounded), nil
}

// retryAfter reads a provider's requested wait. Only the delta-seconds
// form is honoured, which is what these hosts send; anything else falls
// through to the adapter's own backoff.
func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// backoff is the wait before the attempt after this one: the provider's
// own request when it made one, otherwise exponential from the base. Both
// are capped, so no single item can hold a worker for minutes.
func backoff(attempt int, base, requested time.Duration) time.Duration {
	if requested > 0 {
		return min(requested, maxBackoff)
	}
	wait := base
	for i := 1; i < attempt && wait < maxBackoff; i++ {
		wait *= 2
	}
	return min(wait, maxBackoff)
}

// retryable reports whether trying the same request again could plausibly
// succeed: the host was busy, unwell or slow. A rejected key or an
// unusable answer would only fail again.
func retryable(err error) bool {
	return errors.Is(err, translation.ErrRateLimited) ||
		errors.Is(err, translation.ErrUnavailable) ||
		errors.Is(err, translation.ErrTimeout)
}

// sleepContext waits for d, or until the context ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// snippetOf bounds what an error message quotes back from a response, in
// whole runes, so a megabyte of provider HTML cannot land in a log line.
func snippetOf(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return "(empty body)"
	}
	if len(text) <= errorBodySnippet {
		return text
	}
	runes := []rune(text)
	if len(runes) <= errorBodySnippet {
		return text
	}
	return string(runes[:errorBodySnippet]) + "..."
}

// The OpenAI chat-completions wire types, kept unexported: the shape of
// somebody else's API is not part of this system's vocabulary.

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Temperature    float64        `json:"temperature"`
	MaxTokens      int            `json:"max_tokens"`
	ResponseFormat responseFormat `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}
