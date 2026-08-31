package earnings_test

// What a commission divides into, and the identity that keeps a ledger
// balancing (T068, FR-040, C-1).
//
// The exactness property is asserted over a range rather than at a handful of
// chosen points, because the failure it guards against is not a wrong answer
// at one input: it is a minor unit disappearing at some rate nobody thought
// to write a case for, which shows up months later as a ledger that does not
// sum to zero.

import (
	"errors"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// eur is a commission in the currency the fixtures use.
func eur(t *testing.T, minor int64) money.Amount {
	t.Helper()
	amount, err := money.New(minor, "EUR")
	if err != nil {
		t.Fatalf("money.New(%d): %v", minor, err)
	}
	return amount
}

// promised is a click-time snapshot carrying the given member share.
func promised(bps money.BasisPoints) clickout.Promise {
	return clickout.Promise{MemberShare: bps}
}

// TestTheMemberAndTheRemainderAreAlwaysTheWholeCommission is C-1 at this
// layer. Every split is a transfer, and a transfer that does not sum to zero
// is a ledger that has stopped balancing - so the identity is asserted across
// every rate at several awkward amounts rather than at a few chosen points.
func TestTheMemberAndTheRemainderAreAlwaysTheWholeCommission(t *testing.T) {
	t.Parallel()

	// Primes and near-scale values: the amounts where a rate is least likely
	// to divide cleanly, plus a debit, because a reversal splits too.
	amounts := []int64{0, 1, 3, 7, 99, 101, 4999, 9999, 10001, 123457, -4999, -1}
	for _, minor := range amounts {
		commission := eur(t, minor)
		for bps := money.BasisPoints(0); bps <= money.BasisPointsScale; bps += 7 {
			share, err := earnings.ShareOf(commission, promised(bps))
			if err != nil {
				t.Fatalf("ShareOf(%d, %d bps): %v", minor, int32(bps), err)
			}
			total, err := share.Member.Add(share.Remainder)
			if err != nil {
				t.Fatalf("adding the two halves of %d at %d bps: %v", minor, int32(bps), err)
			}
			if total != commission {
				t.Fatalf("at %d bps, %d split into %d and %d, which is %d - a minor unit was lost",
					int32(bps), minor, share.Member.Minor, share.Remainder.Minor, total.Minor)
			}
		}
	}
}

// TestRoundingIsNeverMoreThanOneMinorUnit pins what the house absorbs. One
// minor unit is the whole of what a rounding step can move, so anything
// larger is the arithmetic having gone wrong somewhere the identity above
// cannot see.
func TestRoundingIsNeverMoreThanOneMinorUnit(t *testing.T) {
	t.Parallel()

	for _, minor := range []int64{0, 1, 3, 7, 99, 101, 4999, 9999, 123457, -4999} {
		commission := eur(t, minor)
		for bps := money.BasisPoints(1); bps < money.BasisPointsScale; bps += 13 {
			share, err := earnings.ShareOf(commission, promised(bps))
			if err != nil {
				t.Fatalf("ShareOf(%d, %d bps): %v", minor, int32(bps), err)
			}
			if share.Rounding.Minor < -1 || share.Rounding.Minor > 1 {
				t.Fatalf("at %d bps, splitting %d rounded by %d minor units, want at most one",
					int32(bps), minor, share.Rounding.Minor)
			}
		}
	}
}

// TestRoundingFavoursTheMemberOnBothSidesOfZero is the product promise. A
// credit rounds up so the member is never short a minor unit; a reversal
// rounds toward zero so a clawback never takes back more than was earned.
func TestRoundingFavoursTheMemberOnBothSidesOfZero(t *testing.T) {
	t.Parallel()

	// 101 at 5000 bps is exactly 50.5 - the awkward half that tells the
	// directions apart.
	credit, err := earnings.ShareOf(eur(t, 101), promised(5000))
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	if credit.Member.Minor != 51 {
		t.Errorf("a credit of 101 at half gave the member %d, want 51 - rounded up", credit.Member.Minor)
	}
	if credit.Rounding.Minor != 1 {
		t.Errorf("Rounding = %d, want 1 - what rounding up cost", credit.Rounding.Minor)
	}

	debit, err := earnings.ShareOf(eur(t, -101), promised(5000))
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	if debit.Member.Minor != -50 {
		t.Errorf("a reversal of -101 at half took %d from the member, want -50 - rounded toward zero", debit.Member.Minor)
	}
}

// TestNothingIsOwedAtNoShareAndEverythingAtTheWhole pins the two ends, which
// are the rates a misplaced comparison gets wrong: a member owed the entire
// commission, or owed nothing on every purchase.
func TestNothingIsOwedAtNoShareAndEverythingAtTheWhole(t *testing.T) {
	t.Parallel()

	none, err := earnings.ShareOf(eur(t, 4999), promised(0))
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	if !none.Member.IsZero() || none.Remainder.Minor != 4999 {
		t.Errorf("at no share the split was %d and %d, want 0 and 4999", none.Member.Minor, none.Remainder.Minor)
	}

	all, err := earnings.ShareOf(eur(t, 4999), promised(money.BasisPointsScale))
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	if all.Member.Minor != 4999 || !all.Remainder.IsZero() {
		t.Errorf("at the whole the split was %d and %d, want 4999 and 0", all.Member.Minor, all.Remainder.Minor)
	}
}

// TestASnapshotOutsideTheWholeIsRefused covers a row this code cannot write.
// The schema checks the column and clickout checks it again on the way out,
// so reaching it means a row predates a constraint or came from somewhere
// else - and paying against it would credit more than was earned.
func TestASnapshotOutsideTheWholeIsRefused(t *testing.T) {
	t.Parallel()

	for _, bps := range []money.BasisPoints{-1, money.BasisPointsScale + 1} {
		_, err := earnings.ShareOf(eur(t, 4999), promised(bps))
		if !errors.Is(err, earnings.ErrShareOutOfRange) {
			t.Errorf("ShareOf(_, %d bps) error = %v, want one wrapping %v",
				int32(bps), err, earnings.ErrShareOutOfRange)
		}
	}
}

// TestACommissionWithNoCurrencyIsRefused keeps a zero Amount from being read
// as zero euros. An amount without a currency is not money (C-6), and a
// credit posted against one is a figure in no ledger.
func TestACommissionWithNoCurrencyIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := earnings.ShareOf(money.Amount{Minor: 4999}, promised(6000)); err == nil {
		t.Error("ShareOf() divided an amount carrying no currency")
	}
}
