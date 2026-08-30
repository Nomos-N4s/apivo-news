package clickout_test

// POST /api/v1/cashback/clickouts.
//
// The gate is the first subject: FR-023 says an anonymous click can never be
// credited to an account, and the way this module guarantees that is that
// the handler behind the gate cannot be reached without a member - not that
// it remembers to look for one.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// discardLogger keeps the refusal paths, which log, out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubMemberAuth is the composition root's adapter, canned.
type stubMemberAuth struct {
	member clickout.Member
	err    error
}

func (s stubMemberAuth) AuthenticateMember(context.Context, string) (clickout.Member, error) {
	return s.member, s.err
}

// aMember is an authenticated caller the gate lets through.
var aMember = clickout.Member{ID: uuid.New()}

// served builds a handler over the given parts.
func served(t *testing.T, offers clickout.Offers, clicks clickout.ClickStore, deeplinks clickout.Deeplinks, auth clickout.MemberAuthenticator) http.Handler {
	t.Helper()
	return clickout.NewHandler(discardLogger(), issuer(t, offers, clicks, deeplinks), auth)
}

// post sends one request at the click-out path.
func post(t *testing.T, h http.Handler, body, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, clickout.Prefix, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// problemOf decodes a problem+json body, failing the test if the answer is
// not one - which is itself the assertion that the single error convention
// held.
func problemOf(t *testing.T, rec *httptest.ResponseRecorder) (status int, detail string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json (body %q)", ct, rec.Body.String())
	}
	var body struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answer is not problem+json: %v (body %q)", err, rec.Body.String())
	}
	return body.Status, body.Detail
}

// TestAnAnonymousClickIsNeverEvenAttempted is FR-023 at the boundary. The
// store assertion is the load-bearing half: a 401 that had already written a
// row would leave a click nobody can be credited for.
func TestAnAnonymousClickIsNeverEvenAttempted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		authorization string
		auth          clickout.MemberAuthenticator
	}{
		{name: "no header at all", auth: stubMemberAuth{member: aMember}},
		{name: "the wrong scheme", authorization: "Basic aGk6dGhlcmU=", auth: stubMemberAuth{member: aMember}},
		{name: "bearer with nothing after it", authorization: "Bearer", auth: stubMemberAuth{member: aMember}},
		{name: "a token that resolves to nobody", authorization: "Bearer t", auth: stubMemberAuth{err: clickout.ErrUnauthenticated}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offer := anOffer()
			clicks := &fakeStore{echo: true}
			h := served(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://x.test/go"}, tc.auth)

			rec := post(t, h, `{"offer_id":"`+offer.ID.String()+`"}`, tc.authorization)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
			if clicks.inserts != 0 {
				t.Errorf("%d click(s) were recorded for an unauthenticated request", clicks.inserts)
			}
		})
	}
}

// TestAFailedAuthenticationIsNotAVerdict keeps an unreachable account table
// from reading as "your token is bad", and keeps its reason off the wire.
func TestAFailedAuthenticationIsNotAVerdict(t *testing.T) {
	t.Parallel()

	const secret = "dial tcp 10.0.0.4:5432: connection refused"
	h := served(t, &fakeOffers{}, &fakeStore{}, &fakeDeeplinks{}, stubMemberAuth{err: errors.New(secret)})
	rec := post(t, h, `{"offer_id":"`+uuid.NewString()+`"}`, "Bearer t")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("the answer leaks the failure: %q", rec.Body.String())
	}
}

func TestASuccessfulClickOutAnswersTheTrackedRedirect(t *testing.T) {
	t.Parallel()

	offer := anOffer()
	clicks := &fakeStore{echo: true}
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go?merchant=42&clickref=abc"}
	h := served(t, &fakeOffers{offer: offer}, clicks, deeplinks, stubMemberAuth{member: aMember})

	rec := post(t, h, `{"offer_id":"`+offer.ID.String()+`"}`, "Bearer t")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		ClickRef    string  `json:"click_ref"`
		RedirectURL string  `json:"redirect_url"`
		ExpiresAt   *string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the contract's shape: %v (body %q)", err, rec.Body.String())
	}
	if body.RedirectURL != deeplinks.url {
		t.Errorf("redirect_url = %q, want %q", body.RedirectURL, deeplinks.url)
	}
	// The reference on the wire is the one the click carries - it is what a
	// support conversation quotes, and it has to be the value the evidence
	// will be matched on.
	if body.ClickRef != clicks.inserted.ClickRef {
		t.Errorf("click_ref = %q, want the recorded %q", body.ClickRef, clicks.inserted.ClickRef)
	}
	if body.ExpiresAt == nil || *body.ExpiresAt != bandEndsAt.UTC().Format(time.RFC3339Nano) {
		t.Errorf("expires_at = %v, want the band's published end %s", body.ExpiresAt, bandEndsAt)
	}
	// The member comes from the token and never from the body.
	if uuid.UUID(clicks.inserted.AccountID.Bytes) != aMember.ID {
		t.Errorf("recorded account %v, want the authenticated caller %v", uuid.UUID(clicks.inserted.AccountID.Bytes), aMember.ID)
	}
}

// TestAnOpenEndedBandHasANullExpiry keeps "no published end" from being
// rendered as an expiry, which a client would read as an offer that has
// already closed.
func TestAnOpenEndedBandHasANullExpiry(t *testing.T) {
	t.Parallel()

	offer := anOffer()
	offer.ValidTo = time.Time{}
	h := served(t, &fakeOffers{offer: offer}, &fakeStore{echo: true}, &fakeDeeplinks{url: "https://x.test/go"}, stubMemberAuth{member: aMember})

	rec := post(t, h, `{"offer_id":"`+offer.ID.String()+`"}`, "Bearer t")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body does not parse: %v", err)
	}
	expires, present := body["expires_at"]
	if !present {
		t.Fatal("expires_at is absent; the field is always present and null when there is no end")
	}
	if expires != nil {
		t.Errorf("expires_at = %v, want null for a band with no published end", expires)
	}
}

func TestABodyThatNamesNoOfferIsRefusedBeforeAnythingHappens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "no offer at all", body: `{}`, want: "offer_id is required"},
		{name: "an empty offer", body: `{"offer_id":""}`, want: "offer_id is required"},
		{name: "an offer of spaces", body: `{"offer_id":"   "}`, want: "offer_id is required"},
		{name: "an offer that is not a UUID", body: `{"offer_id":"a-slug"}`, want: "offer_id is not a UUID"},
		{name: "the nil UUID", body: `{"offer_id":"00000000-0000-0000-0000-000000000000"}`, want: "offer_id names no offer"},
		{name: "a misspelled field", body: `{"ofer_id":"x"}`, want: "not valid JSON for this endpoint"},
		{name: "no body at all", body: ``, want: "not valid JSON for this endpoint"},
		{name: "two documents", body: `{"offer_id":"x"}{"offer_id":"y"}`, want: "single JSON document"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offers, clicks := &fakeOffers{}, &fakeStore{}
			h := served(t, offers, clicks, &fakeDeeplinks{}, stubMemberAuth{member: aMember})

			rec := post(t, h, tc.body, "Bearer t")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if _, detail := problemOf(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.want)
			}
			if offers.reads != 0 || clicks.inserts != 0 {
				t.Errorf("a refused body read %d offer(s) and recorded %d click(s), want none", offers.reads, clicks.inserts)
			}
		})
	}
}

func TestAnOfferNobodyPublishesRightNowIsAConflict(t *testing.T) {
	t.Parallel()

	clicks := &fakeStore{}
	h := served(t, &fakeOffers{err: catalogue.ErrOfferNotLive}, clicks, &fakeDeeplinks{}, stubMemberAuth{member: aMember})

	rec := post(t, h, `{"offer_id":"`+uuid.NewString()+`"}`, "Bearer t")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if _, detail := problemOf(t, rec); !strings.Contains(detail, "not available") {
		t.Errorf("detail = %q, want it to say the offer is not available", detail)
	}
	if clicks.inserts != 0 {
		t.Errorf("%d click(s) were recorded for an offer nobody publishes", clicks.inserts)
	}
}

// TestARedirectThatCannotBeBuiltIsA502AndRecordsNothing is contract test
// obligation 2, at the endpoint. The member is told plainly that this one
// cannot be opened, and told it is safe to try again - which it is, exactly
// because nothing was written.
func TestARedirectThatCannotBeBuiltIsA502AndRecordsNothing(t *testing.T) {
	t.Parallel()

	offer := anOffer()
	clicks := &fakeStore{echo: true}
	h := served(t, &fakeOffers{offer: offer}, clicks,
		&fakeDeeplinks{err: networks.ErrDeeplinkNotFormed}, stubMemberAuth{member: aMember})

	rec := post(t, h, `{"offer_id":"`+offer.ID.String()+`"}`, "Bearer t")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if clicks.inserts != 0 {
		t.Errorf("%d click(s) were recorded although no redirect was built", clicks.inserts)
	}
	status, detail := problemOf(t, rec)
	if status != http.StatusBadGateway {
		t.Errorf("the problem says %d, want %d", status, http.StatusBadGateway)
	}
	// Plain, and useful: nothing about templates, adapters or networks.
	for _, leak := range []string{"deeplink", "template", "adapter"} {
		if strings.Contains(strings.ToLower(detail), leak) {
			t.Errorf("detail = %q, which tells a member about %q", detail, leak)
		}
	}
}

func TestTheWrongMethodIsA405WithAnAllowHeader(t *testing.T) {
	t.Parallel()

	h := served(t, &fakeOffers{}, &fakeStore{}, &fakeDeeplinks{}, stubMemberAuth{member: aMember})
	req := httptest.NewRequest(http.MethodGet, clickout.Prefix, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Errorf("Allow = %q, want %q", allow, "POST")
	}
}

func TestAStraySubPathIsProblemJSON(t *testing.T) {
	t.Parallel()

	h := served(t, &fakeOffers{}, &fakeStore{}, &fakeDeeplinks{}, stubMemberAuth{member: aMember})
	req := httptest.NewRequest(http.MethodPost, clickout.Prefix+"/somewhere", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if _, detail := problemOf(t, rec); !strings.Contains(detail, "no such endpoint") {
		t.Errorf("detail = %q, want the catch-all's answer", detail)
	}
}

// TestEveryRegisteredRouteIsReachable proves Patterns() is not bookkeeping
// that drifted from the mux: a pattern that lost its registration falls to
// the catch-all, which dresses the loss up as an ordinary 404.
func TestEveryRegisteredRouteIsReachable(t *testing.T) {
	t.Parallel()

	offer := anOffer()
	h := served(t, &fakeOffers{offer: offer}, &fakeStore{echo: true},
		&fakeDeeplinks{url: "https://x.test/go"}, stubMemberAuth{member: aMember})

	for _, pattern := range clickout.Patterns() {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				t.Fatalf("pattern %q is not %q", pattern, "METHOD /path")
			}
			if !strings.HasPrefix(path, clickout.Prefix) {
				t.Fatalf("%q is outside the mounted path %q, so nothing would reach it", pattern, clickout.Prefix)
			}
			req := httptest.NewRequest(method, path, strings.NewReader(`{"offer_id":"`+offer.ID.String()+`"}`))
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			var problem struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err == nil {
				if strings.Contains(problem.Detail, "no such endpoint") ||
					strings.Contains(problem.Detail, "is not allowed on this endpoint") {
					t.Fatalf("%s is listed by Patterns() but the catch-all answered it: %q", pattern, problem.Detail)
				}
			}
		})
	}
}
