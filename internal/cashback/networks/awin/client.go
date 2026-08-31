// The authenticated, paced, retried transport every Awin call goes through
// (T137, ADR-0003).
//
// It is a transport and not the port: it knows how to reach Awin and what
// their HTTP outcomes mean in the port's vocabulary, and nothing about
// transactions or merchants. The port methods are built on it.

package awin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// ID names the network this adapter speaks to, in the vocabulary the
// network table is keyed by.
const ID networks.NetworkID = "awin"

// DefaultBaseURL is the root every Awin endpoint is relative to.
//
//	https://help.awin.com/apidocs/introduction-1
const DefaultBaseURL = "https://api.awin.com"

// DocumentedRateLimitPerMinute is what Awin publishes: "a throttling system
// in place that limits the number of API requests to 20 API calls per
// minute per user".
//
// It is a DEFAULT and not a constant the client enforces. The rate a
// deployment actually paces to comes from cashback.network, because a limit
// Awin raises for an account is a row an operator edits rather than a
// release; this is what a deployment gets when nobody has said otherwise.
const DocumentedRateLimitPerMinute = 20

// secondsPerMinute converts the rate the network row carries into the rate
// networks.RateLimiter takes.
//
// This division is the whole of the unit conversion, and it is written once
// here rather than at each call site for the reason RateLimiter.Rate() was
// made observable: its own doc comment warns of "an adapter that passed its
// network's declared limit through a unit-conversion bug would build a
// limiter at sixty requests a second". One site can be tested; several
// cannot all be remembered.
const secondsPerMinute = 60.0

// defaultRequestTimeout bounds a single request.
//
// Separate from the retry policy's MaxElapsed, which bounds the whole
// sequence, because RetryBackoffPolicy.MaxElapsed says so in as many words:
// "an adapter that wants an individual request cut off sooner than the
// whole budget still sets that on its own client". Without it a connection
// that is accepted and never answered would consume the entire budget in
// one attempt, and the retries the budget was sized for would never happen.
const defaultRequestTimeout = 30 * time.Second

var (
	// ErrNoPublisherAccount reports a client built without the account it
	// polls for. Refused at construction: an adapter's account is its
	// identity, and one discovered missing later is discovered with a
	// poller already running.
	ErrNoPublisherAccount = errors.New("awin: a client needs the publisher account it polls for")
	// ErrNoToken reports a client built with no credential.
	//
	// Refused rather than defaulted to the empty string, which would send
	// unauthenticated requests and read Awin's 401 as though the account
	// had been rejected - a deployment would then be told its publisher
	// account was refused when what was missing was NETWORK_API_KEY.
	ErrNoToken = errors.New("awin: a client needs an access token; set NETWORK_API_KEY")
	// ErrNotConfigured reports a client that could not be built from what
	// it was given: an unusable base URL, or a rate that paces nothing.
	ErrNotConfigured = errors.New("awin: the client could not be configured")
)

// Client performs authenticated requests against Awin, paced to the rate
// the deployment configured and retried under the port's own backoff.
type Client struct {
	account networks.PublisherAccount
	token   string
	base    *url.URL
	http    *http.Client
	limiter *networks.RateLimiter
	retry   *networks.RetryBackoff
}

// Option configures a [Client].
type Option func(*settings)

// settings collects what the options set, so New can validate the whole
// picture once rather than each option guessing at the others.
type settings struct {
	token         string
	baseURL       string
	ratePerMinute int
	burst         int
	httpClient    *http.Client
	policy        networks.RetryBackoffPolicy
	clock         networks.RateLimitClock
}

// WithToken supplies the access token. It reaches the client from the
// environment through the composition root and is never stored.
func WithToken(token string) Option {
	return func(s *settings) { s.token = strings.TrimSpace(token) }
}

// WithBaseURL overrides [DefaultBaseURL]. Tests point it at a local server;
// a deployment has no reason to set it.
func WithBaseURL(raw string) Option {
	return func(s *settings) { s.baseURL = strings.TrimSpace(raw) }
}

// WithRateLimitPerMinute paces the client to the rate the network row
// carries (cashback.network.rate_limit_per_minute).
//
// Per minute because that is the unit Awin publishes and the column now
// holds; the division into the per-second rate the limiter takes happens
// here, once. See [secondsPerMinute].
func WithRateLimitPerMinute(perMinute int) Option {
	return func(s *settings) { s.ratePerMinute = perMinute }
}

// WithBurst allows burst requests to be taken at once. The default is one:
// a limit of twenty a minute is not an invitation to spend twenty at once,
// and a poller walking windows has no reason to.
func WithBurst(burst int) Option {
	return func(s *settings) { s.burst = burst }
}

// WithHTTPClient replaces the HTTP client, for tests and for a deployment
// that must route through a proxy.
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithRetryPolicy replaces the retry policy.
func WithRetryPolicy(p networks.RetryBackoffPolicy) Option {
	return func(s *settings) { s.policy = p }
}

// WithClock replaces the clock the limiter and the backoff read, so a test
// can exercise pacing without sleeping.
func WithClock(clock networks.RateLimitClock) Option {
	return func(s *settings) { s.clock = clock }
}

// New builds the transport for one publisher account.
func New(account networks.PublisherAccount, opts ...Option) (*Client, error) {
	if err := account.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoPublisherAccount, err)
	}
	s := settings{
		baseURL:       DefaultBaseURL,
		ratePerMinute: DocumentedRateLimitPerMinute,
		burst:         1,
		httpClient:    &http.Client{Timeout: defaultRequestTimeout},
	}
	for _, opt := range opts {
		opt(&s)
	}
	if s.token == "" {
		return nil, ErrNoToken
	}

	base, err := url.Parse(s.baseURL)
	switch {
	case err != nil:
		// The value is not repeated: a base URL somebody pasted a token
		// into would otherwise print it.
		return nil, fmt.Errorf("%w: the base URL will not parse (the value is not repeated here: it may carry a credential)", ErrNotConfigured)
	case base.Scheme != "https":
		// https only, and refused rather than upgraded. Awin says "only
		// forwarding from HTTPS is supported and not from HTTP", and a
		// token sent over http is a token on the wire whatever the server
		// then does with it. Tests point at httptest.NewTLSServer.
		return nil, fmt.Errorf("%w: the base URL must be https, not %q", ErrNotConfigured, base.Scheme)
	case base.Host == "":
		return nil, fmt.Errorf("%w: the base URL names no host", ErrNotConfigured)
	}

	var limiterOpts []networks.RateLimiterOption
	var retryOpts []networks.RetryBackoffOption
	if s.clock != nil {
		limiterOpts = append(limiterOpts, networks.WithRateLimiterClock(s.clock))
		retryOpts = append(retryOpts, networks.WithRetryBackoffClock(s.clock))
	}
	limiter, err := networks.NewRateLimiter(float64(s.ratePerMinute)/secondsPerMinute, s.burst, limiterOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotConfigured, err)
	}
	retry, err := networks.NewRetryBackoff(s.policy, retryOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotConfigured, err)
	}

	return &Client{
		account: account,
		token:   s.token,
		base:    base,
		http:    s.httpClient,
		limiter: limiter,
		retry:   retry,
	}, nil
}

// Account is the publisher account this client polls for.
func (c *Client) Account() networks.PublisherAccount { return c.account }

// RateLimit reports the rate the client is pacing to, in requests a second.
//
// Exposed for the reason networks.RateLimiter.Rate() is: the conversion
// from the network's published per-minute figure is the one place a unit
// bug can hide, and a bug there is invisible from outside unless the number
// actually in force can be read back.
func (c *Client) RateLimit() float64 { return c.limiter.Rate() }

// Get performs an authenticated GET of one path relative to the base URL,
// paced and retried, and answers the whole body.
//
// The composition is the one RetryBackoff.Do documents: the limiter goes
// INSIDE the attempt, because Do re-issues the operation and a limiter
// outside would spend a single token on an entire retry sequence.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	target := c.base.JoinPath(path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	var body []byte
	err := c.retry.Do(ctx, c.limiter.Pace(func(ctx context.Context) error {
		var err error
		body, err = c.once(ctx, target.String())
		return err
	}))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// once performs a single request and maps its outcome onto the port's
// error vocabulary.
func (c *Client) once(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building the request: %w", networks.ErrNetworkUnavailable, err)
	}
	// The token travels in the header and NOWHERE else. Awin also accepts
	// it as an `accessToken` query parameter, and that is the form to
	// refuse: a URL is written to access logs, proxy logs and error
	// messages by everything it passes through, and a credential in one is
	// a credential in all of them.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Retryable: a connection that failed to open, or died mid-flight,
		// is the classic blip a second attempt survives. Not repeated
		// verbatim - url.Error carries the request URL, and while this
		// client never puts the token in one, an operator's mistyped base
		// URL might.
		return nil, networks.NewRetryableError(
			fmt.Errorf("%w: %s", networks.ErrNetworkUnavailable, redactedTransportError(err)), 0)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, c.refusal(resp)
	}

	read, err := io.ReadAll(resp.Body)
	if err != nil {
		// A body that stops mid-read is a truncated answer, and a
		// truncated page of transactions parsed as a whole one is a window
		// silently missing its tail. Retryable: the next attempt asks for
		// the same window and either gets it whole or fails again.
		return nil, networks.NewRetryableError(
			fmt.Errorf("%w: reading Awin's answer: %w", networks.ErrNetworkUnavailable, err), 0)
	}
	return read, nil
}

// refusal turns a non-200 into the port's vocabulary, marked retryable or
// terminal so RetryBackoff.Do knows whether asking again could help.
//
// The classification is networks.RetryableHTTPStatus's and not this
// package's. That matters more than it looks: it already excludes 501, which
// is a statement about the endpoint that will be just as true in thirty
// seconds, and 499, which is a front-end recording that WE hung up. An
// adapter re-deriving the set by hand gets those wrong and turns a clear
// error into a slow one.
func (c *Client) refusal(resp *http.Response) error {
	retryAfter, askErr := networks.RetryAfterFromHeader(resp.Header)
	if askErr != nil {
		// Unreadable rather than absent, and the difference is the
		// difference between backing off and being banned: a CDN in front
		// of Awin can answer a 429 with the HTTP-date form, which this
		// deliberately does not parse. The ask is dropped - the adapter's
		// own backoff still applies - but it is said out loud rather than
		// silently read as "no wait asked for".
		retryAfter = 0
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Terminal, explicitly. Re-sending a rejected credential cannot
		// start working, and a poller hammering a 401 looks to Awin
		// exactly like an attack on the account.
		return networks.NewTerminalError(fmt.Errorf(
			"%w: Awin answered %d for publisher %s", networks.ErrNetworkRefused, resp.StatusCode, c.account.ExternalID()))
	case resp.StatusCode == http.StatusTooManyRequests:
		return networks.NewRetryableError(fmt.Errorf(
			"%w: Awin answered %d%s", networks.ErrNetworkRateLimited, resp.StatusCode, askNote(retryAfter, askErr)), retryAfter)
	case networks.RetryableHTTPStatus(resp.StatusCode):
		return networks.NewRetryableError(fmt.Errorf(
			"%w: Awin answered %d%s", networks.ErrNetworkUnavailable, resp.StatusCode, askNote(retryAfter, askErr)), retryAfter)
	default:
		// Every other 4xx is our request being wrong - a malformed window,
		// an unknown parameter - and no amount of retrying fixes it. It is
		// reported as unavailable rather than refused so an operator is not
		// sent to Awin support over a date range we built badly.
		return networks.NewTerminalError(fmt.Errorf(
			"%w: Awin answered %d", networks.ErrNetworkUnavailable, resp.StatusCode))
	}
}

// askNote renders what the network said about coming back, including the
// case where it said something unreadable.
func askNote(retryAfter time.Duration, askErr error) string {
	switch {
	case askErr != nil:
		return " (with an unreadable Retry-After, so only our own backoff applies)"
	case retryAfter > 0:
		return fmt.Sprintf(" (Retry-After %s)", retryAfter)
	default:
		return ""
	}
}

// redactedTransportError renders a transport failure without the URL it
// was attempting.
func redactedTransportError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + urlErr.Err.Error()
	}
	return err.Error()
}
