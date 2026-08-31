package wallet_test

// What a member's wallet says, and what it refuses to say (T078).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakePayouts answers what a member has been paid, and records what it was
// asked - the currency above all, because asking in the wrong one would
// silently return a total that belongs to somebody else's money.
type fakePayouts struct {
	minor  int64
	err    error
	asked  []store.SettledPayoutsForParams
	member uuid.UUID
}

func (f *fakePayouts) SettledPayoutsFor(_ context.Context, arg store.SettledPayoutsForParams) (int64, error) {
	f.asked = append(f.asked, arg)
	if f.err != nil {
		return 0, f.err
	}
	if f.member != uuid.Nil && uuid.UUID(arg.AccountID.Bytes) != f.member {
		return 0, nil
	}
	return f.minor, nil
}

func wallets(t *testing.T, ledger wallet.Balances, payouts wallet.Payouts, threshold money.Amount) *wallet.Wallets {
	t.Helper()
	w, err := wallet.NewWallets(projector(t, ledger), payouts, threshold)
	if err != nil {
		t.Fatalf("NewWallets(): %v", err)
	}
	return w
}

// TestTheWalletIsAssembledFromBothKindsOfTruth: four balances from the
// ledger, one total from the payments, and the threshold the deployment
// stated - in one answer, all in one currency.
func TestTheWalletIsAssembledFromBothKindsOfTruth(t *testing.T) {
	t.Parallel()

	ledger := &fakeBalances{held: map[wallet.Stage]money.Amount{
		wallet.StageHeld:      {Minor: 100, Currency: "EUR"},
		wallet.StagePending:   {Minor: 200, Currency: "EUR"},
		wallet.StageConfirmed: {Minor: 300, Currency: "EUR"},
		wallet.StageReserved:  {Minor: 400, Currency: "EUR"},
	}}
	payouts := &fakePayouts{minor: 5000}
	threshold := money.Amount{Minor: 2000, Currency: "EUR"}

	got, err := wallets(t, ledger, payouts, threshold).Of(t.Context(), fakeMember)
	if err != nil {
		t.Fatalf("Of(): %v", err)
	}

	if got.Confirmed.Minor != 300 {
		t.Errorf("confirmed = %d, want 300", got.Confirmed.Minor)
	}
	if got.PaidOut.Minor != 5000 || got.PaidOut.Currency != "EUR" {
		t.Errorf("paid out = %v, want 5000 EUR", got.PaidOut)
	}
	if got.Threshold != threshold {
		t.Errorf("threshold = %v, want %v", got.Threshold, threshold)
	}
}

// TestThePayoutsAreAskedForInTheWalletsCurrency. Asked in another, the sum
// would be somebody else's money: currencies do not net against each other,
// so a member paid in zloty has been paid no euros (C-6).
func TestThePayoutsAreAskedForInTheWalletsCurrency(t *testing.T) {
	t.Parallel()

	payouts := &fakePayouts{}
	threshold := money.Amount{Minor: 500, Currency: "PLN"}

	if _, err := wallets(t, &fakeBalances{}, payouts, threshold).Of(t.Context(), fakeMember); err != nil {
		t.Fatalf("Of(): %v", err)
	}

	if len(payouts.asked) != 1 {
		t.Fatalf("asked for the payouts %d times, want once", len(payouts.asked))
	}
	if payouts.asked[0].Currency != "PLN" {
		t.Errorf("asked for payouts in %q, want PLN", payouts.asked[0].Currency)
	}
	if uuid.UUID(payouts.asked[0].AccountID.Bytes) != fakeMember {
		t.Errorf("asked about %v, want the member %s", uuid.UUID(payouts.asked[0].AccountID.Bytes), fakeMember)
	}
}

// TestAWalletWithNoThresholdRefusesRatherThanInvents. Every figure it would
// show depends on a currency, and the threshold is the only place the
// deployment has stated one.
func TestAWalletWithNoThresholdRefusesRatherThanInvents(t *testing.T) {
	t.Parallel()

	ledger, payouts := &fakeBalances{}, &fakePayouts{}

	_, err := wallets(t, ledger, payouts, money.Amount{}).Of(t.Context(), uuid.New())

	if !errors.Is(err, wallet.ErrNoThreshold) {
		t.Fatalf("Of() error = %v, want %v", err, wallet.ErrNoThreshold)
	}
	if len(ledger.named) != 0 || len(payouts.asked) != 0 {
		t.Error("a wallet with no threshold read the ledger and the payouts anyway")
	}
}

// TestAWalletIsRefusedWithoutItsParts. "You have been paid nothing" is a
// claim, and a wallet that could not check it would be making it anyway.
func TestAWalletIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	p := projector(t, &fakeBalances{})
	if _, err := wallet.NewWallets(nil, &fakePayouts{}, money.Amount{}); !errors.Is(err, wallet.ErrNoProjector) {
		t.Errorf("NewWallets(nil, _, _) error = %v, want %v", err, wallet.ErrNoProjector)
	}
	if _, err := wallet.NewWallets(p, nil, money.Amount{}); !errors.Is(err, wallet.ErrNoPayouts) {
		t.Errorf("NewWallets(_, nil, _) error = %v, want %v", err, wallet.ErrNoPayouts)
	}
}

// TestAWalletThatCannotReadThePayoutsIsNoWallet, for the reason a partial
// projection is no wallet: a member acts on what they are shown.
func TestAWalletThatCannotReadThePayoutsIsNoWallet(t *testing.T) {
	t.Parallel()

	payouts := &fakePayouts{err: errors.New("connection reset")}

	got, err := wallets(t, &fakeBalances{}, payouts, money.Amount{Minor: 1, Currency: "EUR"}).
		Of(t.Context(), uuid.New())

	if !errors.Is(err, wallet.ErrNotRead) {
		t.Fatalf("Of() error = %v, want one wrapping %v", err, wallet.ErrNotRead)
	}
	if (got != wallet.Wallet{}) {
		t.Errorf("a failed read returned %+v, want nothing", got)
	}
}
