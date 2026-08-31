package wallet_test

// What a member's totals must be, and what the projection must refuse
// (T077, FR-041, FR-050, SC-006).
//
// The property SC-006 asks for is an equality between two computations of
// the same money: what the wallet displays, and what somebody adding up the
// postings would get. So the sums here are built from the postings this file
// made, never from the projection's own answer - a test that asked the
// projection twice would agree with itself about anything.
//
// It runs against every ledger implementation, for the reason the
// conformance suite does: a wallet that is right over Postgres and wrong
// over Blnk is a wallet nobody can move.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakeBalances answers what it was told to, and can refuse.
type fakeBalances struct {
	held    map[wallet.Stage]money.Amount
	named   []wallet.AccountRef
	ensErr  error
	readErr error
	// readsBefore is how many balances the ledger answers before readErr
	// starts applying. It is what makes a partial-read case partial: a
	// ledger that fails on the FIRST stage leaves the totals at their zero
	// value, so returning them early would be indistinguishable from
	// returning nothing.
	readsBefore int
	reads       int
	// wrong makes the ledger answer one stage in another currency, which is
	// the shape of an implementation bug no type can catch.
	wrong wallet.Stage
}

func (f *fakeBalances) EnsureAccount(_ context.Context, ref wallet.AccountRef, _ money.Currency) (wallet.LedgerAccountID, error) {
	f.named = append(f.named, ref)
	if f.ensErr != nil {
		return "", f.ensErr
	}
	return wallet.LedgerAccountID("acct:" + ref.String()), nil
}

func (f *fakeBalances) Balance(_ context.Context, account wallet.LedgerAccountID, currency money.Currency) (money.Amount, error) {
	f.reads++
	if f.readErr != nil && f.reads > f.readsBefore {
		return money.Amount{}, f.readErr
	}
	for stage, held := range f.held {
		if account != wallet.LedgerAccountID("acct:"+wallet.MemberAccount(f.member(), stage).String()) {
			continue
		}
		if stage == f.wrong {
			return money.Amount{Minor: held.Minor, Currency: "GBP"}, nil
		}
		return money.Amount{Minor: held.Minor, Currency: currency}, nil
	}
	return money.Amount{Minor: 0, Currency: currency}, nil
}

// member is the one member this fake answers for; the projector only ever
// asks about one at a time.
func (f *fakeBalances) member() uuid.UUID { return fakeMember }

var fakeMember = uuid.New()

func projector(t *testing.T, ledger wallet.Balances) *wallet.Projector {
	t.Helper()
	p, err := wallet.NewProjector(ledger)
	if err != nil {
		t.Fatalf("NewProjector(): %v", err)
	}
	return p
}

// TestEachStageIsReadFromItsOwnAccount is the mapping, and it is worth its
// own case: three of the four totals differ only by which account they came
// from, so a transposition would leave every number plausible and the
// member's spendable balance wrong.
func TestEachStageIsReadFromItsOwnAccount(t *testing.T) {
	t.Parallel()

	ledger := &fakeBalances{held: map[wallet.Stage]money.Amount{
		wallet.StageHeld:      {Minor: 100, Currency: "EUR"},
		wallet.StagePending:   {Minor: 200, Currency: "EUR"},
		wallet.StageConfirmed: {Minor: 300, Currency: "EUR"},
		wallet.StageReserved:  {Minor: 400, Currency: "EUR"},
	}}

	totals, err := projector(t, ledger).Of(t.Context(), fakeMember, "EUR")
	if err != nil {
		t.Fatalf("Of(): %v", err)
	}

	for _, tc := range []struct {
		stage wallet.Stage
		got   money.Amount
		want  int64
	}{
		{wallet.StageHeld, totals.Held, 100},
		{wallet.StagePending, totals.Pending, 200},
		{wallet.StageConfirmed, totals.Confirmed, 300},
		{wallet.StageReserved, totals.Reserved, 400},
	} {
		if tc.got.Minor != tc.want {
			t.Errorf("%s = %d, want %d", tc.stage, tc.got.Minor, tc.want)
		}
		if tc.got.Currency != "EUR" {
			t.Errorf("%s came back in %s, want EUR", tc.stage, tc.got.Currency)
		}
	}
}

// TestAMemberWhoHasEarnedNothingReadsAsZeroes, not as four unknown-account
// failures. Everybody's wallet is empty before their first purchase, so the
// empty case is the ordinary one.
func TestAMemberWhoHasEarnedNothingReadsAsZeroes(t *testing.T) {
	t.Parallel()

	totals, err := projector(t, &fakeBalances{}).Of(t.Context(), uuid.New(), "EUR")
	if err != nil {
		t.Fatalf("Of(): %v", err)
	}

	claim, err := totals.Claim()
	if err != nil {
		t.Fatalf("Claim(): %v", err)
	}
	if claim.Minor != 0 {
		t.Errorf("a member who has earned nothing claims %d, want 0", claim.Minor)
	}
	// "Nothing, in euros" is a different statement from "nothing": a zero
	// with no currency renders against whatever symbol the page happens to
	// hold (C-6).
	if claim.Currency != "EUR" {
		t.Errorf("the empty claim came back in %q, want EUR", claim.Currency)
	}
}

// TestAProjectionIsRefusedWithoutItsParts covers the refusals that happen
// before the ledger is touched.
func TestAProjectionIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	if _, err := wallet.NewProjector(nil); !errors.Is(err, wallet.ErrNoLedgerToProject) {
		t.Errorf("NewProjector(nil) error = %v, want %v", err, wallet.ErrNoLedgerToProject)
	}

	ledger := &fakeBalances{}
	if _, err := projector(t, ledger).Of(t.Context(), uuid.Nil, "EUR"); !errors.Is(err, wallet.ErrNoMemberToProject) {
		t.Errorf("Of(nobody) error = %v, want %v", err, wallet.ErrNoMemberToProject)
	}
	if _, err := projector(t, ledger).Of(t.Context(), uuid.New(), "eur"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Of(bad currency) error = %v, want one wrapping %v", err, money.ErrInvalidCurrency)
	}
	if len(ledger.named) != 0 {
		t.Errorf("a refused projection named %d account(s), want none", len(ledger.named))
	}
}

// TestAPartialReadIsNoWallet. A total silently short by one stage is a
// number the member will act on, so nothing partial is returned.
func TestAPartialReadIsNoWallet(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		ledger *fakeBalances
	}{
		{"the account cannot be named", &fakeBalances{ensErr: errors.New("connection reset")}},
		{"the balance cannot be read", &fakeBalances{readErr: errors.New("connection reset")}},
		// The stage that fails is the THIRD, so two totals have already been
		// read when it does. A projection that returned what it had would
		// hand back a wallet missing confirmed and reserved - which is the
		// number a member acts on, silently short.
		{"a later stage cannot be read", &fakeBalances{
			held: map[wallet.Stage]money.Amount{
				wallet.StageHeld:    {Minor: 100, Currency: "EUR"},
				wallet.StagePending: {Minor: 200, Currency: "EUR"},
			},
			readErr:     errors.New("connection reset"),
			readsBefore: 2,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			totals, err := projector(t, tc.ledger).Of(t.Context(), fakeMember, "EUR")
			if !errors.Is(err, wallet.ErrNotProjected) {
				t.Fatalf("Of() error = %v, want one wrapping %v", err, wallet.ErrNotProjected)
			}
			if (totals != wallet.Totals{}) {
				t.Errorf("a failed projection returned %+v, want nothing", totals)
			}
		})
	}
}

// TestABalanceInAnotherCurrencyIsRefused. It is an implementation bug no
// type can catch, and the place to catch it is before the number reaches a
// member beside the wrong symbol (C-6).
func TestABalanceInAnotherCurrencyIsRefused(t *testing.T) {
	t.Parallel()

	ledger := &fakeBalances{
		held:  map[wallet.Stage]money.Amount{wallet.StageConfirmed: {Minor: 500, Currency: "EUR"}},
		wrong: wallet.StageConfirmed,
	}

	_, err := projector(t, ledger).Of(t.Context(), fakeMember, "EUR")

	if !errors.Is(err, wallet.ErrNotProjected) {
		t.Fatalf("Of() error = %v, want one wrapping %v", err, wallet.ErrNotProjected)
	}
}

// TestClaimRefusesToMixCurrencies. Adding euros to zloty needs a rate, a
// rate needs a moment, and a wallet is not the place to invent either.
func TestClaimRefusesToMixCurrencies(t *testing.T) {
	t.Parallel()

	mixed := wallet.Totals{
		Held:      money.Amount{Minor: 1, Currency: "EUR"},
		Pending:   money.Amount{Minor: 1, Currency: "PLN"},
		Confirmed: money.Amount{Minor: 1, Currency: "EUR"},
		Reserved:  money.Amount{Minor: 1, Currency: "EUR"},
	}

	if _, err := mixed.Claim(); !errors.Is(err, wallet.ErrNotProjected) {
		t.Fatalf("Claim() error = %v, want one wrapping %v", err, wallet.ErrNotProjected)
	}
}

// TestTheTotalsEqualTheSumOfThePostings is SC-006 itself, against every
// implementation.
//
// The member's money is moved the way the earnings path moves it - out of a
// house account into held, held to pending, pending to confirmed, confirmed
// to reserved - and the expected totals are accumulated from those transfers
// as they are posted. Nothing here asks the projection what it thinks.
func TestTheTotalsEqualTheSumOfThePostings(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		member := uuid.New()
		house := wallet.HouseAccount("projection-" + uuid.NewString())
		const eur = money.Currency("EUR")

		accounts := map[wallet.Stage]wallet.LedgerAccountID{}
		for _, stage := range []wallet.Stage{wallet.StageHeld, wallet.StagePending, wallet.StageConfirmed, wallet.StageReserved} {
			accounts[stage] = ensure(t, ledger, wallet.MemberAccount(member, stage), eur)
		}
		houseID := ensure(t, ledger, house, eur)

		// The independent tally: every posting this test makes, added up per
		// stage by hand.
		want := map[wallet.Stage]int64{}
		move := func(from, to wallet.LedgerAccountID, fromStage, toStage wallet.Stage, minor int64) {
			t.Helper()
			if _, err := ledger.Post(ctx, wallet.Transfer{
				IdempotencyKey: uuid.NewString(),
				Postings: []wallet.Posting{
					{Account: from, Amount: amount(-minor, eur)},
					{Account: to, Amount: amount(minor, eur)},
				},
			}); err != nil {
				t.Fatalf("posting %d from %s to %s: %v", minor, fromStage, toStage, err)
			}
			want[fromStage] -= minor
			want[toStage] += minor
		}

		// Credited, released, confirmed, and partly claimed by a withdrawal -
		// with an extra credit left held, so no two stages end up equal and a
		// transposition cannot hide.
		move(houseID, accounts[wallet.StageHeld], 0, wallet.StageHeld, 1000)
		move(houseID, accounts[wallet.StageHeld], 0, wallet.StageHeld, 250)
		move(accounts[wallet.StageHeld], accounts[wallet.StagePending], wallet.StageHeld, wallet.StagePending, 1000)
		move(accounts[wallet.StagePending], accounts[wallet.StageConfirmed], wallet.StagePending, wallet.StageConfirmed, 700)
		move(accounts[wallet.StageConfirmed], accounts[wallet.StageReserved], wallet.StageConfirmed, wallet.StageReserved, 400)

		totals, err := projector(t, ledger).Of(ctx, member, eur)
		if err != nil {
			t.Fatalf("Of(): %v", err)
		}

		for _, tc := range []struct {
			stage wallet.Stage
			got   money.Amount
		}{
			{wallet.StageHeld, totals.Held},
			{wallet.StagePending, totals.Pending},
			{wallet.StageConfirmed, totals.Confirmed},
			{wallet.StageReserved, totals.Reserved},
		} {
			if tc.got.Minor != want[tc.stage] {
				t.Errorf("%s = %d, but the postings sum to %d", tc.stage, tc.got.Minor, want[tc.stage])
			}
		}

		// And the whole claim is what left the house, to the minor unit: the
		// member's stages hold everything that was credited and nothing else.
		claim, err := totals.Claim()
		if err != nil {
			t.Fatalf("Claim(): %v", err)
		}
		if claim.Minor != 1250 {
			t.Errorf("the member claims %d, but %d was credited to them", claim.Minor, 1250)
		}
		if held := balance(t, ledger, houseID, eur); held.Minor != -1250 {
			t.Errorf("the house is out by %d, but %d was credited away from it", held.Minor, 1250)
		}
	})
}
