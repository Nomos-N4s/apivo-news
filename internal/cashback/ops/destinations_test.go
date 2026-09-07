package ops_test

// The operator's half of FR-051 on the wire: the queue of destinations
// nobody has proved, and the act of proving one.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// unreachableDestinations is the stand-in every case that must not touch
// this surface is given.
type unreachableDestinations struct{}

func (unreachableDestinations) UnverifiedDestinations(context.Context, ops.DestinationAfter, int) ([]ops.UnverifiedDestination, error) {
	return nil, errors.New("this case must not list destinations")
}

func (unreachableDestinations) Verify(context.Context, ops.Verification) (ops.Verified, error) {
	return ops.Verified{}, errors.New("this case must not verify a destination")
}

// fakeDestinations answers with canned rows and records what it was asked.
type fakeDestinations struct {
	queue    []ops.UnverifiedDestination
	verified ops.Verified
	err      error

	asked ops.Verification
	pages []int
}

func (f *fakeDestinations) UnverifiedDestinations(_ context.Context, _ ops.DestinationAfter, limit int) ([]ops.UnverifiedDestination, error) {
	f.pages = append(f.pages, limit)
	if f.err != nil {
		return nil, f.err
	}
	if limit < len(f.queue) {
		return f.queue[:limit], nil
	}
	return f.queue, nil
}

func (f *fakeDestinations) Verify(_ context.Context, v ops.Verification) (ops.Verified, error) {
	f.asked = v
	if f.err != nil {
		return ops.Verified{}, f.err
	}
	return f.verified, nil
}

// waiting is one destination nobody has verified.
func waiting(t *testing.T) ops.UnverifiedDestination {
	t.Helper()
	return ops.UnverifiedDestination{
		ID:           uuid.New(),
		AccountID:    uuid.New(),
		AccountEmail: "member@example.test",
		Kind:         "sepa",
		DetailsRef:   "openbao:secret/cashback/payout-destinations/3f2a",
		CreatedAt:    time.Date(2026, time.September, 1, 9, 0, 0, 0, time.UTC),
	}
}

// operatorRequest sends an authenticated request to the operator surface.
func operatorRequest(t *testing.T, d ops.DestinationVerifier, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, ops.Prefix+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
		unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, d,
		stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// TestTheQueueShowsWhoCannotBePaidYet. A destination sits here from the
// moment a member records it until somebody proves it is theirs, and no
// withdrawal may name it in between - so a row here is a member waiting.
func TestTheQueueShowsWhoCannotBePaidYet(t *testing.T) {
	t.Parallel()
	row := waiting(t)
	rec := operatorRequest(t, &fakeDestinations{queue: []ops.UnverifiedDestination{row}},
		http.MethodGet, "payout-destinations", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var page struct {
		Items []struct {
			DestinationID string `json:"destination_id"`
			AccountID     string `json:"account_id"`
			AccountEmail  string `json:"account_email"`
			Kind          string `json:"kind"`
			DetailsRef    string `json:"details_ref"`
			CreatedAt     string `json:"created_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body is not the contract's list shape: %v (body %q)", err, rec.Body.String())
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d rows, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.DestinationID != row.ID.String() || got.AccountID != row.AccountID.String() {
		t.Errorf("the row names %s/%s, want %s/%s", got.DestinationID, got.AccountID, row.ID, row.AccountID)
	}
	// Verification is a conversation with a person, so the queue carries
	// the way to have it.
	if got.AccountEmail != row.AccountEmail {
		t.Errorf("account_email = %q, want %q", got.AccountEmail, row.AccountEmail)
	}
	// The reference travels; what it points at does not, and cannot.
	if got.DetailsRef != row.DetailsRef {
		t.Errorf("details_ref = %q, want %q", got.DetailsRef, row.DetailsRef)
	}
}

// TestTheQueueNeverCarriesTheDetailsThemselves is ADR-0006's rule read on
// the wire. The api cannot fetch them and must not appear able to: an
// operator opens the reference in the vault, and a field here that looked
// like an account number would be the leak the whole arrangement prevents.
func TestTheQueueNeverCarriesTheDetailsThemselves(t *testing.T) {
	t.Parallel()
	rec := operatorRequest(t, &fakeDestinations{queue: []ops.UnverifiedDestination{waiting(t)}},
		http.MethodGet, "payout-destinations", "")

	for _, forbidden := range []string{"iban", "IBAN", "holder", "account_number", "details\":{"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("the queue body carries %q: %s", forbidden, rec.Body.String())
		}
	}
}

// TestVerifyingRecordsTheOperatorAndTheMethod is FR-061 on this action: a
// verification that named nobody would be the one operator decision in this
// system that could not be defended later.
func TestVerifyingRecordsTheOperatorAndTheMethod(t *testing.T) {
	t.Parallel()
	row := waiting(t)
	fake := &fakeDestinations{verified: ops.Verified{
		ID:         row.ID,
		AccountID:  row.AccountID,
		Kind:       "sepa",
		Method:     "operator: confirmed on a support call",
		VerifiedBy: anOperator.ID,
		VerifiedAt: time.Date(2026, time.September, 2, 11, 0, 0, 0, time.UTC),
	}}

	rec := operatorRequest(t, fake, http.MethodPost, "payout-destinations/"+row.ID.String()+"/verify",
		`{"method":"operator: confirmed on a support call"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The acting operator comes from the authenticated request, never from
	// the body: a caller that could name the verifier could name somebody
	// else.
	if fake.asked.Operator.ID != anOperator.ID {
		t.Errorf("the verification names operator %s, want the authenticated %s", fake.asked.Operator.ID, anOperator.ID)
	}
	if fake.asked.ID != row.ID {
		t.Errorf("the verification names destination %s, want %s", fake.asked.ID, row.ID)
	}
	if fake.asked.Method != "operator: confirmed on a support call" {
		t.Errorf("the method reads %q, want what the operator wrote", fake.asked.Method)
	}

	var answer struct {
		DestinationID  string `json:"destination_id"`
		VerifiedMethod string `json:"verified_method"`
		VerifiedBy     string `json:"verified_by"`
		VerifiedAt     string `json:"verified_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("body is not problem+json or the contract's shape: %v (body %q)", err, rec.Body.String())
	}
	if answer.VerifiedBy != anOperator.ID.String() {
		t.Errorf("verified_by = %q, want %q", answer.VerifiedBy, anOperator.ID)
	}
	if answer.VerifiedAt == "" {
		t.Error("the answer carries no instant, so nothing says when the proof was recorded")
	}
}

// TestAVerificationWithNoMethodIsRefused. The schema makes "verified,
// method unknown" unstorable, and the endpoint says which field rather
// than letting the database say it as a 500.
func TestAVerificationWithNoMethodIsRefused(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	for _, body := range []string{`{}`, `{"method":""}`, `{"method":"   "}`} {
		rec := operatorRequest(t, unreachableDestinations{}, http.MethodPost,
			"payout-destinations/"+id.String()+"/verify", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status for %s = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

// TestVerifyingSomethingThatIsNotADestinationIs404. An id naming nothing is
// a not-found; an id that is not a UUID is the caller's own mistake.
func TestVerifyingSomethingThatIsNotADestinationIs404(t *testing.T) {
	t.Parallel()

	missing := &fakeDestinations{err: ops.ErrNoSuchDestination}
	rec := operatorRequest(t, missing, http.MethodPost,
		"payout-destinations/"+uuid.NewString()+"/verify", `{"method":"operator: checked"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	rec = operatorRequest(t, unreachableDestinations{}, http.MethodPost,
		"payout-destinations/not-a-uuid/verify", `{"method":"operator: checked"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status for a malformed id = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestTheDestinationRoutesNeedAnOperator. Every route on this surface sits
// behind the same gate, and a new one added by omission would not.
func TestTheDestinationRoutesNeedAnOperator(t *testing.T) {
	t.Parallel()

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "payout-destinations"},
		{http.MethodPost, "payout-destinations/" + uuid.NewString() + "/verify"},
	} {
		req := httptest.NewRequest(route.method, ops.Prefix+route.path, strings.NewReader(`{"method":"x"}`))
		rec := httptest.NewRecorder()
		ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
			unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, unreachableDestinations{},
			stubAuth{err: ops.ErrUnauthenticated}).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want %d", route.method, route.path, rec.Code, http.StatusUnauthorized)
		}
	}
}
