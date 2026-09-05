// The authenticated, paced, retried transport every Linkwise call goes
// through (T243, ADR-0003).
//
// It is a transport and not the port: it knows how to reach Linkwise and what
// their HTTP outcomes mean in the port's vocabulary, and nothing about
// transactions or merchants. The port methods are built on it.
//
// Three things here are Linkwise's rather than a generic REST client's, and
// each was learned from the live API rather than from a document:
//
//   - format=json travels on EVERY request, including ones expected to fail.
//     Without it a 400 answers with an HTML page and an adapter reports a
//     parse error while discarding what the network actually said.
//   - Dates are DAY-FIRST, 31/12/2010. A window rendered month-first is a
//     different window that the API answers perfectly happily.
//   - The credential is two values, a username and a password, sent as HTTP
//     Basic. Linkwise's own integrators put them in the URL; this does not.

package linkwise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// DefaultBaseURL is the root every Linkwise endpoint is relative to.
//
// The version is part of the path rather than a header, and it is pinned:
// 1.1 is the version the recordings in testdata/ were captured from, and a
// network that ships a 1.2 with a renamed field would otherwise change this
// adapter's meaning without changing a line of it.
const DefaultBaseURL = "https://affiliate.linkwi.se"

// apiPath is the prefix every endpoint below sits under.
const apiPath = "/api/1.1"

// secondsPerMinute converts the rate the network row carries into the rate
// networks.RateLimiter takes.
//
// Written once, here, for the reason the Awin client gives for its own: a
// unit-conversion bug in this division is invisible from outside unless the
// number actually in force can be read back, which is what [Client.RateLimit]
// is for.
const secondsPerMinute = 60.0

// defaultRequestTimeout bounds a single request, and is sized from the
// measurements rather than copied.
//
// A full-width window costs about two seconds and the widest window ever
// measured - a year, which this adapter refuses - took eighty-one. Thirty
// seconds is therefore roughly fifteen times what a legal request should
// cost: generous enough that a slow afternoon is not an outage, tight enough
// that a connection accepted and never answered does not eat the whole retry
// budget in one attempt.
const defaultRequestTimeout = 30 * time.Second

// linkwiseDateLayout is how Linkwise writes a date in a query: DAY first.
//
//	from       Its value is of the format 31/12/2010
//
// Written as a named constant rather than inline at the two call sites
// because 02/01/2006 and 01/02/2006 differ by one transposition and produce
// no error - the API answers a month-first window as cheerfully as a
// day-first one, with transactions from the wrong period. The recorded usage
// text is quoted above so the next reader can check the layout against the
// network's own words without leaving the file.
const linkwiseDateLayout = "02/01/2006"

// linkwiseTimeLayout is the companion from_time/to_time format, 00:00:00.
const linkwiseTimeLayout = "15:04:05"

// errorBodyLimit caps how much of a refusal's body is read.
//
// Linkwise's 400 carries the API's entire usage text - about four kilobytes -
// as the error description, and that text is the only documentation this API
// has. It is worth reading and worth truncating: [firstLine] takes the
// sentence, the rest is what testdata/error-400.json is for.
const errorBodyLimit = 16 << 10

var (
	// ErrNoPublisherAccount reports a client built without the account it
	// polls for. Refused at construction: an adapter's account is its
	// identity, and one discovered missing later is discovered with a
	// poller already running.
	ErrNoPublisherAccount = errors.New("linkwise: a client needs the publisher account it polls for")
	// ErrNoCredential reports a client built with half a credential or none.
	//
	// Both halves are required because Linkwise authenticates with HTTP
	// Basic and a request carrying a username and an empty password is a
	// well-formed request that is refused - which this adapter would read as
	// the account having been rejected. A deployment would then be told its
	// publisher account was refused when what was missing was an environment
	// variable.
	ErrNoCredential = errors.New("linkwise: a client needs both halves of the credential; set NETWORK_LINKWISE_API_KEY and NETWORK_LINKWISE_API_SECRET")
	// ErrNotConfigured reports a client that could not be built from what
	// it was given: an unusable base URL, or a rate that paces nothing.
	ErrNotConfigured = errors.New("linkwise: the client could not be configured")
	// ErrNoReportCurrency reports a client built without being told which
	// currency this publisher account's report is denominated in.
	//
	// THE REPORT CARRIES NO CURRENCY FIELD. That is not an omission in this
	// adapter: the endpoint's own usage text lists every field the report can
	// return - program_id, program, program_cat, rotator, creative,
	// subid1..subid5, transaction_id, type, status, subaction, amended,
	// amount, commission, click_date, transaction_date, status_date,
	// click_ref_url, payout_cat, payment_status - and there is no currency
	// among them. The programme list carries one per programme; the
	// transaction report carries none at all.
	//
	// So it has to come from configuration, and it is REQUIRED rather than
	// defaulted to EUR. A default would be right for the Greek programmes
	// this account is joined to and wrong the day one of them is Romanian,
	// and the failure would be silent: RON amounts stored as EUR, a member
	// paid roughly five times what they earned, and nothing in the evidence
	// saying which currency the number was.
	//
	// It is a per-ACCOUNT declaration, and the recording of the programme
	// list (testdata/programs.json) now says that assumption is FALSE for the
	// account it was captured from. Of the 334 joined programmes, 329 report
	// in EUR, three in PLN and two in USD - so a single declared currency is
	// right for 98.5% of them and silently wrong for the rest, storing zloty
	// as euro at whatever this deployment declares.
	//
	// The fix is a join rather than a better default: every transaction row
	// names its programme (program.id) and every programme names its
	// currency, so the adapter can read the currency off the programme rather
	// than off configuration. That is a real change - the client would hold
	// a programme-to-currency map and decide when to refresh it - and it is
	// not this constructor's to make silently. Until it lands, this value is
	// the deployment's stated assumption, wrong for five programmes out of
	// three hundred and thirty-four, and TestTheCurrencyIsPerProgrammeAndNot
	// Uniform is what keeps that fact in the suite rather than only here.
	ErrNoReportCurrency = errors.New("linkwise: a client needs the currency its report is denominated in; the transaction report carries none")
)

// Client performs authenticated requests against Linkwise, paced to the rate
// the deployment configured and retried under the port's own backoff.
type Client struct {
	account  networks.PublisherAccount
	currency money.Currency
	username string
	password string
	base     *url.URL
	http     *http.Client
	limiter  *networks.RateLimiter
	retry    *networks.RetryBackoff
}

// Option configures a [Client].
type Option func(*settings)

// settings collects what the options set, so New can validate the whole
// picture once rather than each option guessing at the others.
type settings struct {
	username      string
	password      string
	currency      string
	baseURL       string
	ratePerMinute int
	burst         int
	httpClient    *http.Client
	policy        networks.RetryBackoffPolicy
	clock         networks.RateLimitClock
}

// WithCredential supplies the HTTP Basic pair. Both halves reach the client
// from the environment through the composition root and neither is stored
// anywhere else.
func WithCredential(username, password string) Option {
	return func(s *settings) {
		s.username = strings.TrimSpace(username)
		// The password is NOT trimmed. A credential is bytes somebody was
		// issued, and trimming one silently sends a different secret than
		// the operator set - which fails as an authentication error and
		// sends them looking at the account rather than at the whitespace.
		s.password = password
	}
}

// WithReportCurrency states which currency this publisher account's
// transaction report is denominated in, as an ISO-4217 code.
//
// Required: see [ErrNoReportCurrency] for why the report itself cannot
// answer this and why no default is chosen.
func WithReportCurrency(code string) Option {
	return func(s *settings) { s.currency = strings.ToUpper(strings.TrimSpace(code)) }
}

// WithBaseURL overrides [DefaultBaseURL]. Tests point it at a local server;
// a deployment has no reason to set it.
func WithBaseURL(raw string) Option {
	return func(s *settings) { s.baseURL = strings.TrimSpace(raw) }
}

// WithRateLimitPerMinute paces the client to the rate the network row
// carries (cashback.network.rate_limit_per_minute), defaulting to what
// [Limits] declares.
func WithRateLimitPerMinute(perMinute int) Option {
	return func(s *settings) { s.ratePerMinute = perMinute }
}

// WithBurst allows burst requests to be taken at once. The default is one,
// and for this network the default is more than usually right: a request
// costs a second at minimum and grows with the window, so there is nothing a
// burst would buy that the pacing was in the way of.
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
		ratePerMinute: requestsPerMinute,
		burst:         1,
		httpClient:    &http.Client{Timeout: defaultRequestTimeout},
	}
	for _, opt := range opts {
		opt(&s)
	}
	if s.username == "" || s.password == "" {
		return nil, ErrNoCredential
	}
	currency, err := money.ParseCurrency(s.currency)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoReportCurrency, err)
	}

	base, err := url.Parse(s.baseURL)
	switch {
	case err != nil:
		// The value is not repeated: a base URL somebody pasted a
		// credential into would otherwise print it.
		return nil, fmt.Errorf("%w: the base URL will not parse (the value is not repeated here: it may carry a credential)", ErrNotConfigured)
	case base.Scheme != "https":
		// https only, and refused rather than upgraded. HTTP Basic is a
		// credential in a header on every single request, base64 of the
		// plaintext and nothing more, so a client that would fall back to
		// http is one transposed character away from publishing the
		// publisher account's password to every hop on the route.
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
		account:  account,
		currency: currency,
		username: s.username,
		password: s.password,
		base:     base,
		http:     s.httpClient,
		limiter:  limiter,
		retry:    retry,
	}, nil
}

// ID names the network this adapter speaks to. It is constant for the life
// of the client: the id is how a stored row is traced back to the code that
// wrote it, so an adapter whose answer varied would strand its own evidence.
func (c *Client) ID() networks.NetworkID { return ID }

// Account is the publisher account this client polls for.
func (c *Client) Account() networks.PublisherAccount { return c.account }

// Limits is what this adapter tells the poller it may ask for. It is the
// package's own declaration; the client carries it so that the [Client] alone
// satisfies the part of the port that describes how it may be queried.
func (c *Client) Limits() networks.Limits { return Limits() }

// RateLimit reports the rate the client is pacing to, in requests a second.
func (c *Client) RateLimit() float64 { return c.limiter.Rate() }

// ReportCurrency is the currency this account's report is read as. Exposed
// because it is configuration rather than an answer from the network, and an
// operator reconciling a statement has to be able to see which currency the
// stored numbers were taken to be.
func (c *Client) ReportCurrency() money.Currency { return c.currency }

// Get performs an authenticated GET of one API endpoint, paced and retried,
// and answers the whole body.
//
// The endpoint is a bare file name - "reports_transaction.html" - and the
// version prefix is this client's, not the caller's. Callers therefore
// cannot reach a path outside the API, and the version is pinned in exactly
// one place.
//
// The composition is the one RetryBackoff.Do documents: the limiter goes
// INSIDE the attempt, because Do re-issues the operation and a limiter
// outside would spend a single token on an entire retry sequence.
func (c *Client) Get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	target := c.base.JoinPath(apiPath, endpoint)
	target.RawQuery = withMachineReadableAnswers(query).Encode()

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

// withMachineReadableAnswers adds the two global options every request this
// adapter makes carries, and returns a copy so a caller's own values are
// never mutated.
//
// format=json is not merely how the success body is chosen. Recorded: the
// SAME malformed request answers 400 with an HTML page without it and with a
// structured {code, name, description} object with it. So it travels on
// every request including ones expected to fail, which is the only reason
// [refusal] below has anything to read.
//
// timezone=UTC is sent although UTC is already the documented default. A
// [networks.QueryWindow] is a pair of instants and this adapter renders them
// in UTC; a network that revised its default to Europe/Athens would
// otherwise shift every window by two or three hours - silently, seasonally,
// and only at the seams.
func withMachineReadableAnswers(query url.Values) url.Values {
	full := url.Values{}
	for key, values := range query {
		full[key] = values
	}
	full.Set("format", "json")
	full.Set("timezone", "UTC")
	return full
}

// once performs a single request and maps its outcome onto the port's
// error vocabulary.
func (c *Client) once(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building the request: %w", networks.ErrNetworkUnavailable, err)
	}
	// The credential travels in the header and NOWHERE else. Linkwise's own
	// integrators put it in the URL - the WordPress plugin that documents
	// rest_programs.html embeds user:pass in the address - and that is the
	// form to refuse: a URL is written to access logs, proxy logs and error
	// messages by everything it passes through, and a credential in one is a
	// credential in all of them.
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Retryable: a connection that failed to open, or died mid-flight,
		// is the classic blip a second attempt survives. Not repeated
		// verbatim - url.Error carries the request URL, and while this
		// client never puts a credential in one, an operator's mistyped
		// base URL might.
		return nil, networks.NewRetryableError(
			fmt.Errorf("%w: %s", networks.ErrNetworkUnavailable, redactedTransportError(err)), 0)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, c.refusal(resp)
	}

	read, err := io.ReadAll(resp.Body)
	if err != nil {
		// A body that stops mid-read is a truncated answer, and a truncated
		// report parsed as a whole one is a window silently missing its
		// tail. This network has no paging, so the whole window arrives in
		// one body and a truncation loses an arbitrary amount of it.
		// Retryable: the next attempt asks for the same window and either
		// gets it whole or fails again.
		return nil, networks.NewRetryableError(
			fmt.Errorf("%w: reading Linkwise's answer: %w", networks.ErrNetworkUnavailable, err), 0)
	}
	return read, nil
}

// refusal turns a non-200 into the port's vocabulary, marked retryable or
// terminal so RetryBackoff.Do knows whether asking again could help, and
// carrying whatever Linkwise said about it.
//
// The classification is networks.RetryableHTTPStatus's and not this
// package's, for the reason the Awin client gives: that set already excludes
// 501, a statement about the endpoint that will be just as true in thirty
// seconds, and 499, a front end recording that WE hung up.
func (c *Client) refusal(resp *http.Response) error {
	retryAfter, askErr := networks.RetryAfterFromHeader(resp.Header)
	if askErr != nil {
		// Unreadable rather than absent. The ask is dropped - the adapter's
		// own backoff still applies - but it is said out loud rather than
		// silently read as "no wait asked for".
		retryAfter = 0
	}
	said := c.saidAbout(resp)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Terminal, explicitly. Re-sending a rejected credential cannot
		// start working, and a poller hammering a 401 with HTTP Basic looks
		// to Linkwise exactly like somebody guessing the password.
		return networks.NewTerminalError(fmt.Errorf(
			"%w: Linkwise answered %d for publisher %s%s",
			networks.ErrNetworkRefused, resp.StatusCode, c.account.ExternalID(), said))
	case resp.StatusCode == http.StatusTooManyRequests:
		// Not observed. Twelve requests back to back at about 1.5 a second
		// were all answered 200 and no response carried a rate-limit header
		// of any kind, so this network's real ceiling is unknown - which is
		// exactly why the case is here rather than left out as unreachable.
		return networks.NewRetryableError(fmt.Errorf(
			"%w: Linkwise answered %d%s%s",
			networks.ErrNetworkRateLimited, resp.StatusCode, askNote(retryAfter, askErr), said), retryAfter)
	case networks.RetryableHTTPStatus(resp.StatusCode):
		return networks.NewRetryableError(fmt.Errorf(
			"%w: Linkwise answered %d%s%s",
			networks.ErrNetworkUnavailable, resp.StatusCode, askNote(retryAfter, askErr), said), retryAfter)
	default:
		// Every other 4xx is our request being wrong - a missing fields
		// parameter, a window rendered month-first - and no amount of
		// retrying fixes it. Reported as unavailable rather than refused so
		// an operator is not sent to Linkwise support over a query we built
		// badly, and carrying what the network said, because on this network
		// that sentence is the documentation.
		return networks.NewTerminalError(fmt.Errorf(
			"%w: Linkwise answered %d%s", networks.ErrNetworkUnavailable, resp.StatusCode, said))
	}
}

// saidAbout renders Linkwise's own account of a refusal, and is why
// format=json is sent on requests this adapter expects to fail.
//
// Recorded (testdata/error-400.json): a refusal is an object carrying an
// error with a code, a name and a description, and the description is the
// endpoint's entire usage text - the only documentation this API publishes.
// Only its first sentence is quoted here; a whole page in an error message
// is a page nobody reads.
//
// It returns the empty string rather than an error of its own when the body
// is not that shape. A refusal that could not be explained is still a
// refusal, and losing the status code because the explanation was
// unparseable would be the worse trade.
func (c *Client) saidAbout(resp *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Error struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	switch {
	case body.Error.Name != "" && body.Error.Description != "":
		return fmt.Sprintf(" (%s: %s)", body.Error.Name, firstLine(body.Error.Description))
	case body.Error.Name != "":
		return " (" + body.Error.Name + ")"
	case body.Error.Description != "":
		return " (" + firstLine(body.Error.Description) + ")"
	default:
		return ""
	}
}

// firstLine takes the sentence at the top of Linkwise's error description
// and leaves the usage text behind it.
func firstLine(s string) string {
	if cut, _, found := strings.Cut(s, "\n"); found {
		s = cut
	}
	return strings.TrimSpace(s)
}

// askNote renders what the network said about coming back, including the
// case where it said something unreadable.
func askNote(retryAfter time.Duration, askErr error) string {
	switch {
	case askErr != nil:
		return " (with an unreadable Retry-After, so only our own backoff applies)"
	case retryAfter > 0:
		return " (Retry-After " + retryAfter.String() + ")"
	default:
		return ""
	}
}

// redactedTransportError renders a transport failure without the URL it was
// attempting.
func redactedTransportError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + urlErr.Err.Error()
	}
	return err.Error()
}

// windowQuery renders a [networks.QueryWindow] into the four parameters
// Linkwise's custom length takes, in UTC and DAY-FIRST.
//
// The upper bound is the subtlety. A QueryWindow is HALF-OPEN - From <= t <
// To - which is what lets adjacent windows partition a backfill with nothing
// counted twice and nothing lost in a seam (FR-031). Linkwise's to/to_time
// is INCLUSIVE: it answers with everything up to and including that instant.
// So the bound is moved back by the smallest unit the API can express, a
// second, and the two seams meet exactly.
//
// A window narrower than one second cannot be expressed at all, and is
// refused rather than widened: silently asking for more than the caller
// asked for is how a transaction gets ingested twice.
func windowQuery(window networks.QueryWindow) (url.Values, error) {
	if err := window.Validate(); err != nil {
		return nil, err
	}
	from := window.From.UTC()
	// The last instant Linkwise will be asked about, which is the last
	// second strictly inside the half-open window.
	last := window.To.UTC().Add(-time.Second)
	if last.Before(from) {
		return nil, fmt.Errorf(
			"%w: %s is narrower than the one second Linkwise's to_time can express, so it cannot be asked for without widening it",
			networks.ErrInvalidQueryWindow, window)
	}
	return url.Values{
		"length":    {"custom"},
		"from":      {from.Format(linkwiseDateLayout)},
		"from_time": {from.Format(linkwiseTimeLayout)},
		"to":        {last.Format(linkwiseDateLayout)},
		"to_time":   {last.Format(linkwiseTimeLayout)},
	}, nil
}
