// Hold rules: the hard rules that keep a suspicious credit out of a member's
// pending balance until a human has looked (T118, US7).
//
// Alpha volume wants a review queue with hard rules, not a score (spec, US7).
// Each rule here asks one question about what already happened - how many
// accounts share the click's device context, how new the account is, how
// large the sale, how many credits the member has had lately - and answers
// with its own name, which is what the entry then carries (entry.hold_rule)
// and what the review queue shows an operator. A rule that is not configured
// does not run. The first rule that matches holds the credit: an entry names
// one rule, and the first is the one an operator most wants to know about,
// which is why the order below puts self-dealing first.
//
// What a hold IS: the credit opens in held rather than pending (open.go),
// with the money posted to the member's held stage. It counts toward nothing
// member-facing and cannot be confirmed - the state machine lets held go
// only to pending (state.go) - until an operator releases it, with their
// name and a reason (T119). A hold is never a refusal: the money is real and
// the network reported it; the question is only whose.
//
// What is NOT here: an excluded-category rule. The catalogue carries no
// category yet (#414), and a rule over a field nobody fills would hold
// nothing while looking like protection.

package earnings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The rules, by the name each writes into entry.hold_rule.
const (
	// RuleSharedContext holds a credit whose click came from a device
	// context other member accounts have clicked from too: the shape of one
	// person cycling purchases through many accounts.
	RuleSharedContext = "shared-context"
	// RuleNewAccount holds a credit earned by an account younger than the
	// configured age at the moment of the click.
	RuleNewAccount = "new-account"
	// RuleSaleCap holds a credit on a sale at or above the configured cap.
	RuleSaleCap = "sale-cap"
	// RuleMemberVelocity holds a credit for a member who has already been
	// credited the configured number of times within the window.
	RuleMemberVelocity = "member-velocity"
)

var (
	// ErrBadHoldRules reports a configuration the rules cannot run under: a
	// count without its window, a window without its count, or a negative
	// value. The text after it names the field.
	ErrBadHoldRules = errors.New("earnings: the hold rules cannot be read")
	// ErrNoHoldStore reports rules built with nothing to ask.
	ErrNoHoldStore = errors.New("earnings: hold rules need a store to read from")
	// ErrNoCandidate reports a credit the rules cannot judge: one owed to
	// nobody, or one with no instant to measure windows from.
	ErrNoCandidate = errors.New("earnings: a hold rule needs the member and the moment")
	// ErrHoldUnread reports a rule whose question the database did not
	// answer. Nothing is decided when it is returned: a credit is not opened
	// pending because a rule could not be asked.
	ErrHoldUnread = errors.New("earnings: a hold rule could not be evaluated")
)

// HoldRules is the configured set. A zero threshold turns its rule off;
// every window is required where its count is set, and refused where it is
// not, so a rule cannot be half-configured into holding nothing or
// everything.
type HoldRules struct {
	// SharedContextAccounts holds when at least this many DISTINCT member
	// accounts, this one included, clicked from the candidate's device
	// context within SharedContextWindow. 2 means one other account.
	SharedContextAccounts int
	SharedContextWindow   time.Duration
	// NewAccountAge holds when the member's account was younger than this at
	// the moment of the click.
	NewAccountAge time.Duration
	// SaleCap holds when the reported sale is at or above it. A sale in
	// another currency is not compared and is not held by this rule: a cap
	// is money, and money is only ever compared in its own currency (C-6).
	SaleCap money.Amount
	// MemberVelocity holds when the member has already been credited at
	// least this many times within MemberVelocityWindow.
	MemberVelocity       int
	MemberVelocityWindow time.Duration
}

// Validate refuses a set the rules cannot run under.
func (r HoldRules) Validate() error {
	switch {
	case r.SharedContextAccounts < 0 || r.MemberVelocity < 0:
		return fmt.Errorf("%w: a count is negative", ErrBadHoldRules)
	case r.SharedContextWindow < 0 || r.NewAccountAge < 0 || r.MemberVelocityWindow < 0:
		return fmt.Errorf("%w: a window is negative", ErrBadHoldRules)
	case r.SharedContextAccounts == 1:
		return fmt.Errorf("%w: %s at 1 would hold every click, since the member's own account is counted", ErrBadHoldRules, RuleSharedContext)
	case (r.SharedContextAccounts > 0) != (r.SharedContextWindow > 0):
		return fmt.Errorf("%w: %s needs both its account count and its window", ErrBadHoldRules, RuleSharedContext)
	case (r.MemberVelocity > 0) != (r.MemberVelocityWindow > 0):
		return fmt.Errorf("%w: %s needs both its count and its window", ErrBadHoldRules, RuleMemberVelocity)
	case r.SaleCap.Currency != "":
		if err := r.SaleCap.Validate(); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrBadHoldRules, RuleSaleCap, err)
		}
		if r.SaleCap.Minor <= 0 {
			return fmt.Errorf("%w: %s must be a positive amount", ErrBadHoldRules, RuleSaleCap)
		}
	}
	return nil
}

// Active lists the rules that will run, in the order they run.
func (r HoldRules) Active() []string {
	var active []string
	if r.SharedContextAccounts > 0 {
		active = append(active, RuleSharedContext)
	}
	if r.NewAccountAge > 0 {
		active = append(active, RuleNewAccount)
	}
	if r.SaleCap.Currency != "" {
		active = append(active, RuleSaleCap)
	}
	if r.MemberVelocity > 0 {
		active = append(active, RuleMemberVelocity)
	}
	return active
}

// Candidate is a credit as the rules see it, before it opens.
type Candidate struct {
	// Member is who would be credited.
	Member uuid.UUID
	// Click is the click the report was attributed to. Its Context is what
	// the shared-context rule reads and its ClickedAt is when the account's
	// age is measured; a credit an operator attributed by hand has neither,
	// and those rules then judge what they can.
	Click clickout.Click
	// Sale is what the member spent, as the network reported it.
	Sale money.Amount
	// At is when the rules are asked; every window is measured back from it.
	At time.Time
}

// HoldStore is the reads the rules need, named here per the boundary rules.
// *store.Queries satisfies it.
type HoldStore interface {
	AccountsSharingContext(ctx context.Context, arg store.AccountsSharingContextParams) (int64, error)
	MemberCreditsSince(ctx context.Context, arg store.MemberCreditsSinceParams) (int64, error)
	AccountCreatedAt(ctx context.Context, id pgtype.UUID) (pgtype.Timestamptz, error)
}

// Hold is a verdict: which rule held the credit and why, in words an
// operator reads from the queue. The zero value is "no rule matched".
type Hold struct {
	Rule   string
	Reason string
}

// Held reports whether a rule matched.
func (h Hold) Held() bool { return h.Rule != "" }

// Open applies the verdict to a credit about to open: held, naming the rule
// and carrying the reason, where a rule matched; pending otherwise. The
// credit's own reason is kept where nothing held it.
func (h Hold) Open(credit Credit) Credit {
	if !h.Held() {
		credit.State = StatePending
		credit.HoldRule = ""
		return credit
	}
	credit.State = StateHeld
	credit.HoldRule = h.Rule
	credit.Reason = h.Reason
	return credit
}

// Holds evaluates the configured rules against a candidate.
type Holds struct {
	rules HoldRules
	store HoldStore
}

// NewHolds builds the evaluator, refusing rules it cannot run under and a
// missing store.
func NewHolds(rules HoldRules, s HoldStore) (*Holds, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNoHoldStore
	}
	return &Holds{rules: rules, store: s}, nil
}

// Rules is the configured set.
func (h *Holds) Rules() HoldRules { return h.rules }

// Evaluate asks each active rule in order and answers the first that holds
// the candidate, or the zero Hold when none does.
//
// A rule that cannot be asked is an error, never a pass: a credit opened
// pending because the database was unreachable for one query would be
// exactly the credit the rule exists to catch, slipping through on a bad
// day. The caller decides what to do with an unjudged credit; this does not
// decide for it.
func (h *Holds) Evaluate(ctx context.Context, c Candidate) (Hold, error) {
	if c.Member == uuid.Nil || c.At.IsZero() {
		return Hold{}, ErrNoCandidate
	}
	if held, err := h.sharedContext(ctx, c); err != nil || held.Held() {
		return held, err
	}
	if held, err := h.newAccount(ctx, c); err != nil || held.Held() {
		return held, err
	}
	if held := h.saleCap(c); held.Held() {
		return held, nil
	}
	return h.memberVelocity(ctx, c)
}

// sharedContext is the self-dealing rule. A candidate whose click recorded
// no context cannot be judged by it - the digest is nullable (FR-022) - and
// is not held by it either: absence of evidence is not evidence.
func (h *Holds) sharedContext(ctx context.Context, c Candidate) (Hold, error) {
	if h.rules.SharedContextAccounts == 0 || !c.Click.Context.Recorded() {
		return Hold{}, nil
	}
	since := c.At.Add(-h.rules.SharedContextWindow)
	accounts, err := h.store.AccountsSharingContext(ctx, store.AccountsSharingContextParams{
		ContextDigest: pgtype.Text{String: c.Click.Context.String(), Valid: true},
		Since:         pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return Hold{}, fmt.Errorf("%w: %s: %w", ErrHoldUnread, RuleSharedContext, err)
	}
	if accounts < int64(h.rules.SharedContextAccounts) {
		return Hold{}, nil
	}
	return Hold{
		Rule: RuleSharedContext,
		Reason: fmt.Sprintf("%d member accounts clicked from this device context in the last %s; the rule holds at %d",
			accounts, h.rules.SharedContextWindow, h.rules.SharedContextAccounts),
	}, nil
}

// newAccount holds a credit earned by an account younger than the
// configured age at the click. Measured at the click where there is one,
// because that is when the purchase happened; at the evaluation otherwise.
func (h *Holds) newAccount(ctx context.Context, c Candidate) (Hold, error) {
	if h.rules.NewAccountAge == 0 {
		return Hold{}, nil
	}
	created, err := h.store.AccountCreatedAt(ctx, pgtype.UUID{Bytes: c.Member, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Hold{}, fmt.Errorf("%w: %s: member %s has no account", ErrHoldUnread, RuleNewAccount, c.Member)
	case err != nil:
		return Hold{}, fmt.Errorf("%w: %s: %w", ErrHoldUnread, RuleNewAccount, err)
	}
	at := c.Click.ClickedAt
	if at.IsZero() {
		at = c.At
	}
	age := at.Sub(created.Time)
	if age >= h.rules.NewAccountAge {
		return Hold{}, nil
	}
	return Hold{
		Rule: RuleNewAccount,
		Reason: fmt.Sprintf("the account was %s old at the click; the rule holds accounts younger than %s",
			age.Round(time.Minute), h.rules.NewAccountAge),
	}, nil
}

// saleCap holds a sale at or above the cap, in the cap's currency only.
func (h *Holds) saleCap(c Candidate) Hold {
	ceiling := h.rules.SaleCap
	if ceiling.Currency == "" || c.Sale.Currency != ceiling.Currency || c.Sale.Minor < ceiling.Minor {
		return Hold{}
	}
	return Hold{
		Rule:   RuleSaleCap,
		Reason: fmt.Sprintf("the sale of %s is at or above the cap of %s", c.Sale, ceiling),
	}
}

// memberVelocity holds a member already credited the configured number of
// times within the window.
func (h *Holds) memberVelocity(ctx context.Context, c Candidate) (Hold, error) {
	if h.rules.MemberVelocity == 0 {
		return Hold{}, nil
	}
	since := c.At.Add(-h.rules.MemberVelocityWindow)
	credits, err := h.store.MemberCreditsSince(ctx, store.MemberCreditsSinceParams{
		AccountID: pgtype.UUID{Bytes: c.Member, Valid: true},
		Since:     pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return Hold{}, fmt.Errorf("%w: %s: %w", ErrHoldUnread, RuleMemberVelocity, err)
	}
	if credits < int64(h.rules.MemberVelocity) {
		return Hold{}, nil
	}
	return Hold{
		Rule: RuleMemberVelocity,
		Reason: fmt.Sprintf("the member has been credited %d times in the last %s; the rule holds at %d",
			credits, h.rules.MemberVelocityWindow, h.rules.MemberVelocity),
	}, nil
}
