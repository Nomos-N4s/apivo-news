package wallet_test

// SC-006, as a contract rather than a claim: what the wallet shows a member
// equals what the ledger actually holds for them (T084).
//
// The word that matters is INDEPENDENTLY. The projection reads Balance, so a
// test that also read Balance would prove only that one function agrees with
// itself - and a balance is exactly the kind of number that drifts: a cached
// column somebody forgot to update, a Blnk aggregate computed from an
// indexed view, a Postgres sum over a table a migration reshaped. So every
// figure here is recomputed from the POSTINGS, through History, and compared
// against what the member would be shown.
//
// Two ways of arriving at one number, over all three implementations of the
// port. That is the whole of it, and it is what US3's checkpoint - "the
// member can see, and what they see is true" - actually rests on.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// totalsEUR is the currency every scenario here posts in.
const totalsEUR = money.Currency("EUR")

// everyStage is the four buckets a member's money can sit in, in the order
// the projection reads them.
var everyStage = []wallet.Stage{
	wallet.StageHeld,
	wallet.StagePending,
	wallet.StageConfirmed,
	wallet.StageReserved,
}

// summedFromPostings recomputes one member's totals WITHOUT reading a
// balance: it walks each stage account's history and adds the postings up.
//
// This is the independent half of SC-006. It deliberately does the arithmetic
// the long way - money.Sum over every posting, one stage at a time - because
// the point is to arrive at the figure by a different route than the one
// under test, not by a faster one.
func summedFromPostings(t *testing.T, ledger wallet.Ledger, member uuid.UUID) wallet.Totals {
	t.Helper()
	var (
		totals wallet.Totals
		into   = map[wallet.Stage]*money.Amount{
			wallet.StageHeld:      &totals.Held,
			wallet.StagePending:   &totals.Pending,
			wallet.StageConfirmed: &totals.Confirmed,
			wallet.StageReserved:  &totals.Reserved,
		}
	)
	for _, stage := range everyStage {
		account := ensure(t, ledger, wallet.MemberAccount(member, stage), totalsEUR)
		history, err := ledger.History(context.Background(), account, wallet.Window{})
		if err != nil {
			t.Fatalf("reading %s's %v history: %v", member, stage, err)
		}
		// Starts at zero IN THE CURRENCY, not at the zero Amount: an
		// account with no postings must read as "nothing, in euros" rather
		// than as "nothing", which is what the projection answers and what
		// C-6 requires of every figure a member is shown.
		running := money.Amount{Minor: 0, Currency: totalsEUR}
		for posting, err := range history {
			if err != nil {
				t.Fatalf("reading %s's %v history: %v", member, stage, err)
			}
			running, err = money.Sum(running, posting.Amount)
			if err != nil {
				t.Fatalf("summing %s's %v postings: %v", member, stage, err)
			}
		}
		*into[stage] = running
	}
	return totals
}

// projected answers what the member would actually be shown.
func projected(t *testing.T, ledger wallet.Ledger, member uuid.UUID) wallet.Totals {
	t.Helper()
	projector, err := wallet.NewProjector(ledger)
	if err != nil {
		t.Fatalf("NewProjector(): %v", err)
	}
	totals, err := projector.Of(context.Background(), member, totalsEUR)
	if err != nil {
		t.Fatalf("projecting %s: %v", member, err)
	}
	return totals
}

// mustAgree is the assertion SC-006 reduces to.
func mustAgree(t *testing.T, ledger wallet.Ledger, member uuid.UUID, what string) {
	t.Helper()
	shown, summed := projected(t, ledger, member), summedFromPostings(t, ledger, member)
	for _, stage := range everyStage {
		a, b := stageOf(shown, stage), stageOf(summed, stage)
		if !a.Equal(b) {
			t.Errorf("%s: %s's %v total is %v but their postings add up to %v",
				what, member, stage, a, b)
		}
	}
	// And the claim, which is the one figure combining all four: an error
	// that cancelled between two stages would pass every check above.
	shownClaim, err := shown.Claim()
	if err != nil {
		t.Fatalf("claiming the shown totals: %v", err)
	}
	summedClaim, err := summed.Claim()
	if err != nil {
		t.Fatalf("claiming the summed totals: %v", err)
	}
	if !shownClaim.Equal(summedClaim) {
		t.Errorf("%s: %s is shown a claim of %v against postings of %v", what, member, shownClaim, summedClaim)
	}
}

// stageOf reads one bucket out of a Totals.
func stageOf(totals wallet.Totals, stage wallet.Stage) money.Amount {
	switch stage {
	case wallet.StageHeld:
		return totals.Held
	case wallet.StagePending:
		return totals.Pending
	case wallet.StageConfirmed:
		return totals.Confirmed
	case wallet.StageReserved:
		return totals.Reserved
	}
	return money.Amount{}
}

// credit moves minor units from the house into one of a member's stages -
// an earning, in the shape every earning takes.
func credit(t *testing.T, ledger wallet.Ledger, member uuid.UUID, stage wallet.Stage, minor int64) {
	t.Helper()
	house := ensure(t, ledger, conformHouse(), totalsEUR)
	into := ensure(t, ledger, wallet.MemberAccount(member, stage), totalsEUR)
	post(t, ledger, "credit", []wallet.Posting{
		{Account: house, Amount: amount(-minor, totalsEUR)},
		{Account: into, Amount: amount(minor, totalsEUR)},
	})
}

// advance moves money between two of one member's own stages, which is what
// every confirmation and every reservation actually is (D7).
func advance(t *testing.T, ledger wallet.Ledger, member uuid.UUID, from, to wallet.Stage, minor int64) {
	t.Helper()
	out := ensure(t, ledger, wallet.MemberAccount(member, from), totalsEUR)
	in := ensure(t, ledger, wallet.MemberAccount(member, to), totalsEUR)
	post(t, ledger, "advance", []wallet.Posting{
		{Account: out, Amount: amount(-minor, totalsEUR)},
		{Account: in, Amount: amount(minor, totalsEUR)},
	})
}

// claw takes money back out of a stage to the house - a reversal, which is a
// posting of its own rather than an erasure of the credit.
func claw(t *testing.T, ledger wallet.Ledger, member uuid.UUID, stage wallet.Stage, minor int64) {
	t.Helper()
	house := ensure(t, ledger, conformHouse(), totalsEUR)
	from := ensure(t, ledger, wallet.MemberAccount(member, stage), totalsEUR)
	post(t, ledger, "reversal", []wallet.Posting{
		{Account: from, Amount: amount(-minor, totalsEUR)},
		{Account: house, Amount: amount(minor, totalsEUR)},
	})
}

// post writes one transfer under a key nothing else uses.
func post(t *testing.T, ledger wallet.Ledger, why string, postings []wallet.Posting) {
	t.Helper()
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "totals-" + why + "-" + uuid.NewString(),
		Reference:      why,
		Postings:       postings,
	}); err != nil {
		t.Fatalf("posting a %s: %v", why, err)
	}
}

// TestTotalsEqualTheSumOfThePostings is SC-006 itself, over a history with
// something in every stage.
//
// The amounts are deliberately awkward - not round, not equal, not in any
// arithmetic relation - so a projection that read the wrong account or added
// two stages together would produce a figure that could not be mistaken for
// the right one.
func TestTotalsEqualTheSumOfThePostings(t *testing.T) {
	t.Parallel()
	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		member := uuid.New()

		// A member's money arrives held, is released to pending, confirms,
		// and part of it is reserved against a withdrawal - which is the
		// whole lifecycle, and leaves all four stages non-zero.
		credit(t, ledger, member, wallet.StageHeld, 1_733)
		credit(t, ledger, member, wallet.StagePending, 4_099)
		advance(t, ledger, member, wallet.StageHeld, wallet.StagePending, 700)
		advance(t, ledger, member, wallet.StagePending, wallet.StageConfirmed, 2_500)
		advance(t, ledger, member, wallet.StageConfirmed, wallet.StageReserved, 1_100)

		mustAgree(t, ledger, member, "a member with money in every stage")

		// And the figures are the ones arithmetic says, so a projection that
		// agreed with a history that was ALSO wrong would still be caught.
		shown := projected(t, ledger, member)
		for _, want := range []struct {
			stage wallet.Stage
			minor int64
		}{
			{wallet.StageHeld, 1_733 - 700},
			{wallet.StagePending, 4_099 + 700 - 2_500},
			{wallet.StageConfirmed, 2_500 - 1_100},
			{wallet.StageReserved, 1_100},
		} {
			if got := stageOf(shown, want.stage); !got.Equal(amount(want.minor, totalsEUR)) {
				t.Errorf("%v = %v, want %d minor units", want.stage, got, want.minor)
			}
		}
	})
}

// A reversal is a posting, not an erasure (C-3). The credit and the clawback
// both stay in the history, and the total is what they add up to - which is
// the case a projection that "removed" a reversed earning would get right by
// accident and a history that hid one would get wrong.
func TestAReversedEarningNetsOutWithoutHidingEitherPosting(t *testing.T) {
	t.Parallel()
	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		member := uuid.New()
		credit(t, ledger, member, wallet.StagePending, 3_300)
		claw(t, ledger, member, wallet.StagePending, 1_250)

		mustAgree(t, ledger, member, "a member whose earning was partly reversed")

		if got := projected(t, ledger, member).Pending; !got.Equal(amount(3_300-1_250, totalsEUR)) {
			t.Errorf("pending = %v, want what the two postings add up to", got)
		}

		// Both postings survive, so an auditor can see the money went and
		// came back rather than inferring it from a smaller number.
		account := ensure(t, ledger, wallet.MemberAccount(member, wallet.StagePending), totalsEUR)
		history, err := ledger.History(context.Background(), account, wallet.Window{})
		if err != nil {
			t.Fatalf("reading the history: %v", err)
		}
		seen := 0
		for _, err := range history {
			if err != nil {
				t.Fatalf("reading the history: %v", err)
			}
			seen++
		}
		if seen != 2 {
			t.Errorf("the stage holds %d postings after a credit and a reversal, want both", seen)
		}
	})
}

// "For every member" is the half of SC-006 that a single-member test cannot
// reach: an account reference derived from the wrong uuid, or a query
// missing its member predicate, shows one member another's money. Three
// members with deliberately different histories, each checked against their
// own postings.
func TestEveryMembersTotalsAreTheirOwn(t *testing.T) {
	t.Parallel()
	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		var members []uuid.UUID
		for i, minor := range []int64{9_100, 250, 41_777} {
			member := uuid.New()
			members = append(members, member)
			credit(t, ledger, member, wallet.StagePending, minor)
			// Different shapes as well as different amounts, so a mix-up
			// shows in a stage and not only in a figure.
			if i%2 == 0 {
				advance(t, ledger, member, wallet.StagePending, wallet.StageConfirmed, minor/2)
			}
		}
		for _, member := range members {
			mustAgree(t, ledger, member, "one of several members")
		}

		// And explicitly: the first member's confirmed total is not the
		// third's, which is what a shared or truncated account reference
		// would make it.
		first, third := projected(t, ledger, members[0]), projected(t, ledger, members[2])
		if first.Confirmed.Equal(third.Confirmed) {
			t.Errorf("two members with different histories share a confirmed total of %v", first.Confirmed)
		}
	})
}

// A member who has never earned reads as four zeroes IN THE CURRENCY, and
// their postings add up to the same. "Nothing, in euros" is a different
// statement from "nothing", and it is the one a wallet has to make.
func TestAMemberWhoHasEarnedNothingAgreesWithTheirEmptyHistory(t *testing.T) {
	t.Parallel()
	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		member := uuid.New()

		mustAgree(t, ledger, member, "a member who has never earned")

		shown := projected(t, ledger, member)
		for _, stage := range everyStage {
			got := stageOf(shown, stage)
			if got.Minor != 0 {
				t.Errorf("%v = %v for a member who has never earned", stage, got)
			}
			if got.Currency != totalsEUR {
				t.Errorf("%v came back in %q, want the currency that was asked for", stage, got.Currency)
			}
		}
	})
}

// The projection is asked for ONE currency and must sum only that one.
// Adding euros to zloty needs a rate, a rate needs a moment, and a wallet is
// not the place to invent either (C-6) - so a second currency in a member's
// history must leave the euro totals untouched.
func TestASecondCurrencyDoesNotEnterTheTotals(t *testing.T) {
	t.Parallel()
	const otherCurrency = money.Currency("PLN")
	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		member := uuid.New()
		credit(t, ledger, member, wallet.StagePending, 5_000)

		// The same member, the same stage, another currency.
		house := ensure(t, ledger, conformHouse(), otherCurrency)
		into := ensure(t, ledger, wallet.MemberAccount(member, wallet.StagePending), otherCurrency)
		post(t, ledger, "credit", []wallet.Posting{
			{Account: house, Amount: amount(-70_000, otherCurrency)},
			{Account: into, Amount: amount(70_000, otherCurrency)},
		})

		if got := projected(t, ledger, member).Pending; !got.Equal(amount(5_000, totalsEUR)) {
			t.Errorf("pending = %v, want only the euro postings", got)
		}
		mustAgree(t, ledger, member, "a member holding two currencies")
	})
}
