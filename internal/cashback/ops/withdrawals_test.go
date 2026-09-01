package ops_test

// What the approval endpoint answers, and - the point of the file - WHO it
// records as the approver (T092, C-4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// approve sends an authenticated POST to the approval endpoint.
func approve(t *testing.T, approver ops.WithdrawalApprover, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		ops.Prefix+"withdrawals/"+id+"/approve", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, approver, unreachableRefuser{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// aPayout is what the approver answers when a case is not about failure.
func aPayout(t *testing.T, request uuid.UUID) payout.Payout {
	t.Helper()
	amount, err := money.New(3000, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return payout.Payout{
		ID:             uuid.New(),
		Request:        request,
		Brand:          "fixture-de",
		ApprovedBy:     anOperator.ID,
		IdempotencyKey: "payout:" + request.String(),
		Amount:         amount,
		Rail:           payout.KindManual,
		RailReference:  "manual:payout:" + request.String(),
		State:          payout.StatusSubmitted,
		SubmittedAt:    time.Date(2026, time.September, 1, 9, 0, 0, 0, time.UTC),
	}
}

// TestAnApprovalIsMadeByTheTokenSubjectAndNobodyElse is C-4 on this surface.
// There is no field in the request naming an approver, and this proves the
// one the endpoint passes on is the authenticated caller.
func TestAnApprovalIsMadeByTheTokenSubjectAndNobodyElse(t *testing.T) {
	t.Parallel()
	request := uuid.New()
	approver := &stubApprover{paid: aPayout(t, request)}

	rec := approve(t, approver, request.String(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(approver.seen) != 1 {
		t.Fatalf("the approver saw %d approval(s), want one", len(approver.seen))
	}
	seen := approver.seen[0]
	if seen.Operator != anOperator.ID {
		t.Errorf("approved by %s, want the token subject %s", seen.Operator, anOperator.ID)
	}
	if seen.Request != request {
		t.Errorf("approved %s, want %s", seen.Request, request)
	}
}

// TestAnApprovalAnswersTheEvidenceItRestsOn. The approver and the key are in
// the body because an operator chasing a payment on the rail's side searches
// by the key, and needs to see which account the server recorded rather than
// which one they believe they are.
func TestAnApprovalAnswersTheEvidenceItRestsOn(t *testing.T) {
	t.Parallel()
	request := uuid.New()
	paid := aPayout(t, request)

	rec := approve(t, &stubApprover{paid: paid}, request.String(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PayoutID       string `json:"payout_id"`
		RequestID      string `json:"request_id"`
		ApprovedBy     string `json:"approved_by"`
		IdempotencyKey string `json:"idempotency_key"`
		Amount         struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"amount"`
		Rail          string  `json:"rail"`
		RailReference *string `json:"rail_reference"`
		State         string  `json:"state"`
		SubmittedAt   string  `json:"submitted_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.PayoutID != paid.ID.String() || body.RequestID != request.String() {
		t.Errorf("the body names payout %s of request %s, want %s of %s",
			body.PayoutID, body.RequestID, paid.ID, request)
	}
	if body.ApprovedBy != anOperator.ID.String() {
		t.Errorf("approved_by = %q, want the operator %s", body.ApprovedBy, anOperator.ID)
	}
	if body.IdempotencyKey != paid.IdempotencyKey {
		t.Errorf("idempotency_key = %q, want the generated %q", body.IdempotencyKey, paid.IdempotencyKey)
	}
	if body.Amount.Minor != 3000 || body.Amount.Currency != "EUR" {
		t.Errorf("amount = %d %s, want 3000 EUR", body.Amount.Minor, body.Amount.Currency)
	}
	if body.Rail != payout.KindManual.String() {
		t.Errorf("rail = %q, want %q", body.Rail, payout.KindManual)
	}
	if body.RailReference == nil || *body.RailReference != paid.RailReference {
		t.Errorf("rail_reference = %v, want %q", body.RailReference, paid.RailReference)
	}
	if body.State != payout.StatusSubmitted.String() {
		t.Errorf("state = %q, want %q", body.State, payout.StatusSubmitted)
	}
	if body.SubmittedAt != "2026-09-01T09:00:00Z" {
		t.Errorf("submitted_at = %q, want the payout row's instant", body.SubmittedAt)
	}
}

// TestAPayoutWithNoRailReferenceYetAnswersNull. A submitted payout the rail
// has not answered about is not broken - it is the state a retry picks up
// (FR-053) - so the field is null rather than an empty string a client would
// render as a reference.
func TestAPayoutWithNoRailReferenceYetAnswersNull(t *testing.T) {
	t.Parallel()
	request := uuid.New()
	paid := aPayout(t, request)
	paid.RailReference = ""

	rec := approve(t, &stubApprover{paid: paid}, request.String(), "")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if reference, present := body["rail_reference"]; !present || reference != nil {
		t.Errorf("rail_reference = %v, want null", reference)
	}
}

// TestAnApprovalTakesNoBody. Who approves and when come from the token and
// the clock, so a client that sent something believed it was saying
// otherwise - most likely naming an approver, which is exactly what C-4
// forbids being accepted quietly.
func TestAnApprovalTakesNoBody(t *testing.T) {
	t.Parallel()
	request := uuid.New()

	for name, body := range map[string]string{
		"an approver":  `{"approved_by":"` + uuid.NewString() + `"}`,
		"an empty doc": `{}`,
		"junk":         `not json`,
	} {
		rec := approve(t, unreachableApprover{}, request.String(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", name, rec.Code, rec.Body.String())
		}
	}
	// Whitespace is not a body: a client that sent a trailing newline meant
	// nothing by it.
	if rec := approve(t, &stubApprover{paid: aPayout(t, request)}, request.String(), "\n  \n"); rec.Code != http.StatusOK {
		t.Errorf("with only whitespace: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// TestAnApprovalOfSomethingThatIsNotAUUIDIsRefused. The address is well
// formed as a route and the id in it is not a request's, and saying so is
// more useful than pretending to have looked.
func TestAnApprovalOfSomethingThatIsNotAUUIDIsRefused(t *testing.T) {
	t.Parallel()

	rec := approve(t, unreachableApprover{}, "not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestEachRefusalGetsTheStatusItMeans. The mapping is the contract, and the
// 502 is the one that matters most: the approval STANDS, and an operator
// told otherwise would approve it again.
func TestEachRefusalGetsTheStatusItMeans(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"no such request":       {payout.ErrNoSuchWithdrawal, http.StatusNotFound},
		"already decided":       {payout.ErrNotAwaitingApproval, http.StatusConflict},
		"already paid":          {payout.ErrAlreadyApproved, http.StatusConflict},
		"two brands":            {payout.ErrBrandUnresolved, http.StatusConflict},
		"refused by the schema": {payout.ErrNotApproved, http.StatusConflict},
		"the rail timed out":    {payout.Retryable(errors.New("timeout")), http.StatusBadGateway},
		"the rail refused":      {payout.Terminal(errors.New("rejected")), http.StatusBadGateway},
		"something else":        {errors.New("the pool is gone"), http.StatusInternalServerError},
	} {
		rec := approve(t, &stubApprover{err: c.err}, uuid.NewString(), "")
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body: %s", name, rec.Code, c.want, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q, want application/problem+json", name, got)
		}
	}
}

// TestApprovingNeedsTheOperatorRole. The gate wraps the whole table, so this
// is the module's own rule rather than the endpoint's - and it is proved
// here because on this route it is what stands between a reader and money
// leaving.
func TestApprovingNeedsTheOperatorRole(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost,
		ops.Prefix+"withdrawals/"+uuid.NewString()+"/approve", nil)
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{},
		stubAuth{err: fmt.Errorf("%w: a reader", ops.ErrNotOperator)}).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 or 403; body: %s", rec.Code, rec.Body.String())
	}
}

// TestTheApprovalRouteIsListed keeps Patterns() and the mux in step, so the
// OpenAPI check reads what is registered.
func TestTheApprovalRouteIsListed(t *testing.T) {
	t.Parallel()
	want := "POST " + ops.Prefix + "withdrawals/{id}/approve"
	for _, pattern := range ops.Patterns() {
		if pattern == want {
			return
		}
	}
	t.Errorf("Patterns() = %v, want it to name %q", ops.Patterns(), want)
}

// stubRefuser answers one refusal, or one failure, and records what it saw.
type stubRefuser struct {
	refused payout.Rejected
	err     error
	seen    []payout.Rejection
}

func (s *stubRefuser) Reject(_ context.Context, rejection payout.Rejection) (payout.Rejected, error) {
	s.seen = append(s.seen, rejection)
	if s.err != nil {
		return payout.Rejected{}, s.err
	}
	return s.refused, nil
}

// reject sends an authenticated POST to the refusal endpoint.
func reject(t *testing.T, refuser ops.WithdrawalRefuser, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		ops.Prefix+"withdrawals/"+id+"/reject", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, refuser,
		stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

// aRefusal is what the refuser answers when a case is not about failure.
func aRefusal(t *testing.T, request uuid.UUID) payout.Rejected {
	t.Helper()
	amount, err := money.New(3000, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return payout.Rejected{
		Request: payout.Withdrawal{
			ID:             request,
			State:          payout.StateRejected,
			DecidedBy:      anOperator.ID,
			DecidedAt:      time.Date(2026, time.September, 1, 9, 30, 0, 0, time.UTC),
			DecisionReason: "the network reversed the commission",
		},
		Released:        amount,
		ReleaseTransfer: "transfer:release:" + request.String(),
	}
}

// TestARefusalRecordsTheOperatorAndTheReason. FR-061 asks for both on every
// operator action, and the operator is the token subject rather than
// anything the body could name.
func TestARefusalRecordsTheOperatorAndTheReason(t *testing.T) {
	t.Parallel()
	request := uuid.New()
	refuser := &stubRefuser{refused: aRefusal(t, request)}

	rec := reject(t, refuser, request.String(), `{"reason":"  the network reversed the commission  "}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(refuser.seen) != 1 {
		t.Fatalf("the refuser saw %d refusal(s), want one", len(refuser.seen))
	}
	seen := refuser.seen[0]
	if seen.Operator != anOperator.ID {
		t.Errorf("refused by %s, want the token subject %s", seen.Operator, anOperator.ID)
	}
	if seen.Request != request {
		t.Errorf("refused %s, want %s", seen.Request, request)
	}
	// Trimmed before it travels, so the service and the schema both see the
	// reason a person typed rather than the whitespace a form added.
	if seen.Reason != "the network reversed the commission" {
		t.Errorf("reason = %q, want it trimmed", seen.Reason)
	}
}

// TestARefusalAnswersWhatWentBack. A refusal is two facts - a decision and a
// movement - and an operator checking the second against the ledger needs its
// reference.
func TestARefusalAnswersWhatWentBack(t *testing.T) {
	t.Parallel()
	request := uuid.New()
	refused := aRefusal(t, request)

	rec := reject(t, &stubRefuser{refused: refused}, request.String(), `{"reason":"a reason"}`)

	var body struct {
		RequestID      string `json:"request_id"`
		State          string `json:"state"`
		DecidedBy      string `json:"decided_by"`
		DecidedAt      string `json:"decided_at"`
		DecisionReason string `json:"decision_reason"`
		ReleasedAmount struct {
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"released_amount"`
		ReleaseTransfer string `json:"release_transfer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body.RequestID != request.String() || body.State != string(payout.StateRejected) {
		t.Errorf("the body says %s is %s, want %s rejected", body.RequestID, body.State, request)
	}
	if body.DecidedBy != anOperator.ID.String() {
		t.Errorf("decided_by = %q, want %s", body.DecidedBy, anOperator.ID)
	}
	if body.DecidedAt != "2026-09-01T09:30:00Z" {
		t.Errorf("decided_at = %q, want the row's instant", body.DecidedAt)
	}
	if body.DecisionReason != refused.Request.DecisionReason {
		t.Errorf("decision_reason = %q, want %q", body.DecisionReason, refused.Request.DecisionReason)
	}
	if body.ReleasedAmount.Minor != 3000 || body.ReleasedAmount.Currency != "EUR" {
		t.Errorf("released_amount = %d %s, want 3000 EUR", body.ReleasedAmount.Minor, body.ReleasedAmount.Currency)
	}
	if body.ReleaseTransfer != refused.ReleaseTransfer {
		t.Errorf("release_transfer = %q, want %q", body.ReleaseTransfer, refused.ReleaseTransfer)
	}
}

// TestARefusalWithoutAReasonIsRefused, and the refuser is never reached: a
// decision nobody explained is not one to send on.
func TestARefusalWithoutAReasonIsRefused(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"absent":     `{}`,
		"empty":      `{"reason":""}`,
		"whitespace": `{"reason":"   "}`,
		"too long":   `{"reason":"` + strings.Repeat("x", 2001) + `"}`,
		"not JSON":   `{`,
		"an extra":   `{"reason":"why","approve":true}`,
	} {
		if rec := reject(t, unreachableRefuser{}, uuid.NewString(), body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestEachRefusalFailureGetsTheStatusItMeans.
func TestEachRefusalFailureGetsTheStatusItMeans(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"no such request":   {payout.ErrNoSuchWithdrawal, http.StatusNotFound},
		"already decided":   {payout.ErrNotAwaitingApproval, http.StatusConflict},
		"nothing reserved":  {payout.ErrNothingReserved, http.StatusConflict},
		"refused elsewhere": {payout.ErrNotRejected, http.StatusConflict},
		"something else":    {errors.New("the pool is gone"), http.StatusInternalServerError},
	} {
		rec := reject(t, &stubRefuser{err: c.err}, uuid.NewString(), `{"reason":"a reason"}`)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body: %s", name, rec.Code, c.want, rec.Body.String())
		}
	}
}

// TestTheRefusalRouteIsListed keeps Patterns() and the mux in step.
func TestTheRefusalRouteIsListed(t *testing.T) {
	t.Parallel()
	want := "POST " + ops.Prefix + "withdrawals/{id}/reject"
	for _, pattern := range ops.Patterns() {
		if pattern == want {
			return
		}
	}
	t.Errorf("Patterns() = %v, want it to name %q", ops.Patterns(), want)
}
