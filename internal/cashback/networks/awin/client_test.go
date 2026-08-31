package awin_test

// What the transport does with Awin's answers, and what it does with the
// credential (T137).
//
// The credential cases are the ones worth writing first. Every other
// property here fails loudly the first time a poll runs; a token that leaks
// into a URL or an error message fails silently, and is discovered in
// somebody's log aggregator.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/awin"
)

// theToken is the credential every case hands the client. It is
// deliberately a distinctive string so a test can search a URL, an error or
// a log line for it and know a hit is this and nothing else.
const theToken = "tok-8b1f4c0e-never-in-a-url"

// anAccount is a publisher account at Awin.
func anAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), awin.ID, "123456")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

// serving stands up a TLS server answering with handler, and a client
// pointed at it. TLS because the client refuses a base URL that is not
// https - a token over http is a token on the wire.
func serving(t *testing.T, handler http.HandlerFunc, opts ...awin.Option) (*awin.Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	opts = append([]awin.Option{
		awin.WithToken(theToken),
		awin.WithBaseURL(server.URL),
		awin.WithHTTPClient(server.Client()),
		// A rate nothing waits on, and a budget short enough that a case
		// asserting exhaustion does not sit here for a minute.
		awin.WithRateLimitPerMinute(60_000),
		awin.WithRetryPolicy(networks.RetryBackoffPolicy{
			BaseDelay:   time.Millisecond,
			DelayCap:    2 * time.Millisecond,
			MaxAttempts: 3,
			MaxElapsed:  2 * time.Second,
		}),
	}, opts...)

	client, err := awin.New(anAccount(t), opts...)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client, server
}

func TestTheTokenTravelsInTheHeader(t *testing.T) {
	t.Parallel()
	var got string
	client, _ := serving(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got != "Bearer "+theToken {
		t.Errorf("Authorization = %q, want the bearer token", got)
	}
}

// Awin also accepts the token as an `accessToken` QUERY parameter, and that
// is the form this client must never use: a URL is written into access
// logs, proxy logs and error messages by everything it passes through, so a
// credential in one is a credential in all of them.
func TestTheTokenNeverReachesTheURL(t *testing.T) {
	t.Parallel()
	var seen string
	client, _ := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	query := url.Values{"startDate": {"2026-01-01T00:00:00"}, "endDate": {"2026-01-31T23:59:59"}}
	if _, err := client.Get(t.Context(), "/publishers/123456/transactions/", query); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if strings.Contains(seen, theToken) {
		t.Errorf("the request URL carries the token: %s", seen)
	}
	if !strings.Contains(seen, "startDate=") {
		t.Errorf("the query was not sent: %s", seen)
	}
}

// An error is the other place a credential escapes: it is logged, wrapped,
// and often shown. Every failure path is checked, because it only takes one.
func TestNoFailurePathPutsTheTokenInItsError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"refused", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
		{"rate limited", http.StatusTooManyRequests},
		{"server error", http.StatusInternalServerError},
		{"bad request", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
			if err == nil {
				t.Fatalf("status %d produced no error", tc.status)
			}
			if strings.Contains(err.Error(), theToken) {
				t.Errorf("the error carries the token: %v", err)
			}
		})
	}
}

// A transport failure names a URL through url.Error, which is where an
// operator's mistyped base URL - possibly with a credential pasted into it -
// would surface.
func TestATransportFailureDoesNotEchoTheURL(t *testing.T) {
	t.Parallel()
	client, server := serving(t, func(http.ResponseWriter, *http.Request) {})
	server.Close() // nothing is listening now

	_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
	if !errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Fatalf("Get() returned %v, want networks.ErrNetworkUnavailable", err)
	}
	if strings.Contains(err.Error(), server.URL) {
		t.Errorf("the error echoes the URL it was attempting: %v", err)
	}
}

func TestAwinsAnswersMapOntoThePortsVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"a rejected credential is refused", http.StatusUnauthorized, networks.ErrNetworkRefused},
		{"a forbidden account is refused", http.StatusForbidden, networks.ErrNetworkRefused},
		{"a throttled request is rate limited", http.StatusTooManyRequests, networks.ErrNetworkRateLimited},
		{"a server error is unavailable", http.StatusInternalServerError, networks.ErrNetworkUnavailable},
		{"a bad gateway is unavailable", http.StatusBadGateway, networks.ErrNetworkUnavailable},
		// Not "refused": our request was wrong, and telling an operator
		// their publisher account was rejected would send them to Awin
		// support over a malformed date window.
		{"a bad request is unavailable, not refused", http.StatusBadRequest, networks.ErrNetworkUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d returned %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

// A rejected credential is terminal. Re-sending it cannot start working,
// and a poller hammering a 401 looks to Awin exactly like an attack on the
// account.
func TestARejectedCredentialIsNotRetried(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})

	if _, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil); !errors.Is(err, networks.ErrNetworkRefused) {
		t.Fatalf("Get() returned %v, want networks.ErrNetworkRefused", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("the credential was sent %d times, want once", n)
	}
}

// A server error is worth another try, and the retry is what makes a poll
// survive a blip rather than losing the window.
func TestAServerErrorIsRetriedAndCanSucceed(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	body, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want the second attempt's answer", body)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("made %d attempts, want 2", n)
	}
}

// The retry budget is a ceiling, not a suggestion: a network that is down
// must not hold a poll open indefinitely.
func TestAPersistentFailureGivesUpWithinTheBudget(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
	if err == nil {
		t.Fatal("a permanently failing network produced no error")
	}
	// The last failure travels with the verdict, so an operator reading
	// "gave up" still learns what was refusing us.
	if !errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Errorf("the give-up error lost the cause: %v", err)
	}
	if n := attempts.Load(); n != 3 {
		t.Errorf("made %d attempts, want the policy's 3", n)
	}
}

// The conversion from Awin's published per-minute figure into the
// per-second rate the limiter takes is the one place a unit bug can hide -
// and RateLimiter.Rate() exists so it can be read back rather than trusted.
func TestThePerMinuteRateBecomesThePerSecondOneItMeans(t *testing.T) {
	t.Parallel()
	for perMinute, wantPerSecond := range map[int]float64{
		20:  20.0 / 60.0, // Awin's documented limit
		60:  1,
		360: 6,
	} {
		client, err := awin.New(anAccount(t),
			awin.WithToken(theToken),
			awin.WithRateLimitPerMinute(perMinute),
		)
		if err != nil {
			t.Fatalf("New() with %d/min: %v", perMinute, err)
		}
		if got := client.RateLimit(); got != wantPerSecond {
			t.Errorf("%d a minute became %v a second, want %v", perMinute, got, wantPerSecond)
		}
	}
}

// The default is what a deployment gets when nobody has said otherwise, and
// it must be Awin's published figure rather than something convenient.
func TestTheDefaultRateIsTheOneAwinPublishes(t *testing.T) {
	t.Parallel()
	client, err := awin.New(anAccount(t), awin.WithToken(theToken))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if want := 20.0 / 60.0; client.RateLimit() != want {
		t.Errorf("the default rate is %v a second, want Awin's 20 a minute (%v)", client.RateLimit(), want)
	}
}

func TestAClientRefusesWhatItCannotWorkWith(t *testing.T) {
	t.Parallel()
	account := anAccount(t)
	for _, tc := range []struct {
		name string
		opts []awin.Option
		want error
	}{
		{"no token at all", nil, awin.ErrNoToken},
		{"a blank token", []awin.Option{awin.WithToken("   ")}, awin.ErrNoToken},
		{
			"an http base URL, which would put the token on the wire",
			[]awin.Option{awin.WithToken(theToken), awin.WithBaseURL("http://api.awin.com")},
			awin.ErrNotConfigured,
		},
		{
			"a base URL naming no host",
			[]awin.Option{awin.WithToken(theToken), awin.WithBaseURL("https://")},
			awin.ErrNotConfigured,
		},
		{
			"a rate that paces nothing",
			[]awin.Option{awin.WithToken(theToken), awin.WithRateLimitPerMinute(0)},
			awin.ErrNotConfigured,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := awin.New(account, tc.opts...); !errors.Is(err, tc.want) {
				t.Fatalf("New() returned %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAClientNeedsThePublisherAccountItPollsFor(t *testing.T) {
	t.Parallel()
	if _, err := awin.New(networks.PublisherAccount{}, awin.WithToken(theToken)); !errors.Is(err, awin.ErrNoPublisherAccount) {
		t.Fatalf("New() with no account returned %v, want awin.ErrNoPublisherAccount", err)
	}
}

// A cancelled context stops the sequence rather than working through the
// retry budget, so a shutting-down poller stops promptly.
func TestACancelledContextStopsTheRequest(t *testing.T) {
	t.Parallel()
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Get(ctx, "/publishers/123456/transactions/", nil); err == nil {
		t.Fatal("a cancelled context produced no error")
	}
}

// countingClock advances instantly and records how long it was asked to
// wait, so pacing can be asserted without the suite actually sleeping.
type countingClock struct {
	mu    sync.Mutex
	now   time.Time
	slept time.Duration
}

func (c *countingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *countingClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		c.mu.Lock()
		c.now = c.now.Add(d)
		c.slept += d
		c.mu.Unlock()
	}
	return nil
}

// The limiter goes INSIDE the retry, which RetryBackoff.Do's own
// documentation asks for: "a limiter placed outside it would spend one
// token on the whole retry sequence".
//
// That is not a style preference. A retry storm is exactly when a network
// is least able to take one, and an adapter that paced only its first
// attempt would answer a 500 by firing its whole retry budget at the
// network as fast as the connection allowed - the behaviour rate limits
// exist to prevent, arriving precisely when it does most harm.
//
// One token a minute makes the two arrangements unmistakable: paced inside,
// the second and third attempts each wait a minute; paced outside, the
// bucket is full for the one token ever taken and nothing waits at all.
func TestTheLimiterPacesEveryAttemptAndNotJustTheSequence(t *testing.T) {
	t.Parallel()
	clock := &countingClock{now: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}

	var attempts atomic.Int64
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	},
		awin.WithRateLimitPerMinute(1),
		awin.WithClock(clock),
		awin.WithRetryPolicy(networks.RetryBackoffPolicy{
			BaseDelay:   time.Millisecond,
			DelayCap:    time.Millisecond,
			MaxAttempts: 3,
			MaxElapsed:  time.Hour,
		}),
	)

	if _, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil); err == nil {
		t.Fatal("a permanently failing network produced no error")
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("made %d attempts, want 3", n)
	}

	clock.mu.Lock()
	slept := clock.slept
	clock.mu.Unlock()

	// The two arrangements are minutes apart, so the threshold sits between
	// them rather than on either: paced per attempt this waits about two
	// minutes, paced per sequence it waits the retry policy's couple of
	// milliseconds and nothing more.
	//
	// Deliberately not asserted at exactly two minutes. The limiter's token
	// accounting is floating point, and one token a minute lands on
	// 1m59.999999999s - a boundary that says nothing about the property and
	// fails the test for a rounding step.
	if slept < time.Minute {
		t.Errorf("the sequence waited %v in total; paced per attempt at one token a minute it would wait minutes, so the limiter is pacing the sequence rather than each attempt", slept)
	}
}

// Account is what the poller keys its registry by and what every evidence
// row this adapter produces carries, so a client that answered with a
// different account than it was built for would file one account's
// transactions under another's.
func TestTheClientAnswersWithTheAccountItWasBuiltFor(t *testing.T) {
	t.Parallel()
	account := anAccount(t)

	client, err := awin.New(account, awin.WithToken(theToken))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if got := client.Account(); got.ID() != account.ID() || got.ExternalID() != account.ExternalID() {
		t.Errorf("Account() = %s/%s, want %s/%s",
			got.ID(), got.ExternalID(), account.ID(), account.ExternalID())
	}
	if got := client.Account().Network(); got != awin.ID {
		t.Errorf("the account is at %q, want %q", got, awin.ID)
	}
}

// The burst default is one, deliberately: a limit of twenty a minute is not
// an invitation to spend twenty at once, and a poller walking windows has no
// reason to. A deployment Awin has raised a limit for can say otherwise.
func TestTheBurstDefaultsToOneAndCanBeRaised(t *testing.T) {
	t.Parallel()
	clock := &countingClock{now: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)}

	// Two requests at one a minute. With the default burst of one, the
	// bucket holds a single token and the second request waits; with a
	// burst of two, both are taken at once and nothing waits.
	for _, tc := range []struct {
		name     string
		opts     []awin.Option
		wantWait bool
	}{
		{"the default burst of one makes the second request wait", nil, true},
		{"a burst of two lets both go at once", []awin.Option{awin.WithBurst(2)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			paced := &countingClock{now: clock.now}
			opts := append([]awin.Option{
				awin.WithRateLimitPerMinute(1),
				awin.WithClock(paced),
			}, tc.opts...)
			client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}, opts...)

			for range 2 {
				if _, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil); err != nil {
					t.Fatalf("Get(): %v", err)
				}
			}
			paced.mu.Lock()
			slept := paced.slept
			paced.mu.Unlock()

			if tc.wantWait && slept == 0 {
				t.Error("the second request did not wait, so the burst is larger than one")
			}
			if !tc.wantWait && slept != 0 {
				t.Errorf("the requests waited %v although the burst allows both", slept)
			}
		})
	}
}

// A CDN or WAF in front of Awin can answer a 429 with the HTTP-date form of
// Retry-After, which networks.RetryAfterFromHeader deliberately does not
// parse: reading it would mean trusting their clock against ours to decide
// how long to stay away.
//
// The ask is dropped and our own backoff carries the wait, but the refusal
// SAYS the header was unreadable rather than reading it as "no wait asked
// for". The difference between backing off and being banned lives in that
// distinction, and an operator seeing a ban needs the log line that
// explains it.
func TestAnUnreadableRetryAfterIsReportedRatherThanReadAsSilence(t *testing.T) {
	t.Parallel()
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		// The HTTP-date form: legal, and not delta-seconds.
		w.Header().Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
	if !errors.Is(err, networks.ErrNetworkRateLimited) {
		t.Fatalf("Get() returned %v, want networks.ErrNetworkRateLimited", err)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("the refusal does not say the Retry-After could not be read: %v", err)
	}
}

// A readable ask is carried into the refusal, so a log line says what the
// network actually asked for.
func TestAReadableRetryAfterIsCarriedIntoTheRefusal(t *testing.T) {
	t.Parallel()
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.Get(t.Context(), "/publishers/123456/transactions/", nil)
	if !errors.Is(err, networks.ErrNetworkRateLimited) {
		t.Fatalf("Get() returned %v, want networks.ErrNetworkRateLimited", err)
	}
	if !strings.Contains(err.Error(), "Retry-After 7s") {
		t.Errorf("the refusal does not carry the ask: %v", err)
	}
}

// TestAPublisherIDThatCouldReshapeAPathIsRefused covers the third piece of
// configuration that ends up in a URL. The port checks only that the id is
// not blank, because it is network-agnostic and cannot know Awin types this
// as an integer; url.URL.JoinPath escapes a space but not a slash, so an id
// carrying one would send an authenticated request to a path this adapter
// never meant to call and the answer would be read as a catalogue.
func TestAPublisherIDThatCouldReshapeAPathIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		externalID string
		wantErr    error
	}{
		{name: "the digits a real account carries", externalID: "123456"},
		{name: "a slash that would add path segments", externalID: "123/456", wantErr: awin.ErrNotConfigured},
		{name: "a traversal", externalID: "..", wantErr: awin.ErrNotConfigured},
		{name: "a query that would append parameters", externalID: "123?accessToken=x", wantErr: awin.ErrNotConfigured},
		{name: "space around an otherwise good id", externalID: " 123 ", wantErr: awin.ErrNotConfigured},
		{name: "an id that is not a number at all", externalID: "acme-publisher", wantErr: awin.ErrNotConfigured},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			account, err := networks.NewPublisherAccount(uuid.New(), awin.ID, tt.externalID)
			if err != nil {
				t.Fatalf("NewPublisherAccount(%q): %v", tt.externalID, err)
			}

			client, err := awin.New(account, awin.WithToken(theToken))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("New(): %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want one wrapping %v", err, tt.wantErr)
			}
			if client != nil {
				t.Errorf("New() returned a client beside a refusal")
			}
		})
	}
}
