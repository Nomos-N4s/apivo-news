package ops_test

// POST /api/v1/cashback/ops/unattributed/{id}/dismiss: the one decision an
// operator may take on this queue today.
//
// The store is faked here. What the endpoint owes on its own is the shape
// of the decision - a named human and a non-blank reason, refused before
// anything is written - and the statuses it maps the store's verdicts to.
// Whether the write is atomic with its event, and whether two operators
// racing end with one reason recorded, is asserted against the real schema
// in the integration test beside this one.

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

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// dismissStore answers with a canned verdict and records what it was asked
// to do.
type dismissStore struct {
	result ops.Dismissed
	err    error

	got    ops.Dismissal
	called int
}

func (*dismissStore) Open(context.Context, networks.After, int) ([]networks.OpenReport, error) {
	return nil, errors.New("the dismissal cases must not list anything")
}

func (s *dismissStore) Dismiss(_ context.Context, d ops.Dismissal) (ops.Dismissed, error) {
	s.got, s.called = d, s.called+1
	return s.result, s.err
}

// dismiss sends an authenticated POST for the given row id and body.
func dismiss(t *testing.T, store ops.UnattributedStore, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ops.Prefix+"unattributed/"+id+"/dismiss", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	ops.NewHandler(discardLogger(), store, stubAuth{op: anOperator}).ServeHTTP(rec, req)
	return rec
}

func TestADismissalRecordsTheOperatorAndTheReason(t *testing.T) {
	t.Parallel()

	row, report := uuid.New(), uuid.New()
	resolvedAt := time.Date(2026, time.August, 30, 11, 0, 0, 123456000, time.UTC)
	store := &dismissStore{result: ops.Dismissed{
		ID:         row,
		ReportID:   report,
		DetectedAt: resolvedAt.Add(-3 * time.Hour),
		ResolvedBy: anOperator.ID,
		Reason:     "the network confirmed this is a staff test order",
		ResolvedAt: resolvedAt,
	}}

	rec := dismiss(t, store, row.String(), `{"reason":"  the network confirmed this is a staff test order  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The acting account comes from the token, never from the body: a
	// caller who could name the operator could record a decision against
	// somebody else's name (C-4's rule, applied to every operator action).
	if store.got.Operator.ID != anOperator.ID {
		t.Errorf("recorded operator = %s, want the authenticated caller %s", store.got.Operator.ID, anOperator.ID)
	}
	if store.got.ID != row {
		t.Errorf("dismissed row = %s, want the one in the path, %s", store.got.ID, row)
	}
	// Trimmed, so a reason of spaces cannot be stored as one of spaces -
	// the schema's btrim check would refuse it anyway, and refusing it here
	// gives the operator a message instead of a 500.
	if store.got.Reason != "the network confirmed this is a staff test order" {
		t.Errorf("recorded reason = %q, want it trimmed", store.got.Reason)
	}

	var body struct {
		ID                   string `json:"id"`
		NetworkTransactionID string `json:"network_transaction_id"`
		ResolvedBy           string `json:"resolved_by"`
		Reason               string `json:"reason"`
		ResolvedAt           string `json:"resolved_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the recorded resolution: %v (body %q)", err, rec.Body.String())
	}
	for _, pair := range []struct{ name, got, want string }{
		{"id", body.ID, row.String()},
		{"network_transaction_id", body.NetworkTransactionID, report.String()},
		{"resolved_by", body.ResolvedBy, anOperator.ID.String()},
		{"reason", body.Reason, "the network confirmed this is a staff test order"},
		// The instant is the row's, read back, not the one the caller sent
		// or the server guessed.
		{"resolved_at", body.ResolvedAt, resolvedAt.Format(time.RFC3339Nano)},
	} {
		if pair.got != pair.want {
			t.Errorf("%s = %q, want %q", pair.name, pair.got, pair.want)
		}
	}
}

// TestNothingIsWrittenWithoutAReason is FR-061 at the boundary: the audit
// record is part of the action, so a dismissal without one never reaches
// the store at all.
func TestNothingIsWrittenWithoutAReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "no reason field", body: `{}`, want: "non-blank reason"},
		{name: "an empty reason", body: `{"reason":""}`, want: "non-blank reason"},
		{name: "a reason of spaces", body: `{"reason":"   \t\n "}`, want: "non-blank reason"},
		{name: "a reason past the bound", body: `{"reason":"` + strings.Repeat("x", 2001) + `"}`, want: "longer than 2000"},
		{name: "a misspelled field", body: `{"resaon":"typo"}`, want: "not valid JSON for this endpoint"},
		{name: "no body at all", body: ``, want: "not valid JSON for this endpoint"},
		{name: "two documents", body: `{"reason":"one"}{"reason":"two"}`, want: "single JSON document"},
		{name: "trailing rubbish", body: `{"reason":"one"}}`, want: "single JSON document"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &dismissStore{}
			rec := dismiss(t, store, uuid.New().String(), tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if store.called != 0 {
				t.Errorf("the store was called %d times; a refused dismissal writes nothing", store.called)
			}
			if _, detail := problemOf(t, rec); !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

// TestAReasonAtTheBoundIsAccepted keeps the length refusal from being an
// off-by-one that silently costs an operator a paragraph.
func TestAReasonAtTheBoundIsAccepted(t *testing.T) {
	t.Parallel()

	store := &dismissStore{}
	rec := dismiss(t, store, uuid.New().String(), `{"reason":"`+strings.Repeat("x", 2000)+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestTheReasonIsCountedInCharactersNotBytes pins which unit the bound is
// in. Counting bytes would cut an operator writing in Greek off at a third
// of the length one writing in English gets, on the same product.
func TestTheReasonIsCountedInCharactersNotBytes(t *testing.T) {
	t.Parallel()

	// 2000 runes, three bytes each.
	rec := dismiss(t, &dismissStore{}, uuid.New().String(), `{"reason":"`+strings.Repeat("δ", 2000)+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestAnIdThatIsNotAUUIDIsRefusedBeforeAnyLookup(t *testing.T) {
	t.Parallel()

	store := &dismissStore{}
	rec := dismiss(t, store, "not-a-uuid", `{"reason":"whatever"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if store.called != 0 {
		t.Errorf("the store was called %d times for an id that cannot name a row", store.called)
	}
}

func TestTheVerdictsTheStoreReturnsBecomeTheRightStatus(t *testing.T) {
	t.Parallel()

	row := uuid.New()

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantDetail string
	}{
		{
			// A queue row is never deleted, so an id that matches nothing
			// was never issued: a client's mistake, not a race.
			name: "an id that names no row", err: ops.ErrNoSuchQueueRow,
			wantStatus: http.StatusNotFound, wantDetail: "no such unattributed queue row",
		},
		{
			name: "somebody else got there first",
			err: ops.ClosedError{ID: row, Why: ops.ClosedReason{
				Resolved: true, Reason: "duplicate of TX-9",
			}},
			wantStatus: http.StatusConflict, wantDetail: "duplicate of TX-9",
		},
		{
			name:       "the money has since been decided",
			err:        ops.ClosedError{ID: row, Why: ops.ClosedReason{Credited: true}},
			wantStatus: http.StatusConflict, wantDetail: "an entry now cites the report",
		},
		{
			name:       "the network moved underneath the page",
			err:        ops.ClosedError{ID: row, Why: ops.ClosedReason{Superseded: true}},
			wantStatus: http.StatusConflict, wantDetail: "replaced the report",
		},
		{
			// Two at once is the ordinary case after a busy hour, and an
			// operator told only the first would reload and still not
			// understand the row.
			name: "more than one reason at once",
			err: ops.ClosedError{ID: row, Why: ops.ClosedReason{
				Credited: true, Superseded: true,
			}},
			wantStatus: http.StatusConflict, wantDetail: "; ",
		},
		{
			name: "the verdict without a classification behind it", err: networks.ErrNoLongerOpen,
			wantStatus: http.StatusConflict, wantDetail: "no longer open work",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := dismiss(t, &dismissStore{err: tc.err}, row.String(), `{"reason":"a reason"}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if _, detail := problemOf(t, rec); !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.wantDetail)
			}
		})
	}
}

// TestAFailedDismissalIsOpaque keeps the store's troubles off the wire, and
// keeps a failure from reading as a refusal: a 500 says the decision was
// not recorded, where a 409 would say somebody else recorded one.
func TestAFailedDismissalIsOpaque(t *testing.T) {
	t.Parallel()

	const secret = `pq: deadlock detected on relation cashback.unattributed_transaction`
	rec := dismiss(t, &dismissStore{err: errors.New(secret)}, uuid.New().String(), `{"reason":"a reason"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "deadlock") {
		t.Errorf("the answer leaks the failure: %q", rec.Body.String())
	}
}

// TestAClosedErrorIsAlsoAnOpennessVerdict keeps the two matchable ways of
// reading the same failure in step, so a caller that only knows the
// sentinel is never surprised by a type it has not heard of.
func TestAClosedErrorIsAlsoAnOpennessVerdict(t *testing.T) {
	t.Parallel()

	err := error(ops.ClosedError{ID: uuid.New(), Why: ops.ClosedReason{Resolved: true}})
	if !errors.Is(err, networks.ErrNoLongerOpen) {
		t.Errorf("a ClosedError does not match %v", networks.ErrNoLongerOpen)
	}
	if err.Error() == "" {
		t.Error("a ClosedError renders no message")
	}
}
