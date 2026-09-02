package ops_test

// A blank reason on ANY operator action is refused with 400 (T122, FR-061,
// US7 scenario 3). One table over every action that records a reason, so
// an action added without the check fails here rather than in an audit.
//
// Every dependency behind the surface is unreachable: a case that got past
// the check would fail loudly by reaching one, rather than pass by
// recording a decision with no reason on it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

func TestABlankReasonIsRefusedOnEveryOperatorAction(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	actions := map[string]struct {
		path string
		body func(reason string) string
	}{
		"dismissing unattributed work": {"unattributed/" + id + "/dismiss", func(r string) string { return `{"reason":` + r + `}` }},
		"refusing a withdrawal":        {"withdrawals/" + id + "/reject", func(r string) string { return `{"reason":` + r + `}` }},
		"resolving a difference":       {"reconciliation/differences/" + id + "/resolve", func(r string) string { return `{"resolution":"explained","reason":` + r + `}` }},
		"releasing a held credit":      {"held/" + id + "/release", func(r string) string { return `{"reason":` + r + `}` }},
		"rejecting a held credit":      {"held/" + id + "/reject", func(r string) string { return `{"reason":` + r + `}` }},
	}
	blanks := map[string]string{
		"empty":           `""`,
		"spaces":          `"   "`,
		"tabs and breaks": `"\t\n"`,
	}
	for name, action := range actions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for blank, reason := range blanks {
				req := httptest.NewRequest(http.MethodPost, ops.Prefix+action.path, strings.NewReader(action.body(reason)))
				req.Header.Set("Authorization", "Bearer t")
				rec := httptest.NewRecorder()
				ops.NewHandler(discardLogger(), unreachableStore{}, unreachableApprover{}, unreachableRefuser{}, unreachableSettler{},
					unreachableReconciliation{}, unreachableHeld{}, stubAuth{op: anOperator}).ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s reason: status = %d, want 400 (body %q)", blank, rec.Code, rec.Body.String())
					continue
				}
				if detail := problemDetail(t, rec); !strings.Contains(detail, "reason") {
					t.Errorf("%s reason: the 400 does not name the reason: %q", blank, detail)
				}
			}
		})
	}
}
