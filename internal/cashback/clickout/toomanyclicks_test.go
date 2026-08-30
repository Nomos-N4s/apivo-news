package clickout_test

// What a refused click-out says, and when it says the rule lifts - the only
// part of a refusal a member can act on.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
)

func TestAMemberAtTheirAllowanceIsRefusedWithWhenItLifts(t *testing.T) {
	t.Parallel()

	// Exactly at the allowance, not past it: the count is of clicks already
	// recorded, and this one would be the next. A member who has made their
	// allowance has used it.
	oldest := clickedAt.Add(-50 * time.Minute)
	rates := &fakeRates{byAccount: counted(60, oldest)}

	err := limiter(t, rates, clickout.ClickRule{PerMember: 60}).
		Allow(t.Context(), uuid.New(), clickout.ContextDigest{}, clickedAt)

	var tooMany clickout.TooManyClicks
	if !errors.As(err, &tooMany) {
		t.Fatalf("Allow() = %v, want a TooManyClicks", err)
	}
	if !errors.Is(err, clickout.ErrTooManyClicks) {
		t.Errorf("Allow() = %v, want it to match the sentinel too", err)
	}
	if tooMany.Rule != clickout.RuleMember || tooMany.Allowed != 60 {
		t.Errorf("refused as %+v, want the member rule at 60", tooMany)
	}
	// The rule lifts when the oldest click in the window falls out of it.
	if want := oldest.Add(clickout.ClickWindow).Sub(clickedAt); tooMany.RetryAfter != want {
		t.Errorf("RetryAfter = %s, want %s", tooMany.RetryAfter, want)
	}
}

// TestARetryAfterIsNeverNothing keeps a client from being told to try again
// immediately, which would be refused again. The corner is a window whose
// oldest click is about to fall out of it.
func TestARetryAfterIsNeverNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		oldest time.Time
	}{
		{name: "the oldest click is falling out right now", oldest: clickedAt.Add(-clickout.ClickWindow)},
		{name: "the oldest click already fell out", oldest: clickedAt.Add(-2 * clickout.ClickWindow)},
		// Cannot happen while the count is at the limit - a window with
		// clicks in it has an earliest one - but a Retry-After computed
		// from the zero time renders as a delay of two thousand years.
		{name: "there is no oldest at all"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rates := &fakeRates{byAccount: counted(60, tc.oldest)}
			err := limiter(t, rates, clickout.ClickRule{PerMember: 60}).
				Allow(t.Context(), uuid.New(), clickout.ContextDigest{}, clickedAt)

			var tooMany clickout.TooManyClicks
			if !errors.As(err, &tooMany) {
				t.Fatalf("Allow() = %v, want a TooManyClicks", err)
			}
			if tooMany.RetryAfter < time.Second {
				t.Errorf("RetryAfter = %s, want at least a second", tooMany.RetryAfter)
			}
			if tooMany.RetryAfter > clickout.ClickWindow {
				t.Errorf("RetryAfter = %s, want no more than one window", tooMany.RetryAfter)
			}
		})
	}
}
