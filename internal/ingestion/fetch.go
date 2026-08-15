package ingestion

// Retrieval: one conditional GET of one configured feed, modelled on the
// openaicompat client's retry, backoff and timeout shape so the two read as
// siblings.
//
// The posture towards the sources we read, recorded here because it is a
// decision and not an accident:
//
//   - Identify honestly. Every request carries a User-Agent naming the
//     software and a contact, on the first hop and on every redirect after
//     it. A source that wants us gone must be able to say so to somebody.
//   - Ask conditionally. The stored ETag and Last-Modified go out with each
//     request, so an unchanged feed costs the source a 304 and nothing else.
//   - One request per source per interval, jittered. The interval and the
//     jitter belong to the poll loop (the other half of T013); what this
//     file guarantees is that one Fetch is one exchange - retries excepted,
//     and those are bounded and backed off.
//   - Do not fetch the source's robots.txt. FR-001 restricts us to feeds an
//     operator explicitly licensed and configured, one document each, at a
//     rate gentler than a reader with a feed reader; that is not crawling
//     and robots.txt has nothing to say about it. FR-013/D6 is the opposite
//     direction of travel - blocking crawlers on our own site - and is not
//     an obligation this client inherits.
//
// The retrieved document is handed straight to ParseFeed, never read into a
// buffer and never re-encoded: MaxFeedBytes does the bounding, and the bytes
// the database hashes must be the bytes that arrived (I-2).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults applied to a FetchConfig's zero values. They suit a poll loop
// walking a handful of municipal feeds: a generous single-attempt deadline,
// a couple of retries for a source having a bad minute, and then the source
// is left alone until the next interval rather than hammered.
const (
	// DefaultFetchTimeout bounds one attempt, the body read included.
	DefaultFetchTimeout = 30 * time.Second
	// DefaultFetchMaxAttempts is the total number of attempts, retries
	// included.
	DefaultFetchMaxAttempts = 3
	// DefaultFetchBaseBackoff is the wait before the second attempt; each
	// further wait doubles it.
	DefaultFetchBaseBackoff = 2 * time.Second
	// DefaultUserAgent identifies the software and points at somewhere a
	// source can read what we do and complain. A deployment should replace
	// it with one naming the operator and a real contact address - being
	// identifiable is the point, and "apivo-news" identifies the code
	// rather than whoever is running it.
	DefaultUserAgent = "apivo-news/0.1 (+https://github.com/Nomos-N4s/apivo-news)"
)

const (
	// maxFetchBackoff caps every wait, including one a source asks for in
	// Retry-After: a source demanding minutes gets a bounded wait and is
	// then left to the next poll, rather than a worker sleeping on it.
	maxFetchBackoff = 30 * time.Second
	// maxFeedRedirects bounds a redirect chain. Feeds move, and one or two
	// hops (http to https, a CDN) are ordinary; a chain longer than this is
	// a loop or a trap.
	maxFeedRedirects = 5
	// feedErrorSnippet bounds how much of a refusal is quoted back in the
	// error, so a source's HTML error page cannot land whole in a log line.
	// It is worth quoting at all because "403" and "403: crawler blocked"
	// call for very different conversations with the publisher.
	feedErrorSnippet = 256
	// feedAccept states what we can parse, in the order we prefer it, and
	// still accepts anything: plenty of feeds are served as text/xml or
	// even text/plain, and refusing those would be pedantry.
	feedAccept = "application/atom+xml, application/rss+xml, application/xml;q=0.9, */*;q=0.8"
)

// Errors describing why one retrieval produced nothing. They are diagnoses
// for the poll loop: rate limiting and unavailability mean come back later,
// everything else means this source needs a human to look at it.
var (
	// ErrUnsafeLocation reports a feed URL - configured, or arrived at by
	// redirect - that is not http or https. This client speaks those two
	// and nothing else; a "file://" or "gopher://" location is either a
	// configuration mistake or an attempt to make us read something that
	// is not a feed, and both are refused before a byte is requested.
	ErrUnsafeLocation = errors.New("ingestion: feed location is not http or https")
	// ErrRateLimited reports a source asking us to slow down.
	ErrRateLimited = errors.New("ingestion: source refused the request as too frequent")
	// ErrUnavailable reports a source that could not be reached or that
	// failed on its own side.
	ErrUnavailable = errors.New("ingestion: source could not be reached")
	// ErrTimeout reports an attempt that ran out of time.
	ErrTimeout = errors.New("ingestion: source did not answer in time")
	// ErrRefused reports a source rejecting the request on its own terms -
	// gone, forbidden, moved without a redirect. Retrying changes nothing.
	ErrRefused = errors.New("ingestion: source refused the request")
)

// FetchConfig describes how to ask one source for its feed. It is plain
// data with usable zero values, so a caller that has no opinion states none
// and gets the Default* constants.
type FetchConfig struct {
	// Client performs the request. Nil means http.DefaultClient. Whatever
	// is supplied is used for its transport - and therefore its connection
	// pool - but never for its redirect policy: see Fetch.
	Client *http.Client

	// UserAgent identifies us to the source on every request. Empty means
	// DefaultUserAgent; it is never sent empty and never sent as a
	// browser's.
	UserAgent string

	// Timeout bounds one attempt, including reading and parsing the feed.
	// Zero means DefaultFetchTimeout.
	Timeout time.Duration

	// BaseBackoff is the wait before the second attempt, doubling for each
	// attempt after that. Zero means DefaultFetchBaseBackoff.
	BaseBackoff time.Duration

	// MaxAttempts is the total number of attempts, the first included.
	// Zero means DefaultFetchMaxAttempts; one disables retrying.
	MaxAttempts int
}

// Validators are the conditional-GET tokens this source last gave us,
// stored between polls. Empty fields are simply not sent, which is what a
// first-ever poll looks like.
type Validators struct {
	// ETag is the source's last ETag, sent back as If-None-Match.
	ETag string
	// LastModified is the source's last Last-Modified, sent back as
	// If-Modified-Since. It is carried as the source wrote it: it is an
	// opaque token to us, and reformatting a date we did not author is a
	// good way to stop matching it.
	LastModified string
}

// Result is one retrieval's outcome.
type Result struct {
	// NotModified reports a 304: the source confirmed nothing has changed
	// since the validators we sent. Items is empty and this is not an
	// error - it is the outcome conditional GET exists to produce.
	NotModified bool

	// Items are the feed's entries, normalised. Every entry is returned,
	// including ones Validate would reject; the caller decides.
	Items []NormalizedItem

	// ETag and LastModified are what the source stated this time, to be
	// stored and sent back on the next poll. Empty means the response
	// carried none, and the caller keeps whatever it already had.
	ETag         string
	LastModified string

	// RetryAfter is how long a rate-limited source asked us to wait,
	// already clamped to the same ceiling our own backoff uses. It is set
	// on the failure it describes, so the poll loop can defer this one
	// source without reading the error's prose.
	RetryAfter time.Duration
}

// Fetch performs one conditional GET of feedURL and returns its entries.
//
// A 304 returns NotModified with no items and no error. A source that is
// busy, unwell or slow is retried up to MaxAttempts with exponential
// backoff, honouring a Retry-After it asks for but never waiting longer
// than maxFetchBackoff; anything else - a refusal, an unparsable document,
// a feed over MaxFeedBytes - is returned at once, because trying again
// would only produce it again.
func (c FetchConfig) Fetch(ctx context.Context, feedURL string, validators Validators) (Result, error) {
	if err := checkFeedLocation(feedURL); err != nil {
		return Result{}, err
	}
	cfg := c.withDefaults()

	var (
		lastErr    error
		retryAfter time.Duration
	)
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		result, wait, err := cfg.attempt(ctx, feedURL, validators)
		if err == nil {
			return result, nil
		}
		lastErr, retryAfter = err, wait
		if attempt == cfg.MaxAttempts || !retryableFetch(err) || ctx.Err() != nil {
			break
		}
		if err := sleepContext(ctx, fetchBackoff(attempt, cfg.BaseBackoff, wait)); err != nil {
			return Result{RetryAfter: retryAfter}, waitInterrupted(err, lastErr)
		}
	}
	// The wait a source asked for outlives the attempts: it is the one
	// piece of a failure the poll loop can act on.
	return Result{RetryAfter: retryAfter}, lastErr
}

// withDefaults fills the zero values in and returns the configuration this
// call will actually use, leaving the caller's own struct alone.
func (c FetchConfig) withDefaults() FetchConfig {
	if c.Timeout <= 0 {
		c.Timeout = DefaultFetchTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultFetchMaxAttempts
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = DefaultFetchBaseBackoff
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = DefaultUserAgent
	}

	base := c.Client
	if base == nil {
		base = http.DefaultClient
	}
	// A copy, so the redirect policy is this fetcher's and does not become
	// a property of a client the caller may share with anything else. The
	// copy keeps the original's Transport, so the connection pool - the
	// part worth sharing - still is.
	guarded := *base
	guarded.CheckRedirect = refuseUnsafeLocation
	c.Client = &guarded
	return c
}

// attempt performs one request. It returns the wait the source asked for
// separately from the error, because a rate limit is the one failure that
// carries an instruction.
func (c FetchConfig) attempt(ctx context.Context, feedURL string, validators Validators) (Result, time.Duration, error) {
	// A context that has already ended means the request never leaves.
	// Checked here rather than left to the transport so the outcome is
	// unambiguous.
	if err := ctx.Err(); err != nil {
		return Result{}, 0, callerContextError(err)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, feedURL, nil)
	if err != nil {
		return Result{}, 0, fmt.Errorf("ingestion: building the request for %q: %w", feedURL, err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", feedAccept)
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return Result{}, 0, c.transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return Result{
			NotModified:  true,
			ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
			LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		}, 0, nil
	case resp.StatusCode != http.StatusOK:
		return Result{}, feedRetryAfter(resp.Header), statusError(resp.StatusCode, resp.Body)
	}

	// A declared length over the bound is refused before the parser reads
	// a byte of it. ParseFeed's own bound is still the guarantee - a
	// chunked response declares no length, and a declared one is only a
	// claim - but there is no reason to start parsing a document the
	// source has already told us we will have to throw away.
	if resp.ContentLength > MaxFeedBytes {
		return Result{}, 0, fmt.Errorf("ingestion: source declared a %d-byte feed, over the %d-byte limit: %w", resp.ContentLength, int64(MaxFeedBytes), ErrFeedTooLarge)
	}

	// Straight to the parser: not read into a buffer, not decoded and
	// re-encoded. What the write path stores, and what the database hashes
	// into source_item.content_hash, has to be what the source sent (I-2).
	items, err := ParseFeed(resp.Body)
	if err != nil {
		return Result{}, 0, c.parseFailure(ctx, attemptCtx, err)
	}
	return Result{
		Items:        items,
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
	}, 0, nil
}

// parseFailure decides what a failed parse really was. A document that
// stopped arriving because time ran out is a timeout and worth retrying; a
// document that arrived and made no sense is not, and saying so would send
// the poll loop back for the same broken feed twice more.
func (c FetchConfig) parseFailure(ctx, attemptCtx context.Context, err error) error {
	if errors.Is(err, ErrFeedTooLarge) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return callerContextError(ctxErr)
	}
	if attemptCtx.Err() != nil {
		return fmt.Errorf("ingestion: the feed stopped arriving within %s: %w", c.Timeout, ErrTimeout)
	}
	return err
}

// transportError classifies an exchange that never completed, keeping the
// caller's own cancellation distinct from our per-attempt deadline.
func (c FetchConfig) transportError(ctx context.Context, err error) error {
	// A refused location is a decision this client made, not a transport
	// hiccup, so it is neither reclassified nor retried. The transport
	// wraps it in a *url.Error, which unwraps to the sentinel.
	if errors.Is(err, ErrUnsafeLocation) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return callerContextError(ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ingestion: no answer from the feed within %s: %w", c.Timeout, ErrTimeout)
	}
	return fmt.Errorf("ingestion: fetching the feed: %w: %w", err, ErrUnavailable)
}

// callerContextError renders the caller's own deadline or cancellation. A
// deadline is a timeout wherever it was noticed; a cancellation stays a
// cancellation, because a shutting-down poll loop has not been failed by
// the source.
func callerContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ingestion: the caller's deadline passed before the feed answered: %w: %w", err, ErrTimeout)
	}
	return fmt.Errorf("ingestion: the feed fetch was abandoned: %w", err)
}

// waitInterrupted builds the failure for a context that ended while we were
// waiting to retry, keeping the source's own last word in the chain: a poll
// loop deciding how to treat this source needs to know it was being rate
// limited, not merely that a context ended.
func waitInterrupted(waitErr, lastErr error) error {
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return fmt.Errorf("ingestion: the caller's deadline passed while waiting to retry: %w: %w: %w", waitErr, ErrTimeout, lastErr)
	}
	return fmt.Errorf("ingestion: the feed fetch was abandoned while waiting to retry: %w: %w", waitErr, lastErr)
}

// statusError maps a status the fetcher cannot use onto this module's
// vocabulary, quoting enough of the response to tell a rate limit apart
// from a publisher who has decided they would rather we did not.
func statusError(status int, body io.Reader) error {
	snippet := snippetOf(body)
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("ingestion: source answered %d: %w: %s", status, ErrRateLimited, snippet)
	case status >= 500:
		return fmt.Errorf("ingestion: source answered %d: %w: %s", status, ErrUnavailable, snippet)
	default:
		return fmt.Errorf("ingestion: source answered %d: %w: %s", status, ErrRefused, snippet)
	}
}

// snippetOf quotes the start of a refusal, in whole runes.
func snippetOf(body io.Reader) string {
	// One rune is up to four bytes, so reading the limit in runes' worth of
	// bytes is what guarantees there is enough to trim back from.
	raw, err := io.ReadAll(io.LimitReader(body, 4*feedErrorSnippet))
	if err != nil {
		return "(unreadable body)"
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if text == "" {
		return "(empty body)"
	}
	runes := []rune(text)
	if len(runes) <= feedErrorSnippet {
		return text
	}
	return string(runes[:feedErrorSnippet]) + "..."
}

// checkFeedLocation refuses a configured feed URL this client will not
// speak, before any request is built.
func checkFeedLocation(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("ingestion: feed URL %q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("ingestion: feed URL %q has scheme %q: %w", raw, parsed.Scheme, ErrUnsafeLocation)
	}
	if parsed.Host == "" {
		return fmt.Errorf("ingestion: feed URL %q has no host: %w", raw, ErrUnsafeLocation)
	}
	return nil
}

// refuseUnsafeLocation is the redirect policy. A source is free to move its
// feed; it is not free to move it somewhere this client does not speak, and
// following a hop to another scheme is how a redirect turns into a request
// for a local file. The target is described by scheme alone - a redirect
// URL can carry credentials, and an error message is not the place for them.
func refuseUnsafeLocation(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("ingestion: feed redirected to a %q location: %w", req.URL.Scheme, ErrUnsafeLocation)
	}
	if len(via) >= maxFeedRedirects {
		return fmt.Errorf("ingestion: feed redirected more than %d times: %w", maxFeedRedirects, ErrUnavailable)
	}
	return nil
}

// feedRetryAfter reads the wait a source asked for, already clamped. Only
// the delta-seconds form is read, which is what a rate limiter sends; an
// HTTP-date falls through to our own backoff rather than being guessed at.
func feedRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) {
		return 0
	}
	return min(time.Duration(seconds*float64(time.Second)), maxFetchBackoff)
}

// fetchBackoff is the wait before the attempt after this one: the source's
// own request when it made one, otherwise exponential from the base. Both
// are capped, so no single source can hold a worker for minutes.
func fetchBackoff(attempt int, base, requested time.Duration) time.Duration {
	if requested > 0 {
		return min(requested, maxFetchBackoff)
	}
	wait := base
	for i := 1; i < attempt && wait < maxFetchBackoff; i++ {
		wait *= 2
	}
	return min(wait, maxFetchBackoff)
}

// retryableFetch reports whether asking the same source again could
// plausibly work: it was busy, unwell or slow. A refusal, an unsafe
// location or an unparsable document would only happen again.
func retryableFetch(err error) bool {
	return errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrUnavailable) ||
		errors.Is(err, ErrTimeout)
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
