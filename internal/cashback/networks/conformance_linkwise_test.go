package networks_test

// The Linkwise adapter's entry in the shared conformance table (T248).
//
// It is driven through a local TLS server answering the recordings in
// linkwise/testdata, and that IS the knob. The fixture is made unwell by
// options on its constructor because it has no transport to lie to; a real
// adapter has one, so "a network that is rate limited" and "a network that
// reports a word nobody mapped" are both just a different response to the
// same request. Nothing test-only was added to the adapter to make this
// suite run, which is the point: what is under test is the code a deployment
// runs.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
)

// conformLinkwiseRecording reads one of the adapter's own recordings.
//
// The path reaches into the adapter's package directory, which is unusual and
// deliberate: a copy here would be a second recording to keep in step with the
// first, and the day they disagreed this suite would be asserting the
// contract against bytes no network ever sent.
func conformLinkwiseRecording(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("linkwise", "testdata", name))
	if err != nil {
		t.Fatalf("reading the Linkwise recording %s: %v", name, err)
	}
	return raw
}

// conformLinkwiseServing stands up a TLS server whose transaction endpoint
// answers transactions and whose programme endpoint answers the recorded
// programme list, and builds a client pointed at it.
//
// The programme list has to be served whatever the scenario, because a
// transaction carries no currency of its own and the adapter joins one from
// there. A scenario that made only the transaction endpoint answer would see
// every read fail for a reason it did not ask for.
func conformLinkwiseServing(t *testing.T, transactions http.HandlerFunc) networks.Network {
	t.Helper()
	programmes := conformLinkwiseRecording(t, "programs.json")

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "programs.html") {
			_, _ = w.Write(programmes)
			return
		}
		transactions(w, r)
	}))
	t.Cleanup(server.Close)

	account, err := networks.NewPublisherAccount(uuid.New(), linkwise.ID, "conformance-publisher")
	if err != nil {
		t.Fatalf("building Linkwise's publisher account: %v", err)
	}
	client, err := linkwise.New(account,
		// Obviously not a real credential, and it never leaves the test
		// server: the point is that the adapter is built the way its
		// composition root builds it, credential and all.
		linkwise.WithCredential("conformance-user", "conformance-password"),
		linkwise.WithBaseURL(server.URL),
		linkwise.WithHTTPClient(server.Client()),
		// A rate nothing waits on. Pacing is asserted by the suite's own
		// limiter against the adapter's DECLARED rate, not by this one.
		linkwise.WithRateLimitPerMinute(60_000))
	if err != nil {
		t.Fatalf("linkwise.New(): %v", err)
	}
	return client
}

// conformLinkwise is the Linkwise adapter's entry. Every reference to the
// package lives in this file, which is what keeps the scenarios free of them.
func conformLinkwise() conformAdapter {
	return conformAdapter{
		name: string(linkwise.ID),
		open: func(t *testing.T) networks.Network {
			t.Helper()
			body := conformLinkwiseRecording(t, "transactions.json")
			return conformLinkwiseServing(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body)
			})
		},
		// The week around the first recorded transaction, 2024-06-07. Seven
		// days, which is exactly what this adapter declares as its maximum -
		// so the suite's "a window exactly as wide as the documented
		// maximum" case is this one.
		window: func(t *testing.T, _ networks.Network) networks.QueryWindow {
			t.Helper()
			return networks.QueryWindow{
				From: time.Date(2024, time.June, 3, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2024, time.June, 10, 0, 0, 0, 0, time.UTC),
			}
		},
		deeplink: func(t *testing.T, _ networks.Network) networks.DeeplinkTarget {
			t.Helper()
			return networks.DeeplinkTarget{
				OfferID:   uuid.New(),
				NetworkID: linkwise.ID,
				// One of the five sub-id slots, which is what this network
				// reads a reference from.
				ClickRefParam: "subid1",
				Template:      "https://go.linkwi.se/z/5-12/CD00/?lnkurl=https%3A%2F%2Fshop.example%2Fx",
			}
		},
		// A window whose second transaction carries a status word nobody
		// mapped. Second rather than first on purpose: the scenario asserts
		// that reports which arrived BEFORE the surprise are ordinary and
		// stay, and a bad first row would prove nothing about that.
		openUnmappable: func(t *testing.T) networks.Network {
			t.Helper()
			return conformLinkwiseServing(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[` +
					`{"id":1,"program":{"id":5},"amount":"19.51","commission":"2.93",` +
					`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Validated","date":"2024-06-08T00:00:00+03:00"}},` +
					`{"id":2,"program":{"id":5},"amount":"10.00","commission":"1.00",` +
					`"date":"2024-06-08T09:00:00+03:00","status":{"name":"Escalated","date":"2024-06-09T00:00:00+03:00"}}` +
					`]`))
			})
		},
		// A network that answers the status the requested sentinel is
		// classified from. This is where a real adapter's transport earns
		// its keep: the suite asks for "a network that is rate limited" and
		// gets one, without the adapter knowing it is under test.
		openReporting: func(t *testing.T, report error) networks.Network {
			t.Helper()
			status := conformLinkwiseStatusFor(t, report)
			return conformLinkwiseServing(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})
		},
	}
}

// conformLinkwiseStatusFor translates one of the port's classification
// sentinels into the HTTP status Linkwise would answer with.
//
// The mapping is here rather than in a scenario because a scenario asks for
// "a network that refuses us" and must not know how any particular network
// says so. It is the inverse of the client's own refusal(), and writing it
// out separately is deliberate: a shared table would make the two agree by
// construction rather than by test.
func conformLinkwiseStatusFor(t *testing.T, report error) int {
	t.Helper()
	switch {
	case errors.Is(report, networks.ErrNetworkUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(report, networks.ErrNetworkRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(report, networks.ErrNetworkRefused):
		return http.StatusUnauthorized
	default:
		t.Fatalf("the suite asked Linkwise to report %v, which is not a classification sentinel", report)
		return 0
	}
}
