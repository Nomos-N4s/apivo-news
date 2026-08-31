package wallet_test

// What GET /wallet answers, and to whom (T078).
//
// The property that matters most here is a negative and it is about the
// request rather than the response: there is no way to ASK for somebody
// else's wallet. The member comes from the token, so the case that would
// prove an authorisation bug cannot be written - which is the point, and
// TestTheMemberComesFromTheTokenAlone is how that is held.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakeAuth resolves one token to one member, and refuses everything else.
type fakeAuth struct {
	token  string
	member uuid.UUID
	err    error
}

func (f fakeAuth) AuthenticateMember(_ context.Context, token string) (wallet.Member, error) {
	if f.err != nil {
		return wallet.Member{}, f.err
	}
	if token != f.token {
		return wallet.Member{}, wallet.ErrUnauthenticated
	}
	return wallet.Member{ID: f.member}, nil
}

// serve builds the handler over the given parts and answers one request.
// The history is a reader that answers nothing, which is what the wallet
// cases need: they are about the totals, and an entries reader that could
// fail would make a totals case fail for the wrong reason.
func serve(t *testing.T, w *wallet.Wallets, auth wallet.MemberAuthenticator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, w, history(t, &fakeEntries{}), auth, req)
}

// serveWith builds the handler over both readers.
func serveWith(t *testing.T, w *wallet.Wallets, h *wallet.History, auth wallet.MemberAuthenticator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	wallet.NewHandler(slog.New(slog.DiscardHandler), w, h, auth).ServeHTTP(rec, req)
	return rec
}

// walletRequest is a GET carrying the given bearer token.
func walletRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, wallet.Prefix, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func body(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the body is not a JSON object: %v (%s)", err, raw)
	}
	return out
}

// aWallet is a service answering the given figures for fakeMember.
func aWallet(t *testing.T, threshold money.Amount) *wallet.Wallets {
	t.Helper()
	return wallets(t, &fakeBalances{held: map[wallet.Stage]money.Amount{
		wallet.StageHeld:      {Minor: 100, Currency: "EUR"},
		wallet.StagePending:   {Minor: 200, Currency: "EUR"},
		wallet.StageConfirmed: {Minor: 300, Currency: "EUR"},
		wallet.StageReserved:  {Minor: 400, Currency: "EUR"},
	}}, &fakePayouts{minor: 5000}, threshold)
}

// TestTheWalletIsAnsweredAsTheContractDescribesIt.
func TestTheWalletIsAnsweredAsTheContractDescribesIt(t *testing.T) {
	t.Parallel()

	threshold := money.Amount{Minor: 2000, Currency: "EUR"}
	rec := serve(t, aWallet(t, threshold), fakeAuth{token: "t", member: fakeMember}, walletRequest(t, "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	got := body(t, rec)
	for _, tc := range []struct {
		field string
		minor float64
	}{
		{"pending", 200},
		{"confirmed", 300},
		{"reserved", 400},
		{"paid_out", 5000},
		{"payout_threshold", 2000},
	} {
		figure, shaped := got[tc.field].(map[string]any)
		if !shaped {
			t.Errorf("%s = %#v, want an object carrying minor and currency", tc.field, got[tc.field])
			continue
		}
		if figure["minor"] != tc.minor {
			t.Errorf("%s.minor = %v, want %v", tc.field, figure["minor"], tc.minor)
		}
		// C-6: every figure names its currency. A bare integer is a number
		// the client decides a currency for.
		if figure["currency"] != "EUR" {
			t.Errorf("%s.currency = %v, want EUR", tc.field, figure["currency"])
		}
	}

	// Held is not a member-facing total. A hold is the business deciding not
	// to count money yet, and a fifth figure would invite the member to
	// count it.
	if _, shown := got["held"]; shown {
		t.Error("the wallet shows a held total; a hold is not money the member may count")
	}
}

// TestTheMemberComesFromTheTokenAlone. Two tokens, two members, two
// different wallets from one URL: there is no path segment or query
// parameter naming a member, so asking for somebody else's is not a request
// this endpoint can express.
func TestTheMemberComesFromTheTokenAlone(t *testing.T) {
	t.Parallel()

	other := uuid.New()
	payouts := &fakePayouts{minor: 5000, member: fakeMember}
	service := wallets(t, &fakeBalances{}, payouts, money.Amount{Minor: 1, Currency: "EUR"})

	mine := serve(t, service, fakeAuth{token: "mine", member: fakeMember}, walletRequest(t, "mine"))
	theirs := serve(t, service, fakeAuth{token: "theirs", member: other}, walletRequest(t, "theirs"))

	if mine.Code != http.StatusOK || theirs.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200 and 200", mine.Code, theirs.Code)
	}
	if paid := body(t, mine)["paid_out"].(map[string]any); paid["minor"] != float64(5000) {
		t.Errorf("my paid_out = %v, want 5000", paid["minor"])
	}
	if paid := body(t, theirs)["paid_out"].(map[string]any); paid["minor"] != float64(0) {
		t.Errorf("their paid_out = %v, want 0: it is not my money", paid["minor"])
	}
	// Both asked about the account behind their own token, and never about
	// anything carried in the request.
	if len(payouts.asked) != 2 {
		t.Fatalf("asked %d times, want twice", len(payouts.asked))
	}
	if uuid.UUID(payouts.asked[0].AccountID.Bytes) == uuid.UUID(payouts.asked[1].AccountID.Bytes) {
		t.Error("two tokens for two members read one member's payouts")
	}
}

// TestAnUnauthenticatedWalletIsRefused, before anything is read.
func TestAnUnauthenticatedWalletIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		token string
		auth  fakeAuth
	}{
		{"no token at all", "", fakeAuth{token: "t", member: fakeMember}},
		{"a token belonging to nobody", "wrong", fakeAuth{token: "t", member: fakeMember}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payouts := &fakePayouts{}
			service := wallets(t, &fakeBalances{}, payouts, money.Amount{Minor: 1, Currency: "EUR"})

			rec := serve(t, service, tc.auth, walletRequest(t, tc.token))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("a 401 carries no WWW-Authenticate header")
			}
			if len(payouts.asked) != 0 {
				t.Error("an unauthenticated request reached the payouts")
			}
		})
	}
}

// TestAnAuthenticatorThatFailedIsNotAVerdict. A dropped connection to the
// identity store is a 500, never a 401: telling a member their token is
// invalid when nobody checked is how a support conversation starts in the
// wrong place.
func TestAnAuthenticatorThatFailedIsNotAVerdict(t *testing.T) {
	t.Parallel()

	service := wallets(t, &fakeBalances{}, &fakePayouts{}, money.Amount{Minor: 1, Currency: "EUR"})

	rec := serve(t, service, fakeAuth{err: errors.New("connection reset")}, walletRequest(t, "t"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestADeploymentWithNoThresholdSaysSo. 503 and not 500: nothing is broken,
// the deployment is incomplete, and the two are different things to whoever
// is paged.
func TestADeploymentWithNoThresholdSaysSo(t *testing.T) {
	t.Parallel()

	service := wallets(t, &fakeBalances{}, &fakePayouts{}, money.Amount{})

	rec := serve(t, service, fakeAuth{token: "t", member: fakeMember}, walletRequest(t, "t"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body)
	}
}

// TestTheWrongMethodIsRefusedWithAllow, and a path nobody serves is a 404 -
// both in problem+json, so this surface has one error shape rather than
// ServeMux's text/plain in the corners.
func TestTheWrongMethodIsRefusedWithAllow(t *testing.T) {
	t.Parallel()

	service := wallets(t, &fakeBalances{}, &fakePayouts{}, money.Amount{Minor: 1, Currency: "EUR"})
	auth := fakeAuth{token: "t", member: fakeMember}

	req := httptest.NewRequest(http.MethodPost, wallet.Prefix, nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := serve(t, service, auth, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	// HEAD comes with GET, as HTTP requires: a client may ask for the
	// headers of anything it may ask for.
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}

	req = httptest.NewRequest(http.MethodGet, wallet.Prefix+"/nothing", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec = serve(t, service, auth, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
