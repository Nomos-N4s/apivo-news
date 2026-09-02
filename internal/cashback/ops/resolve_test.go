package ops_test

// What a resolution must be before it is recorded (T112, FR-061).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// aResolution is a valid decision, for the cases to spoil.
func aResolution() ops.Resolution {
	return ops.Resolution{
		ID:       uuid.New(),
		Verdict:  ops.VerdictExplained,
		Reason:   "paid in the September statement",
		Operator: ops.Operator{ID: uuid.New(), DisplayName: "Ops Person"},
	}
}

func TestAResolutionIsRefusedBeforeItIsRecorded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		spoil func(r *ops.Resolution)
		want  string
	}{
		{"naming no difference", func(r *ops.Resolution) { r.ID = uuid.Nil }, "names no difference"},
		{"with no verdict", func(r *ops.Resolution) { r.Verdict = "" }, "is not a verdict"},
		{"with a verdict nobody knows", func(r *ops.Resolution) { r.Verdict = "report_stands" }, `"report_stands" is not a verdict`},
		{"with no reason", func(r *ops.Resolution) { r.Reason = "" }, "non-blank reason"},
		{"with a blank reason", func(r *ops.Resolution) { r.Reason = " \t\n" }, "non-blank reason"},
		{"with a reason that goes on too long", func(r *ops.Resolution) { r.Reason = strings.Repeat("x", 2001) }, "longer than 2000 characters"},
		{"by nobody", func(r *ops.Resolution) { r.Operator = ops.Operator{} }, "nobody is deciding it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := aResolution()
			tc.spoil(&r)
			err := r.Validate()
			if !errors.Is(err, ops.ErrInvalidResolution) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidResolution", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestAReasonOfExactlyTheLimitIsAllowed(t *testing.T) {
	t.Parallel()
	r := aResolution()
	// Runes, not bytes: the limit is what an operator typed.
	r.Reason = strings.Repeat("é", 2000)
	if err := r.Validate(); err != nil {
		t.Errorf("a reason of exactly the limit was refused: %v", err)
	}
}

func TestTheVerdictsAreExactlyTheTwoTheSchemaKnows(t *testing.T) {
	t.Parallel()
	for _, v := range []ops.Verdict{ops.VerdictExplained, ops.VerdictAbsorbed} {
		if !v.Valid() {
			t.Errorf("%s is not valid", v)
		}
	}
	// "The network owes us and we are chasing it" is deliberately not a
	// verdict: an open difference is the chase.
	for _, v := range []ops.Verdict{"", "report_stands", "statement_stands", "chasing", "Explained"} {
		if v.Valid() {
			t.Errorf("%q is valid; it must not be", string(v))
		}
	}
}

func TestAnAlreadyResolvedErrorSaysWhoDecidedWhat(t *testing.T) {
	t.Parallel()
	err := ops.AlreadyResolvedError{
		ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), By: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Verdict: ops.VerdictAbsorbed, Reason: "too small to dispute", At: time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC),
	}
	for _, want := range []string{"11111111", "22222222", "absorbed", "too small to dispute", "2026-09-02T10:00:00Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err.Error(), want)
		}
	}
}
