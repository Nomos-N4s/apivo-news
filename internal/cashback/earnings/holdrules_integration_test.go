package earnings_test

// A hold against the real schema and the real ledger (T121, US7): the rules
// read what the poller and the click-out actually wrote, a held credit
// counts toward nothing member-facing and cannot be confirmed, and releasing
// it records who and why before the ordinary lifecycle resumes.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	cataloguestore "github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// clickFrom issues a click for the member from the given device context,
// the way the click-out records one when the edge reports an address.
func (j *theJourney) clickFrom(t *testing.T, member uuid.UUID, context clickout.ContextDigest) clickout.Click {
	t.Helper()
	clicks, err := clickout.NewAnnouncedClicks(j.tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	clickouts, err := clickout.NewClickOuts(
		catalogue.NewOfferReader(cataloguestore.New(j.tx)), clicks, staticDeeplinks{})
	if err != nil {
		t.Fatalf("NewClickOuts(): %v", err)
	}
	issued, err := clickouts.Issue(j.ctx, clickout.Request{Member: member, OfferID: j.offer, Context: context})
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	return issued.Click
}

// anotherMember seeds a second reader account.
func (j *theJourney) anotherMember(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := j.tx.QueryRow(j.ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Second Member', 'reader') returning id`,
		"second-"+tag(t)+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the second member: %v", err)
	}
	return id
}

// covered imports an empty statement covering the purchase, so the only
// thing between the entry and confirmed is its state.
func (j *theJourney) covered(t *testing.T) {
	t.Helper()
	if _, err := j.tx.Exec(j.ctx, `
		insert into cashback.reconciliation_run
		    (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
		values ($1, now() - interval '2 days', now() + interval '1 day', $2, '{"lines":[]}'::jsonb)`,
		j.publisher, j.operator); err != nil {
		t.Fatalf("importing the covering statement: %v", err)
	}
}

// transitionRecorded reads who moved an entry into a state, and why.
func (j *theJourney) transitionRecorded(t *testing.T, entry uuid.UUID, to earnings.State) (actor uuid.UUID, reason string) {
	t.Helper()
	var actorID *uuid.UUID
	var recorded *string
	if err := j.tx.QueryRow(j.ctx, `
		select actor_id, reason from cashback.entry_transition
		 where entry_id = $1 and to_state = $2`, entry, string(to)).Scan(&actorID, &recorded); err != nil {
		t.Fatalf("reading the %s transition: %v", to, err)
	}
	if actorID != nil {
		actor = *actorID
	}
	if recorded != nil {
		reason = *recorded
	}
	return actor, reason
}

func TestAHeldCreditIsNeverCreditedUntilReleased(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)

	// One device, two accounts: the self-dealing shape. The other member
	// clicks first; then ours does, from the same context.
	device := clickout.NewContextDigest("198.51.100.7", "Mozilla/5.0 (same phone)")
	j.clickFrom(t, j.anotherMember(t), device)
	click := j.clickFrom(t, j.member, device)
	report := j.reports(t, click.Ref.Ref(), networks.StatusConfirmed)
	machine, confirmations := j.entries(t)
	queries := earningsstore.New(j.tx)

	// The rules read the schema, not a fake: two accounts share the context.
	holds, err := earnings.NewHolds(earnings.HoldRules{
		SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour,
	}, queries)
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	hold, err := holds.Evaluate(j.ctx, earnings.Candidate{
		Member: j.member, Click: click,
		Sale: money.Amount{Minor: reportedSaleMinor, Currency: "EUR"},
		At:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if hold.Rule != earnings.RuleSharedContext {
		t.Fatalf("held by %q, want %s: %s", hold.Rule, earnings.RuleSharedContext, hold.Reason)
	}

	share, err := earnings.ShareOf(money.Amount{Minor: reportedCommission, Currency: "EUR"}, click.Promised)
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	opened, err := machine.Open(j.ctx, j.tx, hold.Open(earnings.Credit{
		Member: j.member, Brand: "apivo-de", Report: report, Click: click.ID, Amount: share.Member,
	}))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if opened.State != earnings.StateHeld || opened.HoldRule != earnings.RuleSharedContext {
		t.Fatalf("the credit opened as %s under %q, want held under %s", opened.State, opened.HoldRule, earnings.RuleSharedContext)
	}
	if _, reason := j.transitionRecorded(t, opened.ID, earnings.StateHeld); reason != hold.Reason {
		t.Errorf("the opening transition recorded %q, want the rule's reason %q", reason, hold.Reason)
	}

	// Never credited: the money sits in the held stage and nowhere a member
	// or a withdrawal reads.
	if held, pending, confirmed := j.balance(t, wallet.StageHeld), j.balance(t, wallet.StagePending), j.balance(t, wallet.StageConfirmed); held != share.Member.Minor || pending != 0 || confirmed != 0 {
		t.Errorf("balances held=%d pending=%d confirmed=%d, want the share %d held and nothing elsewhere", held, pending, confirmed, share.Member.Minor)
	}
	if spendable, err := machine.Confirmed(j.ctx, queries, j.member, "EUR"); err != nil || len(spendable) != 0 {
		t.Errorf("a held credit counted as confirmed: %v (err %v)", spendable, err)
	}
	// Not even the network's own word confirms it: held goes only to pending.
	j.covered(t)
	if _, err := confirmations.Confirm(j.ctx, j.tx, opened, networks.StatusConfirmed, report); !errors.As(err, new(earnings.ErrIllegalTransition)) {
		t.Fatalf("confirming a held credit = %v, want ErrIllegalTransition", err)
	}

	// Released by a named human, for a reason, and only then ordinary.
	decision := uuid.New()
	released, err := machine.Apply(j.ctx, j.tx, earnings.Move{
		Entry: opened.ID, From: earnings.StateHeld, To: earnings.StatePending,
		Member: j.member, Amount: share.Member, Cause: decision,
		Reason: "the second account is the member's partner; reviewed", Actor: j.operator,
	})
	if err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if released.State != earnings.StatePending || released.HoldRule != "" {
		t.Errorf("released to %s under %q, want pending under no rule", released.State, released.HoldRule)
	}
	if actor, reason := j.transitionRecorded(t, opened.ID, earnings.StatePending); actor != j.operator || reason != "the second account is the member's partner; reviewed" {
		t.Errorf("the release recorded actor %s reason %q, want the operator %s and their reason", actor, reason, j.operator)
	}
	if held, pending := j.balance(t, wallet.StageHeld), j.balance(t, wallet.StagePending); held != 0 || pending != share.Member.Minor {
		t.Errorf("after release held=%d pending=%d, want 0 and %d", held, pending, share.Member.Minor)
	}
	confirmed, err := confirmations.Confirm(j.ctx, j.tx, released, networks.StatusConfirmed, report)
	if err != nil {
		t.Fatalf("confirming after release: %v", err)
	}
	if confirmed.State != earnings.StateConfirmed {
		t.Errorf("after release the credit is %s, want confirmed", confirmed.State)
	}
	j.wantZeroSum(t)
}

func TestANormalPatternPassesUntouched(t *testing.T) {
	t.Parallel()
	j := begin(t)
	j.seed(t)
	// An account a year old. Seeded rows are stamped with the transaction's
	// own now(), the same instant the click below gets, so without this the
	// member would be zero seconds old at their first click.
	if _, err := j.tx.Exec(j.ctx, `update public.account set created_at = now() - interval '365 days' where id = $1`, j.member); err != nil {
		t.Fatalf("ageing the member: %v", err)
	}
	click := j.clickFrom(t, j.member, clickout.NewContextDigest("192.0.2.44", "Mozilla/5.0 (own laptop)"))
	holds, err := earnings.NewHolds(earnings.HoldRules{
		SharedContextAccounts: 2, SharedContextWindow: 24 * time.Hour,
		NewAccountAge:  24 * time.Hour,
		SaleCap:        money.Amount{Minor: reportedSaleMinor + 1, Currency: "EUR"},
		MemberVelocity: 1, MemberVelocityWindow: 24 * time.Hour,
	}, earningsstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewHolds(): %v", err)
	}
	hold, err := holds.Evaluate(j.ctx, earnings.Candidate{
		Member: j.member, Click: click,
		Sale: money.Amount{Minor: reportedSaleMinor, Currency: "EUR"},
		At:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if hold.Held() {
		t.Errorf("an ordinary purchase was held by %s: %s", hold.Rule, hold.Reason)
	}
}
