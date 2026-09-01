package payout_test

// The tests for the read endpoints and the destination endpoints (T095).
//
// What is under test on the reads is WHOSE data comes back and what is left
// out of it; on the POST it is that bank details reach the vault and nothing
// else (ADR-0003).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// get sends an authenticated GET and returns the response.
func get(t *testing.T, h http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postTo sends an authenticated POST to an arbitrary path.
func postTo(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAMemberReadsBackTheirOwnWithdrawals. The member comes from the token,
// so there is no request this endpoint can express that reads somebody
// else's.
func TestAMemberReadsBackTheirOwnWithdrawals(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, made := approvable(ctx, t)
	h := handlerFor(t, f)

	// Another member with a withdrawal of their own, which must not appear.
	stranger := seedMember(ctx, t, pool)
	strangerDestination := seedDestination(ctx, t, pool, stranger, true)
	seedConfirmedEntry(ctx, t, pool, stranger, 9000)
	credit(ctx, t, f.ledger, stranger, euro(t, 9000), "stranger:"+stranger.String())
	if _, err := f.withdrawals.Request(ctx, payout.Request{
		Member: stranger, Destination: strangerDestination, Amount: euro(t, 5000),
	}); err != nil {
		t.Fatalf("the stranger's request: %v", err)
	}

	rec := get(t, h, "good-token", payout.Prefix)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			RequestID     string `json:"request_id"`
			DestinationID string `json:"destination_id"`
			State         string `json:"state"`
			RequestedAt   string `json:"requested_at"`
			Amount        struct {
				Minor    int64  `json:"minor"`
				Currency string `json:"currency"`
			} `json:"amount"`
			DecidedAt       *string `json:"decided_at"`
			DecisionReason  *string `json:"decision_reason"`
			PayoutReference *string `json:"payout_reference"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("listed %d withdrawal(s), want only this member's one", len(body.Items))
	}
	item := body.Items[0]
	if item.RequestID != made.ID.String() {
		t.Errorf("listed %s, want %s", item.RequestID, made.ID)
	}
	if item.State != string(payout.StateAwaitingApproval) {
		t.Errorf("state = %q, want %q", item.State, payout.StateAwaitingApproval)
	}
	if item.Amount.Minor != made.Amount.Minor || item.Amount.Currency != "EUR" {
		t.Errorf("amount = %d %s, want %s", item.Amount.Minor, item.Amount.Currency, made.Amount)
	}
	// Undecided, so all three decision fields are null rather than blank
	// strings a client would render.
	if item.DecidedAt != nil || item.DecisionReason != nil || item.PayoutReference != nil {
		t.Errorf("an undecided request carries %v/%v/%v, want nulls",
			item.DecidedAt, item.DecisionReason, item.PayoutReference)
	}
}

// TestARefusedWithdrawalShowsTheMemberWhy. FR-061's reason is recorded for an
// operator; this is the half that reaches the person it is about.
func TestARefusedWithdrawalShowsTheMemberWhy(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, made := approvable(ctx, t)
	operator := seedOperator(ctx, t, pool)
	const reason = "the network reversed the commission after the return window"

	if _, err := rejections(t, f).Reject(ctx, payout.Rejection{
		Request: made.ID, Operator: operator, Reason: reason,
	}); err != nil {
		t.Fatalf("Reject(): %v", err)
	}

	rec := get(t, handlerFor(t, f), "good-token", payout.Prefix+"/"+made.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var item struct {
		State          string  `json:"state"`
		DecidedAt      *string `json:"decided_at"`
		DecisionReason *string `json:"decision_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if item.State != string(payout.StateRejected) {
		t.Errorf("state = %q, want %q", item.State, payout.StateRejected)
	}
	if item.DecisionReason == nil || *item.DecisionReason != reason {
		t.Errorf("decision_reason = %v, want %q", item.DecisionReason, reason)
	}
	if item.DecidedAt == nil {
		t.Error("decided_at is null on a decided request")
	}
}

// TestAnotherMembersWithdrawalIsNotFound. The same 404 as an id that names
// nothing, so an id cannot be probed for existence one request at a time.
func TestAnotherMembersWithdrawalIsNotFound(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f, made := approvable(ctx, t)

	stranger := seedMember(ctx, t, pool)
	strangerHandler, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, &stubVault{},
		stubAuth{token: "good-token", member: stranger})
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}

	if rec := get(t, strangerHandler, "good-token", payout.Prefix+"/"+made.ID.String()); rec.Code != http.StatusNotFound {
		t.Errorf("a stranger reading it: status = %d, want 404", rec.Code)
	}
	h := handlerFor(t, f)
	if rec := get(t, h, "good-token", payout.Prefix+"/"+uuid.NewString()); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown id: status = %d, want the same 404", rec.Code)
	}
	if rec := get(t, h, "good-token", payout.Prefix+"/not-a-uuid"); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed id: status = %d, want 400", rec.Code)
	}
}

// TestDestinationsAreListedWithoutTheirDetails is ADR-0003 on the wire. What
// a member sees is which rail it is for and whether it is proved theirs; the
// details are somewhere this service cannot read them from, and an endpoint
// that echoed them would be the leak the arrangement exists to prevent.
func TestDestinationsAreListedWithoutTheirDetails(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	unverified := seedDestination(ctx, t, pool, f.member, false)

	rec := get(t, handlerFor(t, f), "good-token", payout.DestinationsPrefix)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "vault:") || strings.Contains(rec.Body.String(), "details") {
		t.Errorf("the body carries details or a reference to them: %s", rec.Body.String())
	}

	var body struct {
		Items []struct {
			DestinationID  string  `json:"destination_id"`
			Kind           string  `json:"kind"`
			VerifiedAt     *string `json:"verified_at"`
			VerifiedMethod *string `json:"verified_method"`
			CreatedAt      string  `json:"created_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("listed %d destination(s), want the verified one and the unverified one", len(body.Items))
	}
	var sawVerified, sawUnverified bool
	for _, item := range body.Items {
		switch item.DestinationID {
		case f.destination.String():
			sawVerified = true
			if item.VerifiedAt == nil || item.VerifiedMethod == nil {
				t.Error("the verified destination reads as unverified")
			}
		case unverified.String():
			sawUnverified = true
			if item.VerifiedAt != nil || item.VerifiedMethod != nil {
				t.Errorf("the unverified destination carries %v/%v, want nulls", item.VerifiedAt, item.VerifiedMethod)
			}
		}
		if item.Kind != "stub" {
			t.Errorf("kind = %q, want stub", item.Kind)
		}
	}
	if !sawVerified || !sawUnverified {
		t.Error("the list is missing one of the member's destinations")
	}
}

// TestARecordedDestinationArrivesUnverifiedAndItsDetailsGoToTheVault.
// Verification is a separate flow (FR-051): one that arrived verified would
// be one nobody proved belongs to the member.
func TestARecordedDestinationArrivesUnverifiedAndItsDetailsGoToTheVault(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	vault := &stubVault{}
	h, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, vault,
		stubAuth{token: "good-token", member: f.member})
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}

	rec := postTo(t, h, "good-token", payout.DestinationsPrefix,
		`{"kind":"sepa","details":{"iban":"DE00 0000 0000 0000 0000 00","holder":"A Member"}}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var item struct {
		DestinationID string  `json:"destination_id"`
		Kind          string  `json:"kind"`
		VerifiedAt    *string `json:"verified_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if item.VerifiedAt != nil {
		t.Errorf("verified_at = %v on a new destination, want null (FR-051)", *item.VerifiedAt)
	}
	if item.Kind != "sepa" {
		t.Errorf("kind = %q, want sepa", item.Kind)
	}
	if _, err := uuid.Parse(item.DestinationID); err != nil {
		t.Errorf("destination_id = %q, want a uuid", item.DestinationID)
	}
	// The details went to the vault, and the answer carries no trace of them.
	if len(vault.seen) != 1 || vault.seen[0] != payout.KindSEPA {
		t.Errorf("the vault saw %v, want one sepa store", vault.seen)
	}
	if strings.Contains(rec.Body.String(), "DE00") || strings.Contains(rec.Body.String(), "iban") {
		t.Errorf("the answer echoes the details: %s", rec.Body.String())
	}
}

// TestADeploymentWithNoVaultCannotRecordADestination. 503 and only here: a
// deployment that has nowhere to put bank details can still read the
// destinations it has and withdraw to them.
func TestADeploymentWithNoVaultCannotRecordADestination(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	h, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, nil,
		stubAuth{token: "good-token", member: f.member})
	if err != nil {
		t.Fatalf("NewHandler() with no vault = %v, want it built", err)
	}

	rec := postTo(t, h, "good-token", payout.DestinationsPrefix, `{"kind":"sepa","details":{"iban":"x"}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	// The rest of the surface is unaffected.
	if rec := get(t, h, "good-token", payout.DestinationsPrefix); rec.Code != http.StatusOK {
		t.Errorf("listing destinations: status = %d, want 200", rec.Code)
	}
	if rec := get(t, h, "good-token", payout.Prefix); rec.Code != http.StatusOK {
		t.Errorf("listing withdrawals: status = %d, want 200", rec.Code)
	}
}

// TestABadDestinationRequestIsRefusedWithoutQuotingIt. An error is the least
// controlled string in a system, and this one would otherwise carry an IBAN
// into a log.
func TestABadDestinationRequestIsRefusedWithoutQuotingIt(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	for name, c := range map[string]struct {
		vault *stubVault
		body  string
		want  int
	}{
		"an unknown kind":   {&stubVault{}, `{"kind":"carrier-pigeon","details":{"to":"x"}}`, http.StatusBadRequest},
		"no kind":           {&stubVault{}, `{"details":{"iban":"x"}}`, http.StatusBadRequest},
		"no details":        {&stubVault{}, `{"kind":"sepa"}`, http.StatusBadRequest},
		"an unknown field":  {&stubVault{}, `{"kind":"sepa","details":{},"verified":true}`, http.StatusBadRequest},
		"details refused":   {&stubVault{refuse: true}, `{"kind":"sepa","details":{"iban":"nonsense"}}`, http.StatusBadRequest},
		"the vault is down": {&stubVault{broken: errAVaultDown}, `{"kind":"sepa","details":{"iban":"x"}}`, http.StatusBadGateway},
	} {
		h, err := payout.NewHandler(discardLogger(), f.withdrawals, f.destinations, c.vault,
			stubAuth{token: "good-token", member: f.member})
		if err != nil {
			t.Fatalf("%s: NewHandler(): %v", name, err)
		}
		rec := postTo(t, h, "good-token", payout.DestinationsPrefix, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body: %s", name, rec.Code, c.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "iban") || strings.Contains(rec.Body.String(), "nonsense") {
			t.Errorf("%s: the refusal quotes what was sent: %s", name, rec.Body.String())
		}
	}
}

// TestEveryReadRouteNeedsAToken. The gate wraps the whole mux, so this is the
// module's rule rather than each route's - proved across all of them because
// an unguarded read here is somebody else's money.
func TestEveryReadRouteNeedsAToken(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	h := handlerFor(t, aFixture(ctx, t, 1000, 5000))

	for _, path := range []string{
		payout.Prefix,
		payout.Prefix + "/" + uuid.NewString(),
		payout.DestinationsPrefix,
	} {
		if rec := get(t, h, "", path); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: status = %d, want 401", path, rec.Code)
		}
	}
	if rec := postTo(t, h, "", payout.DestinationsPrefix, `{"kind":"sepa","details":{}}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without a token: status = %d, want 401", rec.Code)
	}
}
