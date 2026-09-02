package ops_test

// The held queue and its two decisions with the earnings module faked
// (T119). What the endpoints owe on their own: the shape of the page and
// of each decision, the fields refused before anything is read, the
// operator taken from the token, and the statuses the module's answers map
// to. Whether the module keeps its promises is proved against the schema
// in the earnings package.

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

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// unreachableHeld stands in for the held reviewer where a case is about
// something else: every path to it is refused before the handler reaches
// it.
type unreachableHeld struct{}

func (unreachableHeld) Held(context.Context, earnings.HeldAfter, int) ([]earnings.HeldCredit, error) {
	return nil, errors.New("this case must not list held credits")
}

func (unreachableHeld) Release(context.Context, earnings.Review) (earnings.Released, error) {
	return earnings.Released{}, errors.New("this case must not release a credit")
}

func (unreachableHeld) Reject(context.Context, earnings.Review) (earnings.Rejected, error) {
	return earnings.Rejected{}, errors.New("this case must not reject a credit")
}

// heldReviewer answers with canned results and records what it was asked.
type heldReviewer struct {
	queue    []earnings.HeldCredit
	listErr  error
	gotAfter earnings.HeldAfter
	gotLimit int

	released   earnings.Released
	releaseErr error
	rejected   earnings.Rejected
	rejectErr  error
	gotReview  earnings.Review
	decisions  int
}

func (r *heldReviewer) Held(_ context.Context, after earnings.HeldAfter, limit int) ([]earnings.HeldCredit, error) {
	r.gotAfter, r.gotLimit = after, limit
	if r.listErr != nil {
		return nil, r.listErr
	}
	if len(r.queue) > limit {
		return r.queue[:limit], nil
	}
	return r.queue, nil
}

func (r *heldReviewer) Release(_ context.Context, review earnings.Review) (earnings.Released, error) {
	r.gotReview, r.decisions = review, r.decisions+1
	return r.released, r.releaseErr
}

func (r *heldReviewer) Reject(_ context.Context, review earnings.Review) (earnings.Rejected, error) {
	r.gotReview, r.decisions = review, r.decisions+1
	return r.rejected, r.rejectErr
}

// review sends an authenticated request to the held surface.
func review(t *testing.T, held ops.HeldReviewer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, ops.Prefix+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{}, unreachableSettler{}, unreachableReconciliation{}, held, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

var heldAt = time.Date(2026, time.September, 2, 9, 0, 0, 123456000, time.UTC)

// aHeldCredit is one queue row, every field set.
func aHeldCredit(id uuid.UUID) earnings.HeldCredit {
	return earnings.HeldCredit{
		ID: id, Member: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Brand: "apivo-de",
		Report: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Click: uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Rule: earnings.RuleSharedContext, Reason: "2 member accounts clicked from this device context",
		Amount: money.Amount{Minor: 150, Currency: "EUR"}, HeldSince: heldAt,
		Network: "awin", ExternalID: "AWIN-1", ReportStatus: "confirmed",
		Sale: money.Amount{Minor: 4999, Currency: "EUR"}, Commission: money.Amount{Minor: 249, Currency: "EUR"},
		TransactedAt: heldAt.Add(-time.Hour),
	}
}

func TestTheHeldQueueRendersEveryFieldAnOperatorDecidesFrom(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	rec := review(t, &heldReviewer{queue: []earnings.HeldCredit{aHeldCredit(id)}}, http.MethodGet, "held", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Next  *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body is not the page shape: %v", err)
	}
	if len(page.Items) != 1 || page.Next != nil {
		t.Fatalf("page = %+v, want one item and no next cursor", page)
	}
	item := page.Items[0]
	for field, want := range map[string]any{
		"id": id.String(), "account_id": "22222222-2222-2222-2222-222222222222", "brand_id": "apivo-de",
		"network_transaction_id": "33333333-3333-3333-3333-333333333333", "click_id": "44444444-4444-4444-4444-444444444444",
		"hold_rule": earnings.RuleSharedContext, "hold_reason": "2 member accounts clicked from this device context",
		"held_since": "2026-09-02T09:00:00.123456Z", "network_id": "awin", "external_id": "AWIN-1", "report_status": "confirmed",
		"transacted_at": "2026-09-02T08:00:00.123456Z",
	} {
		if item[field] != want {
			t.Errorf("%s = %v, want %v", field, item[field], want)
		}
	}
	for field, minor := range map[string]float64{"amount": 150, "sale": 4999, "commission": 249} {
		figure, shaped := item[field].(map[string]any)
		if !shaped || figure["minor"] != minor || figure["currency"] != "EUR" {
			t.Errorf("%s = %v, want {minor: %v, currency: EUR}", field, item[field], minor)
		}
	}
}

func TestTheHeldQueuePagesWithACursorItIssued(t *testing.T) {
	t.Parallel()
	first, second, third := aHeldCredit(uuid.New()), aHeldCredit(uuid.New()), aHeldCredit(uuid.New())
	second.HeldSince = heldAt.Add(time.Minute)
	third.HeldSince = heldAt.Add(2 * time.Minute)
	reviewer := &heldReviewer{queue: []earnings.HeldCredit{first, second, third}}

	rec := review(t, reviewer, http.MethodGet, "held?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if reviewer.gotLimit != 3 {
		t.Errorf("asked for %d rows, want limit+1 = 3", reviewer.gotLimit)
	}
	var page struct {
		Items []struct{ ID string }
		Next  *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(page.Items) != 2 || page.Next == nil {
		t.Fatalf("page = %+v, want two items and a next cursor", page)
	}

	// The cursor continues after the last row shown.
	rec = review(t, reviewer, http.MethodGet, "held?limit=2&cursor="+*page.Next, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second page status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if reviewer.gotAfter.ID != second.ID || !reviewer.gotAfter.HeldSince.Equal(second.HeldSince) {
		t.Errorf("the second page continued after %+v, want after %s", reviewer.gotAfter, second.ID)
	}

	// And a cursor another queue issued is refused, not silently restarted.
	for _, bad := range []string{"held?cursor=nope", "held?limit=0", "held?limit=101", "held?page=2"} {
		if rec := review(t, unreachableHeld{}, http.MethodGet, bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, rec.Code)
		}
	}
	if rec := review(t, &heldReviewer{listErr: errors.New("boom")}, http.MethodGet, "held", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing read answered %d, want 500", rec.Code)
	}
}

func TestAReleaseRecordsTheOperatorAndAnswersWhatWasRecorded(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	reviewer := &heldReviewer{released: earnings.Released{
		Entry: earnings.Entry{ID: id, Member: uuid.MustParse("22222222-2222-2222-2222-222222222222"), State: earnings.StatePending,
			Amount: money.Amount{Minor: 150, Currency: "EUR"}},
		Rule: earnings.RuleNewAccount, ReleasedBy: anOperator.ID, Reason: "identity verified by support", Transfer: "transfer-1", At: heldAt,
	}}
	rec := review(t, reviewer, http.MethodPost, "held/"+id.String()+"/release", `{"reason":"  identity verified by support  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// The operator comes from the token, never the body; the reason is
	// trimmed before it is recorded.
	if reviewer.gotReview != (earnings.Review{Entry: id, Operator: anOperator.ID, Reason: "identity verified by support"}) {
		t.Errorf("recorded %+v, want the id, the authenticated operator and the trimmed reason", reviewer.gotReview)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	for field, want := range map[string]any{
		"id": id.String(), "account_id": "22222222-2222-2222-2222-222222222222", "state": "pending",
		"hold_rule": earnings.RuleNewAccount, "released_by": anOperator.ID.String(), "reason": "identity verified by support",
		"ledger_transfer_ref": "transfer-1", "released_at": "2026-09-02T09:00:00.123456Z",
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %v", field, body[field], want)
		}
	}
}

func TestARejectionAnswersTheReversingEntry(t *testing.T) {
	t.Parallel()
	id, reversal := uuid.New(), uuid.New()
	reviewer := &heldReviewer{rejected: earnings.Rejected{
		Credit:   earnings.Entry{ID: id, Member: uuid.MustParse("22222222-2222-2222-2222-222222222222"), State: earnings.StateHeld, Amount: money.Amount{Minor: 150, Currency: "EUR"}},
		Reversal: earnings.Entry{ID: reversal, State: earnings.StateReversed, ReversalOf: id},
		Rule:     earnings.RuleSharedContext, RejectedBy: anOperator.ID, Reason: "both accounts are the same person", At: heldAt,
	}}
	rec := review(t, reviewer, http.MethodPost, "held/"+id.String()+"/reject", `{"reason":"both accounts are the same person"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if reviewer.gotReview != (earnings.Review{Entry: id, Operator: anOperator.ID, Reason: "both accounts are the same person"}) {
		t.Errorf("recorded %+v", reviewer.gotReview)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	for field, want := range map[string]any{
		"id": id.String(), "reversal_entry_id": reversal.String(), "hold_rule": earnings.RuleSharedContext,
		"rejected_by": anOperator.ID.String(), "reason": "both accounts are the same person", "rejected_at": "2026-09-02T09:00:00.123456Z",
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %v", field, body[field], want)
		}
	}
}

// TestADecisionIsRefusedBeforeTheModuleIsAsked. Each of these is the
// endpoint's own refusal, and the reviewer behind it is unreachable so a
// case that got through would fail loudly rather than record something.
func TestADecisionIsRefusedBeforeTheModuleIsAsked(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	for name, tc := range map[string]struct {
		path, body string
		want       string
	}{
		"an id that is not a UUID":  {"held/not-a-uuid/release", `{"reason":"ok"}`, "not a UUID"},
		"a body that is not JSON":   {"held/" + id + "/release", `{`, "not valid JSON"},
		"a field nobody asked for":  {"held/" + id + "/reject", `{"reason":"ok","released_by":"me"}`, "not valid JSON"},
		"no reason":                 {"held/" + id + "/release", `{}`, "non-blank reason"},
		"a blank reason":            {"held/" + id + "/reject", `{"reason":" \t "}`, "non-blank reason"},
		"a reason that goes on":     {"held/" + id + "/release", `{"reason":"` + strings.Repeat("x", 2001) + `"}`, "longer than 2000"},
		"two documents in one body": {"held/" + id + "/reject", `{"reason":"ok"}{}`, "single JSON document"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := review(t, unreachableHeld{}, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if detail := problemDetail(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to say %q", detail, tc.want)
			}
		})
	}
}

func TestTheModulesAnswersMapToStatuses(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	for name, tc := range map[string]struct {
		err  error
		want int
		says string
	}{
		"no such entry":         {earnings.ErrNoSuchEntry, http.StatusNotFound, "never issued"},
		"not held":              {earnings.NotHeldError{ID: id, State: earnings.StateConfirmed}, http.StatusConflict, "confirmed, not held"},
		"already rejected":      {earnings.ErrAlreadyRejected, http.StatusConflict, "already rejected"},
		"moved underneath":      {earnings.ErrEntryMoved, http.StatusConflict, "reload the queue"},
		"no receivable named":   {earnings.ErrNoReceivable, http.StatusServiceUnavailable, "HOUSE_ACCOUNT_NETWORK_RECEIVABLE"},
		"refused by the module": {earnings.ErrInvalidReview, http.StatusBadRequest, ""},
		"anything else":         {errors.New("the ledger is on fire"), http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, action := range []string{"release", "reject"} {
				reviewer := &heldReviewer{releaseErr: tc.err, rejectErr: tc.err}
				rec := review(t, reviewer, http.MethodPost, "held/"+id.String()+"/"+action, `{"reason":"looked at it"}`)
				if rec.Code != tc.want {
					t.Errorf("%s: status = %d, want %d (body %q)", action, rec.Code, tc.want, rec.Body.String())
				}
				if tc.says != "" && !strings.Contains(problemDetail(t, rec), tc.says) {
					t.Errorf("%s: detail = %q, want it to say %q", action, problemDetail(t, rec), tc.says)
				}
				// Internals are for the log, never the wire.
				if tc.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "on fire") {
					t.Errorf("%s: the 500 leaked the error: %s", action, rec.Body.String())
				}
			}
		})
	}
}
