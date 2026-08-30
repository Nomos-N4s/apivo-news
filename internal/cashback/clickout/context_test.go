package clickout_test

// What a click records about where it came from, and what it must never
// record (FR-022).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
)

// TestTheContextDigestComesFromWhereTheDeploymentSaysItDoes is FR-022's
// half that only the handler can decide: what is digested, and from where.
func TestTheContextDigestComesFromWhereTheDeploymentSaysItDoes(t *testing.T) {
	t.Parallel()

	const agent = "Mozilla/5.0 (a device someone owns)"

	cases := []struct {
		name    string
		options []clickout.HandlerOption
		header  string
		// wantSame is a request whose digest must match this one's, and
		// wantDifferent one whose must not.
		otherHeader string
		otherSame   bool
	}{
		{
			// No header configured: the digest is the connection's own
			// peer, so a forwarded header a client set changes nothing.
			name:        "no header is configured",
			header:      "198.51.100.9",
			otherHeader: "203.0.113.7",
			otherSame:   true,
		},
		{
			name:        "the configured header carries the client",
			options:     []clickout.HandlerOption{clickout.WithContextHeader("X-Client-IP")},
			header:      "198.51.100.9",
			otherHeader: "203.0.113.7",
		},
		{
			// A chain appends, so the leftmost is the client the edge saw.
			// Taking the whole chain would change the digest whenever a hop
			// was added.
			name:        "only the first value of a chain is taken",
			options:     []clickout.HandlerOption{clickout.WithContextHeader("X-Client-IP")},
			header:      "198.51.100.9",
			otherHeader: "198.51.100.9, 10.0.0.1",
			otherSame:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			digest := func(header string) string {
				offer := anOffer()
				clicks := &fakeStore{echo: true}
				h := clickout.NewHandler(discardLogger(),
					issuer(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://x.test/go"}),
					stubMemberAuth{member: aMember}, tc.options...)

				req := httptest.NewRequest(http.MethodPost, clickout.Prefix,
					strings.NewReader(`{"offer_id":"`+offer.ID.String()+`"}`))
				req.Header.Set("Authorization", "Bearer t")
				req.Header.Set("X-Client-IP", header)
				req.Header.Set("User-Agent", agent)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
				}
				if !clicks.inserted.ContextDigest.Valid {
					t.Fatal("the click recorded no context digest")
				}
				// Whatever went in, the raw parts never did.
				for _, raw := range []string{header, agent} {
					if strings.Contains(clicks.inserted.ContextDigest.String, raw) {
						t.Fatalf("the stored digest carries %q verbatim", raw)
					}
				}
				return clicks.inserted.ContextDigest.String
			}

			first, other := digest(tc.header), digest(tc.otherHeader)
			if (first == other) != tc.otherSame {
				t.Errorf("digests %q and %q: same = %v, want %v", first, other, first == other, tc.otherSame)
			}
		})
	}
}

// TestTheUserAgentIsPartOfTheContext keeps two devices behind one address
// from sharing a bucket, which is the ordinary shape of a household.
func TestTheUserAgentIsPartOfTheContext(t *testing.T) {
	t.Parallel()

	digest := func(agent string) string {
		offer := anOffer()
		clicks := &fakeStore{echo: true}
		h := clickout.NewHandler(discardLogger(),
			issuer(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://x.test/go"}),
			stubMemberAuth{member: aMember})

		req := httptest.NewRequest(http.MethodPost, clickout.Prefix,
			strings.NewReader(`{"offer_id":"`+offer.ID.String()+`"}`))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("User-Agent", agent)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
		}
		return clicks.inserted.ContextDigest.String
	}

	if digest("a-phone/1.0") == digest("a-laptop/2.0") {
		t.Error("two user agents on one address digested to the same context")
	}
}
