package clickout_test

// Applying the click rule: what it counts, when it does not count at all,
// and what a failed count is not.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
)

// fakeRates answers with canned counts and records what it was asked.
type fakeRates struct {
	byAccount  store.CountRecentClicksByAccountRow
	byContext  store.CountRecentClicksByContextRow
	accountErr error
	contextErr error

	askedAccount store.CountRecentClicksByAccountParams
	askedContext store.CountRecentClicksByContextParams
	accountReads int
	contextReads int
}

func (f *fakeRates) CountRecentClicksByAccount(_ context.Context, arg store.CountRecentClicksByAccountParams) (store.CountRecentClicksByAccountRow, error) {
	f.askedAccount, f.accountReads = arg, f.accountReads+1
	return f.byAccount, f.accountErr
}

func (f *fakeRates) CountRecentClicksByContext(_ context.Context, arg store.CountRecentClicksByContextParams) (store.CountRecentClicksByContextRow, error) {
	f.askedContext, f.contextReads = arg, f.contextReads+1
	return f.byContext, f.contextErr
}

// counted builds a row for n clicks whose oldest is at the given instant.
func counted(n int64, oldest time.Time) store.CountRecentClicksByAccountRow {
	row := store.CountRecentClicksByAccountRow{Clicks: n}
	if !oldest.IsZero() {
		row.Oldest = pgtype.Timestamptz{Time: oldest, Valid: true}
	}
	return row
}

// limiter builds one over the given counts and rule.
func limiter(t *testing.T, rates clickout.ClickRates, rule clickout.ClickRule) *clickout.Limiter {
	t.Helper()
	l, err := clickout.NewLimiter(rates, rule)
	if err != nil {
		t.Fatalf("NewLimiter(): %v", err)
	}
	return l
}
func TestTheRuleCountsTheWindowItSaysItDoes(t *testing.T) {
	t.Parallel()

	member := uuid.New()
	rates := &fakeRates{byAccount: counted(1, clickedAt.Add(-time.Minute))}
	if err := limiter(t, rates, clickout.ClickRule{PerMember: 60}).Allow(t.Context(), member, clickout.ContextDigest{}, clickedAt); err != nil {
		t.Fatalf("Allow(): %v", err)
	}

	if uuid.UUID(rates.askedAccount.AccountID.Bytes) != member {
		t.Errorf("counted clicks for %v, want %v", uuid.UUID(rates.askedAccount.AccountID.Bytes), member)
	}
	want := clickedAt.Add(-clickout.ClickWindow)
	if !rates.askedAccount.Since.Time.Equal(want) {
		t.Errorf("counted since %s, want %s - one window back from the moment being decided", rates.askedAccount.Since.Time, want)
	}
}
func TestTheContextHalfIsOnlyAppliedWhenItCanMeanSomething(t *testing.T) {
	t.Parallel()

	digest := clickout.NewContextDigest("ua/1.0", "203.0.113.7")

	cases := []struct {
		name        string
		rule        clickout.ClickRule
		digest      clickout.ContextDigest
		wantContext int
		wantRefused bool
	}{
		{
			// The default: a deployment that cannot tell devices apart
			// leaves this half off, because the digest would be its proxy
			// and the limit would bracket every member behind it.
			name: "the rule turns it off", rule: clickout.ClickRule{PerMember: 60},
			digest: digest,
		},
		{
			name: "nothing was digested", rule: clickout.ClickRule{PerMember: 60, PerContext: 5},
		},
		{
			name: "both a rule and a digest", rule: clickout.ClickRule{PerMember: 60, PerContext: 5},
			digest: digest, wantContext: 1, wantRefused: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rates := &fakeRates{
				byAccount: counted(1, clickedAt.Add(-time.Minute)),
				byContext: store.CountRecentClicksByContextRow{
					Clicks: 5, Oldest: pgtype.Timestamptz{Time: clickedAt.Add(-time.Minute), Valid: true},
				},
			}
			err := limiter(t, rates, tc.rule).Allow(t.Context(), uuid.New(), tc.digest, clickedAt)

			if rates.contextReads != tc.wantContext {
				t.Errorf("the context was counted %d time(s), want %d", rates.contextReads, tc.wantContext)
			}
			var tooMany clickout.TooManyClicks
			if errors.As(err, &tooMany) != tc.wantRefused {
				t.Fatalf("Allow() = %v, want refused = %v", err, tc.wantRefused)
			}
			if tc.wantRefused && tooMany.Rule != clickout.RuleContext {
				t.Errorf("refused as %q, want %q", tooMany.Rule, clickout.RuleContext)
			}
		})
	}
}

// TestTheMemberHalfIsCheckedFirstAndOnItsOwn keeps one member's flood
// refused whether or not this deployment can tell devices apart.
func TestTheMemberHalfIsCheckedFirstAndOnItsOwn(t *testing.T) {
	t.Parallel()

	rates := &fakeRates{
		byAccount: counted(60, clickedAt.Add(-time.Minute)),
		byContext: store.CountRecentClicksByContextRow{Clicks: 0},
	}
	err := limiter(t, rates, clickout.ClickRule{PerMember: 60, PerContext: 5}).
		Allow(t.Context(), uuid.New(), clickout.NewContextDigest("ua/1.0"), clickedAt)

	var tooMany clickout.TooManyClicks
	if !errors.As(err, &tooMany) || tooMany.Rule != clickout.RuleMember {
		t.Fatalf("Allow() = %v, want the member rule to refuse", err)
	}
	// And the second half is not even asked: the answer is already no, and
	// a refused click should cost one query rather than two.
	if rates.contextReads != 0 {
		t.Errorf("the context was counted %d time(s) after the member rule refused", rates.contextReads)
	}
}

// TestAFailedCountIsNotARefusal keeps an unreachable database from reading
// as abuse - which would tell a member to wait an hour for a problem that
// has nothing to do with them.
func TestAFailedCountIsNotARefusal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		rates *fakeRates
	}{
		{name: "the member count failed", rates: &fakeRates{accountErr: errors.New("connection reset")}},
		{
			name: "the context count failed",
			rates: &fakeRates{
				byAccount:  counted(1, clickedAt.Add(-time.Minute)),
				contextErr: errors.New("connection reset"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := limiter(t, tc.rates, clickout.ClickRule{PerMember: 60, PerContext: 5}).
				Allow(t.Context(), uuid.New(), clickout.NewContextDigest("ua/1.0"), clickedAt)

			if err == nil {
				t.Fatal("Allow() returned no error although the count failed")
			}
			if errors.Is(err, clickout.ErrTooManyClicks) {
				t.Error("a failed count reads as too many clicks")
			}
		})
	}
}

func TestALimiterNeedsSomewhereToCount(t *testing.T) {
	t.Parallel()

	if _, err := clickout.NewLimiter(nil, clickout.DefaultClickRule()); !errors.Is(err, clickout.ErrNoClickRates) {
		t.Fatalf("NewLimiter(nil) = %v, want one wrapping %v", err, clickout.ErrNoClickRates)
	}
}
