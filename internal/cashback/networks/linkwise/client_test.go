package linkwise_test

// What the transport does with Linkwise's answers, and what it does with the
// credential (T243).
//
// The credential cases come first, for the reason the Awin suite gives for
// its own: every other property here fails loudly the first time a poll runs;
// a password that leaks into a URL or an error message fails silently, and is
// discovered in somebody's log aggregator. It matters more on this network
// than on Awin's - the credential is HTTP Basic, so it is the account's
// actual password rather than a revocable token, and Linkwise's own
// integrators put it in the URL.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
)

// The credential every case hands the client. Both halves are deliberately
// distinctive strings, so a test can search a URL, an error or a log line for
// one and know a hit is this and nothing else.
const (
	theUsername = "CD-user-never-in-a-url"
	thePassword = "pw-4f19ba7c-never-in-a-url"
)

// theCurrency is what this account's report is declared to be denominated
// in. The report itself carries no currency field, so every client is built
// with one.
const theCurrency = "EUR"

// anAccount is a publisher account at Linkwise.
func anAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), linkwise.ID, "CD20")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

// serving stands up a TLS server answering with handler, and a client
// pointed at it. TLS because the client refuses a base URL that is not
// https - Basic auth over http is the password on the wire.
func serving(t *testing.T, handler http.HandlerFunc, opts ...linkwise.Option) *linkwise.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	opts = append([]linkwise.Option{
		linkwise.WithCredential(theUsername, thePassword),
		linkwise.WithReportCurrency(theCurrency),
		linkwise.WithBaseURL(server.URL),
		linkwise.WithHTTPClient(server.Client()),
		// A rate nothing waits on, and a budget short enough that a case
		// asserting exhaustion does not sit here for a minute.
		linkwise.WithRateLimitPerMinute(60_000),
		linkwise.WithRetryPolicy(networks.RetryBackoffPolicy{
			BaseDelay:   time.Millisecond,
			DelayCap:    2 * time.Millisecond,
			MaxAttempts: 3,
			MaxElapsed:  2 * time.Second,
		}),
	}, opts...)

	client, err := linkwise.New(anAccount(t), opts...)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client
}

// answering is the common case: a handler that records the request it was
// given and answers an empty report.
func answering(t *testing.T, seen *http.Request) *linkwise.Client {
	t.Helper()
	return serving(t, func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})
}

func TestTheCredentialTravelsAsBasicAuthInTheHeader(t *testing.T) {
	t.Parallel()
	var seen http.Request
	client := answering(t, &seen)

	if _, err := client.Get(t.Context(), "reports_transaction.html", nil); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	user, pass, ok := seen.BasicAuth()
	if !ok {
		t.Fatalf("no Basic credential was sent; Authorization was %q", seen.Header.Get("Authorization"))
	}
	if user != theUsername || pass != thePassword {
		t.Errorf("the credential sent was %q/%q, want %q/%q", user, pass, theUsername, thePassword)
	}
}

// TestTheCredentialIsNowhereInTheURL is the silent-failure case. Linkwise's
// own documented integration puts user:pass in the address, and a URL is
// written to access logs, proxy logs and error messages by everything it
// passes through.
func TestTheCredentialIsNowhereInTheURL(t *testing.T) {
	t.Parallel()
	var seen http.Request
	client := answering(t, &seen)

	if _, err := client.Get(t.Context(), "reports_transaction.html", url.Values{"fields": {"transaction_id"}}); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	whole := seen.URL.String()
	for _, half := range []string{theUsername, thePassword} {
		if strings.Contains(whole, half) {
			t.Errorf("the request URL %q carries a half of the credential", whole)
		}
	}
	if seen.URL.User != nil {
		t.Errorf("the request URL carries userinfo: %v", seen.URL.User)
	}
}

// TestFormatJSONTravelsOnEveryRequest is the recorded finding, held as an
// assertion. Without this parameter the same malformed request answers 400
// with an HTML page rather than a structured error, so an adapter reports a
// parse failure and discards what the network actually said.
func TestFormatJSONTravelsOnEveryRequest(t *testing.T) {
	t.Parallel()
	var seen http.Request
	client := answering(t, &seen)

	// Deliberately with a caller query that says nothing about format.
	if _, err := client.Get(t.Context(), "reports_transaction.html", url.Values{"length": {"today"}}); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got := seen.URL.Query().Get("format"); got != "json" {
		t.Errorf("format = %q, want json; without it a refusal comes back as an HTML page", got)
	}
	if got := seen.URL.Query().Get("timezone"); got != "UTC" {
		t.Errorf("timezone = %q, want UTC; the window is a pair of UTC instants and the network's default is not ours to rely on", got)
	}
	if got := seen.URL.Query().Get("length"); got != "today" {
		t.Errorf("the caller's own length was %q, want today: the global options must be added, not substituted", got)
	}
}

// TestTheCallersQueryIsNotMutated. Get adds two parameters to what it is
// given, and a caller that reused its url.Values for a second window would
// otherwise carry the first one's additions into it.
func TestTheCallersQueryIsNotMutated(t *testing.T) {
	t.Parallel()
	var seen http.Request
	client := answering(t, &seen)

	mine := url.Values{"length": {"today"}}
	if _, err := client.Get(t.Context(), "reports_transaction.html", mine); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if _, ok := mine["format"]; ok {
		t.Error("Get() wrote format into the caller's own values")
	}
	if len(mine) != 1 {
		t.Errorf("the caller's values now carry %d parameters, want the 1 it set", len(mine))
	}
}

// TestTheEndpointSitsUnderThePinnedAPIVersion. The caller names a file, not a
// path: the version prefix is the client's, so a caller cannot reach outside
// the API and the version is pinned in exactly one place.
func TestTheEndpointSitsUnderThePinnedAPIVersion(t *testing.T) {
	t.Parallel()
	var seen http.Request
	client := answering(t, &seen)

	if _, err := client.Get(t.Context(), "reports_transaction.html", nil); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got, want := seen.URL.Path, "/api/1.1/reports_transaction.html"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestTheBodyComesBackWhole(t *testing.T) {
	t.Parallel()
	const report = `[{"id":420343717,"subid1":null}]`
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(report))
	})

	body, err := client.Get(t.Context(), "reports_transaction.html", nil)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if string(body) != report {
		t.Errorf("body = %q, want %q", body, report)
	}
}

// TestARefusalCarriesWhatLinkwiseSaid. On this network the error description
// is the only documentation there is, so a transport that reported the status
// code alone would throw away the sentence that says what the request was
// missing.
func TestARefusalCarriesWhatLinkwiseSaid(t *testing.T) {
	t.Parallel()
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"name":"Bad Request",` +
			`"description":"The request parameters are invalid or missing.\n\nUsage:\n\nfields comma separated list"}}`))
	})

	_, err := client.Get(t.Context(), "reports_transaction.html", nil)
	if err == nil {
		t.Fatal("a 400 was not reported as an error")
	}
	if !strings.Contains(err.Error(), "Bad Request") {
		t.Errorf("the error does not name what Linkwise called it: %v", err)
	}
	if !strings.Contains(err.Error(), "The request parameters are invalid or missing.") {
		t.Errorf("the error does not carry Linkwise's own description: %v", err)
	}
	// The description is four kilobytes of usage text in the real answer.
	// Only its first sentence belongs in an error message.
	if strings.Contains(err.Error(), "comma separated list") {
		t.Errorf("the error carries the whole usage text, not its first line: %v", err)
	}
}

// TestARefusalSurvivesAnUnreadableBody. A refusal that could not be explained
// is still a refusal; losing the status code because the explanation was not
// the expected shape would be the worse trade.
func TestARefusalSurvivesAnUnreadableBody(t *testing.T) {
	t.Parallel()
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// What this endpoint answers when format=json was NOT sent.
		_, _ = w.Write([]byte("<html><body>Usage: ...</body></html>"))
	})

	_, err := client.Get(t.Context(), "reports_transaction.html", nil)
	if !errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrNetworkUnavailable", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

func TestHTTPOutcomesAreClassified(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		status   int
		want     error
		attempts int32
	}{
		// Terminal until somebody changes a credential. Re-sending a
		// rejected Basic credential cannot start working, and a poller
		// hammering a 401 looks like somebody guessing the password.
		{name: "unauthorized", status: http.StatusUnauthorized, want: networks.ErrNetworkRefused, attempts: 1},
		{name: "forbidden", status: http.StatusForbidden, want: networks.ErrNetworkRefused, attempts: 1},
		// Not observed on this network - no response ever carried a
		// rate-limit header - which is why it is covered rather than left
		// out as unreachable.
		{name: "too many requests", status: http.StatusTooManyRequests, want: networks.ErrNetworkRateLimited, attempts: 3},
		{name: "server error", status: http.StatusInternalServerError, want: networks.ErrNetworkUnavailable, attempts: 3},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, want: networks.ErrNetworkUnavailable, attempts: 3},
		// Our request being wrong. No amount of retrying fixes it, and it
		// is reported as unavailable rather than refused so an operator is
		// not sent to Linkwise support over a query we built badly.
		{name: "bad request", status: http.StatusBadRequest, want: networks.ErrNetworkUnavailable, attempts: 1},
		{name: "not found", status: http.StatusNotFound, want: networks.ErrNetworkUnavailable, attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tt.status)
			})

			_, err := client.Get(t.Context(), "reports_transaction.html", nil)
			if !errors.Is(err, tt.want) {
				t.Fatalf("%d gave %v, want it to wrap %v", tt.status, err, tt.want)
			}
			if got := attempts.Load(); got != tt.attempts {
				t.Errorf("%d was attempted %d times, want %d", tt.status, got, tt.attempts)
			}
		})
	}
}

// TestATransportFailureIsRetriedAndThenReportedWithoutTheURL.
func TestATransportFailureIsRetriedAndThenReportedWithoutTheURL(t *testing.T) {
	t.Parallel()
	// A server that is closed before the first request, so every attempt
	// fails to connect.
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base, httpClient := server.URL, server.Client()
	server.Close()

	client, err := linkwise.New(anAccount(t),
		linkwise.WithCredential(theUsername, thePassword),
		linkwise.WithReportCurrency(theCurrency),
		linkwise.WithBaseURL(base),
		linkwise.WithHTTPClient(httpClient),
		linkwise.WithRateLimitPerMinute(60_000),
		linkwise.WithRetryPolicy(networks.RetryBackoffPolicy{
			BaseDelay: time.Millisecond, DelayCap: 2 * time.Millisecond,
			MaxAttempts: 2, MaxElapsed: time.Second,
		}))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	_, err = client.Get(t.Context(), "reports_transaction.html", nil)
	if !errors.Is(err, networks.ErrNetworkUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrNetworkUnavailable", err)
	}
	if strings.Contains(err.Error(), base) {
		t.Errorf("the error repeats the request URL, which an operator's mistyped base URL could carry a credential in: %v", err)
	}
}

func TestNewRefusesWhatCannotBeUsed(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		account func(*testing.T) networks.PublisherAccount
		opts    []linkwise.Option
		want    error
	}{
		{
			name:    "no account",
			account: func(*testing.T) networks.PublisherAccount { return networks.PublisherAccount{} },
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
			},
			want: linkwise.ErrNoPublisherAccount,
		},
		{
			name:    "no credential at all",
			account: anAccount,
			opts:    []linkwise.Option{linkwise.WithReportCurrency(theCurrency)},
			want:    linkwise.ErrNoCredential,
		},
		{
			// The half-credential case, which is the one worth naming: a
			// request with a username and an empty password is well formed
			// and refused, and an adapter would read that as the publisher
			// account having been rejected.
			name:    "a username and no password",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, ""),
				linkwise.WithReportCurrency(theCurrency),
			},
			want: linkwise.ErrNoCredential,
		},
		{
			name:    "a password and no username",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential("", thePassword),
				linkwise.WithReportCurrency(theCurrency),
			},
			want: linkwise.ErrNoCredential,
		},
		{
			// THE REPORT CARRIES NO CURRENCY FIELD - the endpoint's own
			// usage text lists every field it can return and there is no
			// currency among them - so it comes from configuration, and it
			// is required rather than defaulted. A default would be right
			// for a Greek programme and wrong the day one is Romanian, and
			// the failure would be silent: RON stored as EUR.
			name:    "no report currency",
			account: anAccount,
			opts:    []linkwise.Option{linkwise.WithCredential(theUsername, thePassword)},
			want:    linkwise.ErrNoReportCurrency,
		},
		{
			name:    "a report currency that is not a currency code",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency("euro"),
			},
			want: linkwise.ErrNoReportCurrency,
		},
		{
			name:    "http, not https",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
				linkwise.WithBaseURL("http://affiliate.linkwi.se"),
			},
			want: linkwise.ErrNotConfigured,
		},
		{
			name:    "a base URL naming no host",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
				linkwise.WithBaseURL("https:///api"),
			},
			want: linkwise.ErrNotConfigured,
		},
		{
			name:    "a base URL that will not parse",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
				linkwise.WithBaseURL("https://%zz"),
			},
			want: linkwise.ErrNotConfigured,
		},
		{
			name:    "a rate that paces nothing",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
				linkwise.WithRateLimitPerMinute(0),
			},
			want: linkwise.ErrNotConfigured,
		},
		{
			name:    "a retry policy that contradicts itself",
			account: anAccount,
			opts: []linkwise.Option{
				linkwise.WithCredential(theUsername, thePassword),
				linkwise.WithReportCurrency(theCurrency),
				linkwise.WithRetryPolicy(networks.RetryBackoffPolicy{MaxAttempts: -1}),
			},
			want: linkwise.ErrNotConfigured,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := linkwise.New(tt.account(t), tt.opts...)
			if !errors.Is(err, tt.want) {
				t.Fatalf("New() = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

// TestNewRefusalsNeverRepeatTheCredential. A base URL is the one piece of
// configuration an operator can paste a credential into, and a refusal that
// echoed it would put the password in whatever reads the startup log.
func TestNewRefusalsNeverRepeatTheCredential(t *testing.T) {
	t.Parallel()

	const pasted = "https://" + theUsername + ":" + thePassword + "@%zz"
	_, err := linkwise.New(anAccount(t),
		linkwise.WithCredential(theUsername, thePassword),
		linkwise.WithReportCurrency(theCurrency),
		linkwise.WithBaseURL(pasted))
	if err == nil {
		t.Fatal("an unparseable base URL was accepted")
	}
	if strings.Contains(err.Error(), thePassword) {
		t.Errorf("the refusal repeats the base URL, credential and all: %v", err)
	}
}

// TestThePasswordIsNotTrimmed. A credential is bytes somebody was issued.
// Trimming one sends a different secret than the operator set, which fails as
// an authentication error and sends them looking at the account rather than
// at the whitespace.
func TestThePasswordIsNotTrimmed(t *testing.T) {
	t.Parallel()
	const padded = "  a password that really does end in spaces  "
	var seen http.Request
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		_, _ = w.Write([]byte(`[]`))
	}, linkwise.WithCredential(theUsername, padded))

	if _, err := client.Get(t.Context(), "reports_transaction.html", nil); err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if _, pass, _ := seen.BasicAuth(); pass != padded {
		t.Errorf("the password sent was %q, want %q byte for byte", pass, padded)
	}
}

// TestTheClientPacesToItsDeclaredRate holds the one unit conversion in the
// package. A per-minute figure divided wrongly builds a limiter at sixty
// requests a second, which is invisible from outside unless the rate actually
// in force can be read back.
func TestTheClientPacesToItsDeclaredRate(t *testing.T) {
	t.Parallel()

	client, err := linkwise.New(anAccount(t),
		linkwise.WithCredential(theUsername, thePassword),
		linkwise.WithReportCurrency(theCurrency))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	want := float64(linkwise.Limits().RequestsPerMinute) / 60
	if got := client.RateLimit(); got != want {
		t.Errorf("RateLimit() = %v requests a second, want %v: the default must be what Limits declares", got, want)
	}
	if got, want := client.Limits(), linkwise.Limits(); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
	if got := client.ID(); got != linkwise.ID {
		t.Errorf("ID() = %q, want %q", got, linkwise.ID)
	}
	if got := client.Account().Network(); got != linkwise.ID {
		t.Errorf("Account() is held at %q, want %q", got, linkwise.ID)
	}
}
