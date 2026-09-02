package earnings_test

// The hold rules as a table (T118, T121, US7): which pattern each rule
// holds, which it lets through, that the first matching rule names itself,
// and that a rule that cannot be asked is an error rather than a pass.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// holdFacts is a fake store answering the three questions the rules ask.
type holdFacts struct {
	sharing   int64
	credits   int64
	created   time.Time
	noAccount bool
	fail      error
	asked     []string
}

func (f *holdFacts) AccountsSharingContext(_ context.Context, _ store.AccountsSharingContextParams) (int64, error) {
	f.asked = append(f.asked, earnings.RuleSharedContext)
	return f.sharing, f.fail
}

func (f *holdFacts) MemberCreditsSince(_ context.Context, _ store.MemberCreditsSinceParams) (int64, error) {
	f.asked = append(f.asked, earnings.RuleMemberVelocity)
	return f.credits, f.fail
}

func (f *holdFacts) AccountCreatedAt(_ context.Context, _ pgtype.UUID) (pgtype.Timestamptz, error) {
	f.asked = append(f.asked, earnings.RuleNewAccount)
	if f.noAccount {
		return pgtype.Timestamptz{}, pgx.ErrNoRows
	}
	return pgtype.Timestamptz{Time: f.created, Valid: true}, f.fail
}

var (
	evaluatedAt = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	clickedAt   = evaluatedAt.Add(-2 * time.Hour)
)

// everyRule is a full configuration, so a case can say which rule it
// expects to fire when all of them are on.
var everyRule = earnings.HoldRules{
	SharedContextAccounts: 3,
	SharedContextWindow:   30 * 24 * time.Hour,
	NewAccountAge:         24 * time.Hour,
	SaleCap:               money.Amount{Minor: 50_000, Currency: "EUR"},
	MemberVelocity:        5,
	MemberVelocityWindow:  24 * time.Hour,
}

// aCandidate is an ordinary purchase: a click with a context, a modest
// sale, from an account a year old.
func aCandidate() earnings.Candidate {
	return earnings.Candidate{
		Member: uuid.New(),
		Click: clickout.Click{
			ID:        uuid.New(),
			ClickedAt: clickedAt,
			Context:   clickout.NewContextDigest("203.0.113.9", "Mozilla/5.0"),
		},
		Sale: money.Amount{Minor: 4_999, Currency: "EUR"},
		At:   evaluatedAt,
	}
}

// ordinaryFacts is what the store says about a member nobody should hold.
func ordinaryFacts() *holdFacts {
	return &holdFacts{sharing: 1, credits: 0, created: clickedAt.Add(-365 * 24 * time.Hour)}
}

func TestEachRuleHoldsItsPatternAndNothingElse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rules earnings.HoldRules
		facts func() *holdFacts
		spoil func(c *earnings.Candidate)
		want  string
		says  string
	}{
		{"a normal pattern passes untouched", everyRule, ordinaryFacts, nil, "", ""},
		{"one device behind three accounts", everyRule, func() *holdFacts { f := ordinaryFacts(); f.sharing = 3; return f }, nil,
			earnings.RuleSharedContext, "3 member accounts clicked from this device context"},
		{"one device behind two accounts is under the rule", everyRule, func() *holdFacts { f := ordinaryFacts(); f.sharing = 2; return f }, nil, "", ""},
		{"a click that recorded no context cannot be judged by the device rule", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.sharing = 99; return f },
			func(c *earnings.Candidate) { c.Click.Context = clickout.ContextDigest{} }, "", ""},
		{"an account created an hour before the click", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.created = clickedAt.Add(-time.Hour); return f }, nil,
			earnings.RuleNewAccount, "the account was 1h0m0s old at the click"},
		{"an account exactly the configured age is old enough", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.created = clickedAt.Add(-24 * time.Hour); return f }, nil, "", ""},
		{"a sale at the cap", everyRule, ordinaryFacts,
			func(c *earnings.Candidate) { c.Sale = money.Amount{Minor: 50_000, Currency: "EUR"} },
			earnings.RuleSaleCap, "at or above the cap"},
		{"a sale over the cap in another currency is not compared", everyRule, ordinaryFacts,
			func(c *earnings.Candidate) { c.Sale = money.Amount{Minor: 900_000, Currency: "GBP"} }, "", ""},
		{"a member credited five times today", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.credits = 5; return f }, nil,
			earnings.RuleMemberVelocity, "credited 5 times in the last 24h0m0s"},
		{"a member credited four times today is under the rule", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.credits = 4; return f }, nil, "", ""},
		{"a rule that is off does not run", earnings.HoldRules{SaleCap: money.Amount{Minor: 50_000, Currency: "EUR"}},
			func() *holdFacts {
				f := ordinaryFacts()
				f.sharing = 99
				f.credits = 99
				f.created = clickedAt
				return f
			}, nil, "", ""},
		{"the first matching rule names itself", everyRule,
			func() *holdFacts { f := ordinaryFacts(); f.sharing = 9; f.credits = 9; f.created = clickedAt; return f },
			func(c *earnings.Candidate) { c.Sale = money.Amount{Minor: 999_999, Currency: "EUR"} },
			earnings.RuleSharedContext, ""},
		{"with no click, the account's age is measured at the evaluation", earnings.HoldRules{NewAccountAge: 24 * time.Hour},
			func() *holdFacts { f := ordinaryFacts(); f.created = evaluatedAt.Add(-time.Hour); return f },
			func(c *earnings.Candidate) { c.Click = clickout.Click{} }, earnings.RuleNewAccount, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts()
			holds, err := earnings.NewHolds(tc.rules, facts)
			if err != nil {
				t.Fatalf("NewHolds(): %v", err)
			}
			c := aCandidate()
			if tc.spoil != nil {
				tc.spoil(&c)
			}
			hold, err := holds.Evaluate(context.Background(), c)
			if err != nil {
				t.Fatalf("Evaluate(): %v", err)
			}
			if hold.Rule != tc.want {
				t.Fatalf("held by %q, want %q (reason %q)", hold.Rule, tc.want, hold.Reason)
			}
			if hold.Held() != (tc.want != "") {
				t.Errorf("Held() = %v with rule %q", hold.Held(), hold.Rule)
			}
			if tc.says != "" && !strings.Contains(hold.Reason, tc.says) {
				t.Errorf("reason = %q, want it to say %q", hold.Reason, tc.says)
			}
		})
	}
}

func TestARuleThatCannotBeAskedIsAnErrorNotAPass(t *testing.T) {
	t.Parallel()
	facts := ordinaryFacts()
	facts.fail = errors.New("connection reset")
	holds, err := earnings.NewHolds(everyRule, facts)
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	hold, err := holds.Evaluate(context.Background(), aCandidate())
	if !errors.Is(err, earnings.ErrHoldUnread) {
		t.Fatalf("Evaluate() = %v, want one wrapping ErrHoldUnread", err)
	}
	if hold.Held() {
		t.Error("an unanswered rule held the credit; it must decide nothing")
	}
}

func TestAMemberWithNoAccountCannotBeJudgedYoung(t *testing.T) {
	t.Parallel()
	facts := ordinaryFacts()
	facts.noAccount = true
	holds, err := earnings.NewHolds(earnings.HoldRules{NewAccountAge: time.Hour}, facts)
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	if _, err := holds.Evaluate(context.Background(), aCandidate()); !errors.Is(err, earnings.ErrHoldUnread) {
		t.Errorf("Evaluate() = %v, want one wrapping ErrHoldUnread", err)
	}
}

func TestRulesThatAreOffAreNotAsked(t *testing.T) {
	t.Parallel()
	facts := ordinaryFacts()
	holds, err := earnings.NewHolds(earnings.HoldRules{}, facts)
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	if _, err := holds.Evaluate(context.Background(), aCandidate()); err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if len(facts.asked) != 0 {
		t.Errorf("rules that are off asked the store %v", facts.asked)
	}
	if active := holds.Rules().Active(); len(active) != 0 {
		t.Errorf("Active() = %v, want none", active)
	}
}

func TestTheRulesRunInTheirDocumentedOrder(t *testing.T) {
	t.Parallel()
	want := []string{earnings.RuleSharedContext, earnings.RuleNewAccount, earnings.RuleSaleCap, earnings.RuleMemberVelocity}
	got := everyRule.Active()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Active() = %v, want %v", got, want)
	}
}

func TestACandidateNeedsAMemberAndAMoment(t *testing.T) {
	t.Parallel()
	holds, err := earnings.NewHolds(everyRule, ordinaryFacts())
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	nobody := aCandidate()
	nobody.Member = uuid.Nil
	if _, err := holds.Evaluate(context.Background(), nobody); !errors.Is(err, earnings.ErrNoCandidate) {
		t.Errorf("a candidate owed to nobody = %v, want ErrNoCandidate", err)
	}
	never := aCandidate()
	never.At = time.Time{}
	if _, err := holds.Evaluate(context.Background(), never); !errors.Is(err, earnings.ErrNoCandidate) {
		t.Errorf("a candidate with no moment = %v, want ErrNoCandidate", err)
	}
}

func TestHoldRulesAreRefusedHalfConfigured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rules earnings.HoldRules
		want  string
	}{
		{"a device count without its window", earnings.HoldRules{SharedContextAccounts: 3}, "needs both"},
		{"a device window without its count", earnings.HoldRules{SharedContextWindow: time.Hour}, "needs both"},
		{"a device count of one", earnings.HoldRules{SharedContextAccounts: 1, SharedContextWindow: time.Hour}, "would hold every click"},
		{"a velocity without its window", earnings.HoldRules{MemberVelocity: 2}, "needs both"},
		{"a negative count", earnings.HoldRules{MemberVelocity: -1, MemberVelocityWindow: time.Hour}, "negative"},
		{"a negative window", earnings.HoldRules{NewAccountAge: -time.Hour}, "negative"},
		{"a cap of nothing", earnings.HoldRules{SaleCap: money.Amount{Minor: 0, Currency: "EUR"}}, "positive amount"},
		{"a cap in no currency that is one", earnings.HoldRules{SaleCap: money.Amount{Minor: 1, Currency: "euro"}}, earnings.RuleSaleCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := earnings.NewHolds(tc.rules, ordinaryFacts())
			if !errors.Is(err, earnings.ErrBadHoldRules) {
				t.Fatalf("NewHolds() = %v, want one wrapping ErrBadHoldRules", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewHolds() = %q, want it to say %q", err, tc.want)
			}
		})
	}
	if _, err := earnings.NewHolds(everyRule, nil); !errors.Is(err, earnings.ErrNoHoldStore) {
		t.Errorf("NewHolds() with no store = %v, want ErrNoHoldStore", err)
	}
}

func TestAVerdictOpensTheCreditWhereItBelongs(t *testing.T) {
	t.Parallel()
	credit := earnings.Credit{Member: uuid.New(), Reason: "attributed by the poller"}

	held := earnings.Hold{Rule: earnings.RuleSaleCap, Reason: "the sale is at or above the cap"}.Open(credit)
	if held.State != earnings.StateHeld || held.HoldRule != earnings.RuleSaleCap || held.Reason != "the sale is at or above the cap" {
		t.Errorf("a held verdict opened %+v", held)
	}

	free := earnings.Hold{}.Open(credit)
	if free.State != earnings.StatePending || free.HoldRule != "" || free.Reason != "attributed by the poller" {
		t.Errorf("a clear verdict opened %+v", free)
	}
}
