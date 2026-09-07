package ops_test

// The queue the three withdrawal decisions act on, on the wire.
//
// The endpoint these cases cover did not exist while approve, reject and
// settle did, so an operator could decide about a request only if they
// already had its id. The screen that was meant to supply one was served
// from fixtures against a route the api never registered.

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

// unreachableAwaiting is the stand-in every case that must not read this
// queue is given.
type unreachableAwaiting struct{}

func (unreachableAwaiting) WithdrawalsAwaitingApproval(context.Context, ops.WithdrawalAfter, int) ([]ops.AwaitingWithdrawal, error) {
	return nil, errors.New("this case must not list withdrawals")
}

// fakeAwaiting answers with canned rows and records the page sizes it was
// asked for.
type fakeAwaiting struct {
	queue []ops.AwaitingWithdrawal
	err   error

	pages  []int
	afters []ops.WithdrawalAfter
}

func (f *fakeAwaiting) WithdrawalsAwaitingApproval(_ context.Context, after ops.WithdrawalAfter, limit int) ([]ops.AwaitingWithdrawal, error) {
	f.pages = append(f.pages, limit)
	f.afters = append(f.afters, after)
	if f.err != nil {
		return nil, f.err
	}
	if limit < len(f.queue) {
		return f.queue[:limit], nil
	}
	return f.queue, nil
}

// asked is one request nobody has decided yet.
func asked(t *testing.T) ops.AwaitingWithdrawal {
	t.Helper()
	return ops.AwaitingWithdrawal{
		ID:                        uuid.New(),
		AccountID:                 uuid.New(),
		AccountEmail:              "member@example.test",
		AmountMinor:               1910,
		Currency:                  "EUR",
		RequestedAt:               time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC),
		DestinationID:             uuid.New(),
		DestinationKind:           "sepa",
		DestinationDetailsRef:     "openbao:secret/cashback/payout-destinations/9c1b",
		DestinationVerifiedAt:     time.Date(2026, time.September, 1, 8, 0, 0, 0, time.UTC),
		DestinationVerifiedMethod: "video call, passport held to camera",
		DestinationCreatedAt:      time.Date(2026, time.August, 30, 7, 0, 0, 0, time.UTC),
	}
}

// awaitingRequest sends an authenticated request to the queue.
func awaitingRequest(t *testing.T, a ops.WithdrawalLister, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, ops.Prefix+path, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
		unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, unreachableDestinations{},
		a, unreachableNetworks{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// awaitingBody is the page shape, spelled out so a rename of a JSON key is a
// failing test rather than a screen that renders undefined.
type awaitingBody struct {
	Items []struct {
		RequestID      string `json:"request_id"`
		AccountID      string `json:"account_id"`
		AccountEmail   string `json:"account_email"`
		ReservedAmount struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"reserved_amount"`
		RequestedAt string `json:"requested_at"`
		Destination struct {
			DestinationID  string  `json:"destination_id"`
			Kind           string  `json:"kind"`
			DetailsRef     string  `json:"details_ref"`
			VerifiedAt     *string `json:"verified_at"`
			VerifiedMethod *string `json:"verified_method"`
			CreatedAt      string  `json:"created_at"`
		} `json:"destination"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// TestTheQueueShowsWhatIsWaitingToBeReleased. Every withdrawal enters this
// state whatever its size, because C-4 forbids a payout without a named
// human approver - so a row here is not an exception, it is money on its way
// out that has stopped, and the member's balance has already left their
// confirmed total to sit in it (D9).
func TestTheQueueShowsWhatIsWaitingToBeReleased(t *testing.T) {
	t.Parallel()
	row := asked(t)
	rec := awaitingRequest(t, &fakeAwaiting{queue: []ops.AwaitingWithdrawal{row}}, "withdrawals")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var page awaitingBody
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page: %v (body %q)", err, rec.Body.String())
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.RequestID != row.ID.String() {
		t.Errorf("request_id = %q, want %q", got.RequestID, row.ID)
	}
	if got.AccountEmail != row.AccountEmail {
		t.Errorf("account_email = %q, want %q — an operator's next step is often a conversation, and an account id is not one", got.AccountEmail, row.AccountEmail)
	}
	if got.Destination.DestinationID != row.DestinationID.String() {
		t.Errorf("destination.destination_id = %q, want %q", got.Destination.DestinationID, row.DestinationID)
	}
	if got.Destination.DetailsRef != row.DestinationDetailsRef {
		t.Errorf("destination.details_ref = %q, want %q — the reference is the operator's next action, because the manual rail pays by opening it in the vault", got.Destination.DetailsRef, row.DestinationDetailsRef)
	}
	if got.Destination.VerifiedAt == nil || got.Destination.VerifiedMethod == nil {
		t.Fatal("destination.verified_at and verified_method are null; a reader about to release money must be able to see that somebody proved this destination")
	}
	if *got.Destination.VerifiedMethod != row.DestinationVerifiedMethod {
		t.Errorf("destination.verified_method = %q, want %q", *got.Destination.VerifiedMethod, row.DestinationVerifiedMethod)
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %q on the only page, want null", *page.NextCursor)
	}
}

// TestTheFigureIsWhatWillBePaidAndSaysSo. The reserved amount is not what
// the member asked for: entries are reserved whole, so covering a request
// may reserve more than it, and the larger figure is what leaves the ledger.
// Nothing stores what was asked, so the one figure that exists is served
// under the one name that is true of it.
func TestTheFigureIsWhatWillBePaidAndSaysSo(t *testing.T) {
	t.Parallel()
	row := asked(t)
	rec := awaitingRequest(t, &fakeAwaiting{queue: []ops.AwaitingWithdrawal{row}}, "withdrawals")

	var page awaitingBody
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	if got := page.Items[0].ReservedAmount; got.Minor != row.AmountMinor || got.Currency != row.Currency {
		t.Errorf("reserved_amount = %d %s, want %d %s", got.Minor, got.Currency, row.AmountMinor, row.Currency)
	}
	// C-6: money is a figure and its currency, never a bare number.
	if strings.Contains(rec.Body.String(), `"amount"`) {
		t.Error(`the page carries an "amount" key; there is no such figure - only the reservation is stored, and naming it "amount" invites a comparison against a number this system does not keep`)
	}
}

// TestTheQueueTakesTheStateTheScreenSends. The client this contract was
// written for sends ?state=awaiting_approval, and parsePage refuses every
// parameter it does not know - so without this the screen would get a 400
// from its own first request.
func TestTheQueueTakesTheStateTheScreenSends(t *testing.T) {
	t.Parallel()
	rec := awaitingRequest(t, &fakeAwaiting{queue: []ops.AwaitingWithdrawal{asked(t)}},
		"withdrawals?state=awaiting_approval")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestAnotherStateIsRefusedRatherThanWidened. Ignoring the parameter would
// leave somebody believing a filter was applied; answering it would make
// this a different query wearing this one's name, over a predicate the
// partial index does not cover.
func TestAnotherStateIsRefusedRatherThanWidened(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"paid", "rejected", "approved", ""} {
		rec := awaitingRequest(t, unreachableAwaiting{}, "withdrawals?state="+state)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("state=%q: status = %d, want %d", state, rec.Code, http.StatusBadRequest)
			continue
		}
		if !strings.Contains(rec.Body.String(), "awaiting_approval") {
			t.Errorf("state=%q: the refusal does not name what is supported: %s", state, rec.Body.String())
		}
	}
}

// TestTheStateIsRefusedTwice. Two values are two filters, and answering one
// of them silently picks a winner the caller did not choose.
func TestTheStateIsRefusedTwice(t *testing.T) {
	t.Parallel()
	rec := awaitingRequest(t, unreachableAwaiting{},
		"withdrawals?state=awaiting_approval&state=awaiting_approval")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestAnUnknownParameterIsStillRefused. Taking `state` must not have opened
// the endpoint to everything else: a misspelled filter that reads as
// accepted is a page somebody trusts.
func TestAnUnknownParameterIsStillRefused(t *testing.T) {
	t.Parallel()
	rec := awaitingRequest(t, unreachableAwaiting{}, "withdrawals?stat=awaiting_approval")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestAFullPageOffersTheNext. The endpoint asks for one more row than the
// page so "is there another?" is answered by what came back rather than
// guessed from a full page.
func TestAFullPageOffersTheNext(t *testing.T) {
	t.Parallel()
	queue := make([]ops.AwaitingWithdrawal, 0, 3)
	for i := range 3 {
		row := asked(t)
		row.RequestedAt = row.RequestedAt.Add(time.Duration(i) * time.Minute)
		queue = append(queue, row)
	}
	fake := &fakeAwaiting{queue: queue}
	rec := awaitingRequest(t, fake, "withdrawals?limit=2")

	var page awaitingBody
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page: %v (body %q)", err, rec.Body.String())
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	if page.NextCursor == nil {
		t.Fatal("next_cursor is null with a further row waiting")
	}
	if len(fake.pages) != 1 || fake.pages[0] != 3 {
		t.Errorf("asked the store for %v rows, want one call for limit+1 = 3", fake.pages)
	}
}

// TestACursorFromAnotherQueueIsRefused. Every queue's cursor is a
// (timestamp, id) pair, so without the list tag one would decode cleanly as
// a position in another - and the caller would get a 200 with rows silently
// missing rather than the 400 the contract promises.
func TestACursorFromAnotherQueueIsRefused(t *testing.T) {
	t.Parallel()
	// Take a real cursor from the destinations queue, which is issued the
	// same way and tagged differently.
	first := waiting(t)
	second := waiting(t)
	second.CreatedAt = first.CreatedAt.Add(time.Minute)
	issued := operatorRequest(t, &fakeDestinations{queue: []ops.UnverifiedDestination{first, second}},
		http.MethodGet, "payout-destinations?limit=1", "")
	var destinations struct {
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &destinations); err != nil {
		t.Fatalf("decoding the destinations page: %v", err)
	}
	if destinations.NextCursor == nil {
		t.Fatal("the destinations queue issued no cursor to replay")
	}

	rec := awaitingRequest(t, unreachableAwaiting{}, "withdrawals?cursor="+*destinations.NextCursor)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestTheQueueNeedsAnOperator. The gate wraps the whole table, so this is
// less a test of this route than proof it did not escape one.
func TestTheQueueNeedsAnOperator(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, ops.Prefix+"withdrawals", nil)
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), &pageStore{}, unreachableApprover{}, unreachableRefuser{},
		unreachableSettler{}, unreachableReconciliation{}, unreachableHeld{}, unreachableDestinations{},
		unreachableAwaiting{}, unreachableNetworks{}, stubAuth{err: ops.ErrUnauthenticated}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestAQueueThatCannotBeReadIsNotAnEmptyOne. An empty page and a broken
// read look identical to a screen, and one of them means an operator sees
// "nothing to approve" while money waits.
func TestAQueueThatCannotBeReadIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	rec := awaitingRequest(t, &fakeAwaiting{err: errors.New("the database is gone")}, "withdrawals")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}
