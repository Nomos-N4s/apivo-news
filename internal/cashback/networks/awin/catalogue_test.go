// The tests for catalogue.go: that both memberships are asked for, that a
// route Awin does not plainly call live is paused rather than published, that
// the payload survives byte for byte, and that nothing which stops early ever
// looks to an import like a complete answer.

package awin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/awin"
)

// aLiveProgramme is one element of Awin's programme array, carrying fields
// this adapter reads and fields it does not, so a test can tell the payload
// was kept whole rather than re-encoded from the parts that were decoded.
const aLiveProgramme = `{"id":4471,"name":"Gartenhaus","status":"active","linkStatus":"online",` +
	`"primaryRegion":{"countryCode":"DE","name":"Germany"},"currencyCode":"EUR",` +
	`"clickThroughUrl":"https://www.awin1.com/cread.php?awinmid=4471","primarySector":"Home & Garden"}`

// catalogueServer answers every programmes request with the bodies given, in
// call order, and records the queries it was asked. A body of "" answers 500,
// which is how a case makes the second request fail after the first worked.
type catalogueServer struct {
	mu       sync.Mutex
	bodies   []string
	queries  []string
	requests int
}

func (s *catalogueServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, r.URL.Path+"?"+r.URL.RawQuery)
	body := ""
	if s.requests < len(s.bodies) {
		body = s.bodies[s.requests]
	}
	s.requests++
	if body == "" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (s *catalogueServer) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// cataloguing stands up a server answering with bodies in order, and a client
// pointed at it.
func cataloguing(t *testing.T, bodies ...string) (*awin.Client, *catalogueServer) {
	t.Helper()
	server := &catalogueServer{bodies: bodies}
	client, _ := serving(t, server.handle)
	return client, server
}

// collect drains a catalogue read, returning what it yielded and the first
// error it reported.
func collect(seq func(func(networks.ReportedMerchant, error) bool)) ([]networks.ReportedMerchant, error) {
	var merchants []networks.ReportedMerchant
	for merchant, err := range seq {
		if err != nil {
			return merchants, err
		}
		merchants = append(merchants, merchant)
	}
	return merchants, nil
}

func TestBothMembershipsThatNameARouteAreAskedFor(t *testing.T) {
	t.Parallel()

	client, server := cataloguing(t, "[]", "[]")
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	if _, err := collect(seq); err != nil {
		t.Fatalf("the catalogue read failed: %v", err)
	}

	asked := server.asked()
	if len(asked) != 2 {
		t.Fatalf("the catalogue made %d requests, want 2: %v", len(asked), asked)
	}
	// Suspended must be asked for as well as joined. Asking only for joined
	// would have every suspension read as a retailer who had left the
	// network, and their offers withdrawn instead of paused.
	if !strings.Contains(asked[0], "relationship=joined") {
		t.Errorf("the first request was %q, want the joined programmes", asked[0])
	}
	if !strings.Contains(asked[1], "relationship=suspended") {
		t.Errorf("the second request was %q, want the suspended programmes", asked[1])
	}
	for _, query := range asked {
		if !strings.HasPrefix(query, "/publishers/123456/programmes?") {
			t.Errorf("the catalogue asked %q, want the publisher's own programmes path", query)
		}
	}
}

func TestALiveProgrammeBecomesAClickableRoute(t *testing.T) {
	t.Parallel()

	client, _ := cataloguing(t, "["+aLiveProgramme+"]", "[]")
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	merchants, err := collect(seq)
	if err != nil {
		t.Fatalf("the catalogue read failed: %v", err)
	}
	if len(merchants) != 1 {
		t.Fatalf("the catalogue yielded %d routes, want 1", len(merchants))
	}

	got := merchants[0]
	if got.ExternalID != "4471" {
		t.Errorf("ExternalID = %q, want the advertiser id a transaction reports", got.ExternalID)
	}
	if got.Name != "Gartenhaus" {
		t.Errorf("Name = %q, want %q", got.Name, "Gartenhaus")
	}
	if got.Country != "DE" {
		t.Errorf("Country = %q, want %q", got.Country, "DE")
	}
	if got.Status != networks.MerchantStatusActive {
		t.Errorf("Status = %q, want %q", got.Status, networks.MerchantStatusActive)
	}
	// The membership is the one fact about the route that the payload cannot
	// carry: it is the request's filter and appears in no response field.
	if got.StatusRaw != "joined" {
		t.Errorf("StatusRaw = %q, want the membership the route was returned under", got.StatusRaw)
	}
	// Byte for byte, including the fields this adapter never decodes - which
	// is what a later normalisation fix is re-derived from (FR-012).
	if string(got.RawPayload) != aLiveProgramme {
		t.Errorf("RawPayload = %s,\nwant %s", got.RawPayload, aLiveProgramme)
	}
}

// TestARouteAwinDoesNotCallLiveIsPausedNotPublished is the mapping's whole
// direction. Paused is reversible on the next import; publishing a route that
// cannot pay costs a member who shopped for cashback that never arrives.
func TestARouteAwinDoesNotCallLiveIsPausedNotPublished(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		joined    string
		suspended string
		want      networks.MerchantStatus
		wantRaw   string
	}{
		{
			name:   "a joined, active programme with links online",
			joined: `[{"id":1,"name":"Live","status":"active","linkStatus":"online"}]`,
			want:   networks.MerchantStatusActive, wantRaw: "joined",
		},
		{
			name:   "a joined programme Awin has hidden",
			joined: `[{"id":1,"name":"Hidden","status":"hidden","linkStatus":"online"}]`,
			want:   networks.MerchantStatusPaused, wantRaw: "joined",
		},
		{
			name:   "a joined programme whose prepayment funds have run out",
			joined: `[{"id":1,"name":"Broke","status":"active","linkStatus":"offline"}]`,
			want:   networks.MerchantStatusPaused, wantRaw: "joined",
		},
		{
			name:   "a joined programme carrying a status Awin has not documented",
			joined: `[{"id":1,"name":"Novel","status":"archived","linkStatus":"online"}]`,
			want:   networks.MerchantStatusPaused, wantRaw: "joined",
		},
		{
			name:      "a suspended relationship, however healthy the programme",
			suspended: `[{"id":1,"name":"Suspended","status":"active","linkStatus":"online"}]`,
			want:      networks.MerchantStatusPaused, wantRaw: "suspended",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			joined, suspended := tt.joined, tt.suspended
			if joined == "" {
				joined = "[]"
			}
			if suspended == "" {
				suspended = "[]"
			}
			client, _ := cataloguing(t, joined, suspended)
			seq, err := client.FetchCatalogue(t.Context())
			if err != nil {
				t.Fatalf("FetchCatalogue(): %v", err)
			}
			merchants, err := collect(seq)
			if err != nil {
				t.Fatalf("the catalogue read failed: %v", err)
			}
			if len(merchants) != 1 {
				t.Fatalf("the catalogue yielded %d routes, want 1", len(merchants))
			}
			if merchants[0].Status != tt.want {
				t.Errorf("Status = %q, want %q", merchants[0].Status, tt.want)
			}
			if merchants[0].StatusRaw != tt.wantRaw {
				t.Errorf("StatusRaw = %q, want %q", merchants[0].StatusRaw, tt.wantRaw)
			}
		})
	}
}

// TestNoStopShortOfTheWholeAnswerLooksComplete is contract rule 8, which
// matters more here than anywhere else: an import reads absence as departure,
// so a truncated read that ended quietly would withdraw every route it never
// saw and empty the catalogue members look at.
func TestNoStopShortOfTheWholeAnswerLooksComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		bodies []string
		// wantAlso is the classification the abandonment must carry beside
		// the marker, so an operator knows whether to look at Awin or at us.
		wantAlso error
	}{
		{
			name:     "the first request fails",
			bodies:   []string{"", ""},
			wantAlso: networks.ErrNetworkUnavailable,
		},
		{
			name:     "the second request fails after the first was read",
			bodies:   []string{"[" + aLiveProgramme + "]", ""},
			wantAlso: networks.ErrNetworkUnavailable,
		},
		{
			name:     "the body is not the documented array",
			bodies:   []string{`{"programmes":[]}`, "[]"},
			wantAlso: networks.ErrNetworkUnavailable,
		},
		{
			name:     "a programme is not the documented shape",
			bodies:   []string{`[{"id":"four thousand"}]`, "[]"},
			wantAlso: networks.ErrNetworkUnavailable,
		},
		{
			name:     "a programme Awin sent with no name, which nobody could publish",
			bodies:   []string{`[{"id":1,"name":"","status":"active"}]`, "[]"},
			wantAlso: networks.ErrMissingMerchantName,
		},
		{
			name:     "a country that is not the ISO alpha-2 Awin documents",
			bodies:   []string{`[{"id":1,"name":"Odd","status":"active","primaryRegion":{"countryCode":"DEU"}}]`, "[]"},
			wantAlso: networks.ErrInvalidMerchantCountry,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, _ := cataloguing(t, tt.bodies...)
			seq, err := client.FetchCatalogue(t.Context())
			if err != nil {
				t.Fatalf("FetchCatalogue(): %v", err)
			}
			_, err = collect(seq)
			if !errors.Is(err, networks.ErrIterationAbandoned) {
				t.Fatalf("the catalogue read ended with %v, which an import would reconcile against", err)
			}
			if !errors.Is(err, tt.wantAlso) {
				t.Errorf("the abandonment = %v, want one also wrapping %v", err, tt.wantAlso)
			}
		})
	}
}

func TestACancelledCatalogueReadSaysTheAnswerIsPartial(t *testing.T) {
	t.Parallel()

	client, _ := cataloguing(t, "["+aLiveProgramme+"]", "[]")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	seq, err := client.FetchCatalogue(ctx)
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	if _, err := collect(seq); !errors.Is(err, networks.ErrIterationAbandoned) {
		t.Fatalf("a cancelled catalogue read ended with %v, want one wrapping ErrIterationAbandoned", err)
	}
}

// TestARejectedCredentialStopsTheCatalogueTerminally keeps the two halves of
// contract rule 9 apart on this method: the answer is partial AND the reason
// will not clear on its own, so a poller stops rather than looping.
func TestARejectedCredentialStopsTheCatalogueTerminally(t *testing.T) {
	t.Parallel()

	var requests int
	client, _ := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	})

	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	_, err = collect(seq)
	if !errors.Is(err, networks.ErrNetworkRefused) {
		t.Fatalf("a rejected credential ended the catalogue with %v, want one wrapping ErrNetworkRefused", err)
	}
	if !errors.Is(err, networks.ErrIterationAbandoned) {
		t.Errorf("the refusal = %v, which does not say the catalogue is partial", err)
	}
	if requests != 1 {
		t.Errorf("a rejected credential was retried %d times; re-sending it cannot start working", requests-1)
	}
}

// TestACallerThatStopsIsNotToldItsOwnAnswerWasAbandoned separates the two
// ways iteration ends. Rule 8 exists for an ADAPTER that stops early; a
// caller that broke already knows it broke, and telling it otherwise would
// have it discard a page it had just stored.
func TestACallerThatStopsIsNotToldItsOwnAnswerWasAbandoned(t *testing.T) {
	t.Parallel()

	body := `[{"id":1,"name":"First","status":"active"},{"id":2,"name":"Second","status":"active"}]`
	client, server := cataloguing(t, body, "[]")

	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}

	var seen int
	for merchant, err := range seq {
		if err != nil {
			t.Fatalf("the catalogue yielded %v to a caller that was about to stop", err)
		}
		seen++
		if merchant.ExternalID != "1" {
			t.Errorf("the first route was %q, want the first element", merchant.ExternalID)
		}
		break
	}
	if seen != 1 {
		t.Fatalf("the caller saw %d routes before breaking, want 1", seen)
	}
	// And the adapter stopped: the second membership was never asked for.
	if asked := server.asked(); len(asked) != 1 {
		t.Errorf("the adapter made %d requests after the caller broke, want 1: %v", len(asked), asked)
	}
}

// TestSuspendedWinsOverJoined pins the order the two memberships are read in.
// Membership is one value, so a programme in both answers is Awin
// contradicting itself - and the last answer is the one an upsert keeps, so
// the order decides which. Paused is the recoverable half of that choice.
func TestSuspendedWinsOverJoined(t *testing.T) {
	t.Parallel()

	both := `[{"id":7,"name":"Contested","status":"active","linkStatus":"online"}]`
	client, _ := cataloguing(t, both, both)

	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	merchants, err := collect(seq)
	if err != nil {
		t.Fatalf("the catalogue read failed: %v", err)
	}
	if len(merchants) != 2 {
		t.Fatalf("the catalogue yielded %d routes, want both answers rather than a silent drop", len(merchants))
	}
	if merchants[len(merchants)-1].Status != networks.MerchantStatusPaused {
		t.Errorf("the last answer was %q, want the paused one to be what an upsert keeps", merchants[len(merchants)-1].Status)
	}
}

// TestTheTokenNeverReachesACatalogueURL is the credential rule held on this
// method too: the query this adapter adds must be the relationship and
// nothing else.
func TestTheTokenNeverReachesACatalogueURL(t *testing.T) {
	t.Parallel()

	var seen []string
	client, _ := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})

	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	if _, err := collect(seq); err != nil {
		t.Fatalf("the catalogue read failed: %v", err)
	}
	for _, target := range seen {
		if strings.Contains(target, theToken) {
			t.Errorf("the catalogue asked %q, which carries the token", target)
		}
	}
}

// TestAnEmptyCatalogueIsAnAnswerAndNotAFailure: Awin answering with no
// programmes is a publisher account that has joined nothing, which is an
// ordinary state on a new account and must not read as an error.
func TestAnEmptyCatalogueIsAnAnswerAndNotAFailure(t *testing.T) {
	t.Parallel()

	client, _ := cataloguing(t, "[]", "[]")
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	merchants, err := collect(seq)
	if err != nil {
		t.Fatalf("an empty catalogue read failed: %v", err)
	}
	if len(merchants) != 0 {
		t.Errorf("an empty catalogue yielded %d routes", len(merchants))
	}
}

// TestTheRawPayloadIsNotAliasedAcrossRoutes: evidence a later caller can edit
// is not evidence, and two routes decoded from one body must not share bytes.
func TestTheRawPayloadIsNotAliasedAcrossRoutes(t *testing.T) {
	t.Parallel()

	body := `[{"id":1,"name":"First","status":"active"},{"id":2,"name":"Second","status":"active"}]`
	client, _ := cataloguing(t, body, "[]")

	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	merchants, err := collect(seq)
	if err != nil {
		t.Fatalf("the catalogue read failed: %v", err)
	}
	if len(merchants) != 2 {
		t.Fatalf("the catalogue yielded %d routes, want 2", len(merchants))
	}
	for i, merchant := range merchants {
		var decoded struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(merchant.RawPayload, &decoded); err != nil {
			t.Fatalf("route %d carries a payload that will not decode: %v", i, err)
		}
		if want := int64(i + 1); decoded.ID != want {
			t.Errorf("route %d carries the payload of programme %d, want %d", i, decoded.ID, want)
		}
	}
}

// TestWhatAwinPublishesCouldSeedItsRow keeps the declaration honest against
// the table it seeds: every rule Validate applies is a check constraint on
// cashback.network, so a declaration that fails here is a row Postgres would
// have refused at the moment an operator ran the connect command.
func TestWhatAwinPublishesCouldSeedItsRow(t *testing.T) {
	t.Parallel()

	documented := awin.Documented()
	if err := documented.Validate(); err != nil {
		t.Fatalf("what Awin publishes could not seed a network row: %v", err)
	}
	if documented.ID != awin.ID {
		t.Errorf("the declaration names %q, want %q", documented.ID, awin.ID)
	}
	// The click reference goes in clickref and not one of clickref2-6: Awin
	// says those cannot reach the advertiser's landing page.
	if documented.ClickRefParam != "clickref" {
		t.Errorf("ClickRefParam = %q, want the one parameter that reaches the retailer", documented.ClickRefParam)
	}
	if documented.MaxQueryWindowDays != 31 {
		t.Errorf("MaxQueryWindowDays = %d, want the 31 Awin documents", documented.MaxQueryWindowDays)
	}
	if documented.RateLimitPerMinute != awin.DocumentedRateLimitPerMinute {
		t.Errorf("RateLimitPerMinute = %d, want the %d Awin publishes",
			documented.RateLimitPerMinute, awin.DocumentedRateLimitPerMinute)
	}
}
