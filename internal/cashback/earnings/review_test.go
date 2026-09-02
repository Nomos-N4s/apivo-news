package earnings_test

// What a review must be before anything is read (T119, T121, FR-061), and
// what the service refuses at construction. What a review DOES is
// review_integration_test.go's, against the real schema.

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	walletmemory "github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
)

// aReview is a valid decision, for the cases to spoil.
func aReview() earnings.Review {
	return earnings.Review{Entry: uuid.New(), Operator: uuid.New(), Reason: "the second account is the member's partner; reviewed"}
}

func TestAReviewIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		spoil func(r *earnings.Review)
		want  string
	}{
		{"naming no entry", func(r *earnings.Review) { r.Entry = uuid.Nil }, "names no entry"},
		{"by nobody", func(r *earnings.Review) { r.Operator = uuid.Nil }, "nobody is deciding it"},
		{"with no reason", func(r *earnings.Review) { r.Reason = "" }, "non-blank reason"},
		{"with a blank reason", func(r *earnings.Review) { r.Reason = " \t\n" }, "non-blank reason"},
		{"with a reason that goes on too long", func(r *earnings.Review) { r.Reason = strings.Repeat("x", 2001) }, "longer than 2000 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := aReview()
			tc.spoil(&r)
			err := r.Validate()
			if !errors.Is(err, earnings.ErrInvalidReview) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidReview", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to say %q", err, tc.want)
			}
		})
	}
	// Runes, not bytes: the limit is what an operator typed.
	r := aReview()
	r.Reason = strings.Repeat("é", 2000)
	if err := r.Validate(); err != nil {
		t.Errorf("a reason of exactly the limit was refused: %v", err)
	}
}

func TestANotHeldErrorSaysWhatTheCreditIsInstead(t *testing.T) {
	t.Parallel()
	err := earnings.NotHeldError{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), State: earnings.StateConfirmed}
	if !errors.Is(err, earnings.ErrNotHeld) {
		t.Error("a NotHeldError does not match ErrNotHeld")
	}
	for _, want := range []string{"11111111", "confirmed", "not held"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err.Error(), want)
		}
	}
}

// TestReviewsNeedADatabaseAndALedgerButNotYetAReceivable. The receivable is
// refused at the first decision rather than here, so the operator queue
// stays mounted in a deployment that cannot move money yet.
func TestReviewsNeedADatabaseAndALedgerButNotYetAReceivable(t *testing.T) {
	t.Parallel()
	if _, err := earnings.NewReviews(nil, walletmemory.New(), "receivable"); !errors.Is(err, earnings.ErrNoEntryStore) {
		t.Errorf("NewReviews(nil db) = %v, want ErrNoEntryStore", err)
	}
	if _, err := earnings.NewReviews(unreachableDB{}, nil, "receivable"); !errors.Is(err, earnings.ErrNoLedger) {
		t.Errorf("NewReviews(nil ledger) = %v, want ErrNoLedger", err)
	}
	reviews, err := earnings.NewReviews(unreachableDB{}, walletmemory.New(), "")
	if err != nil {
		t.Fatalf("NewReviews(blank receivable) = %v, want it built and refusing later", err)
	}
	if _, err := reviews.Held(t.Context(), earnings.HeldAfter{}, 0); !errors.Is(err, earnings.ErrNotReviewed) {
		t.Errorf("Held(limit 0) = %v, want ErrNotReviewed", err)
	}
}
