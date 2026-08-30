package wallet_test

// Tests for the configured house accounts (T047). They run against the
// in-memory ledger because the properties under test - two purposes are
// two accounts, resolution is idempotent, misconfiguration is refused
// before any account exists - are properties of the layer above the port,
// and the port's own behaviour is already held identical across
// implementations by the conformance suite.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// houseNames is a well-formed configuration the refusal cases below break
// one rule at a time. The names deliberately share a prefix: alike
// spellings are exactly what a configuration typo produces, and nothing
// here may rely on the names looking different.
func houseNames() wallet.HouseNames {
	return wallet.HouseNames{
		RoundingRemainder: "house-rounding",
		ClawbackLoss:      "house-clawback",
	}
}

func TestNewHouseAccountsRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   wallet.HouseNames
		// want is the sentinel the refusal must wrap, so callers can
		// tell a name that names nothing from two purposes fighting
		// over one name.
		want error
		// names are fragments the message must carry: the operator
		// fixes a configuration key, so the error has to say which
		// purpose is broken, not merely that one is.
		names []string
	}{
		{
			name:  "an empty rounding remainder name",
			in:    wallet.HouseNames{RoundingRemainder: "", ClawbackLoss: "house-clawback"},
			want:  wallet.ErrInvalidAccountRef,
			names: []string{"rounding remainder"},
		},
		{
			name:  "an empty clawback loss name",
			in:    wallet.HouseNames{RoundingRemainder: "house-rounding", ClawbackLoss: ""},
			want:  wallet.ErrInvalidAccountRef,
			names: []string{"clawback loss"},
		},
		{
			// Padding is refused rather than trimmed for the same reason
			// HouseAccount documents: an account that exists under
			// " house-rounding" beside "house-rounding" is one balance
			// split invisibly in two.
			name:  "a padded name",
			in:    wallet.HouseNames{RoundingRemainder: " house-rounding", ClawbackLoss: "house-clawback"},
			want:  wallet.ErrInvalidAccountRef,
			names: []string{"rounding remainder"},
		},
		{
			// The dangerous misconfiguration: both purposes on one name
			// would hand D6's remainder and Q3's absorbed losses to a
			// single balance, and no later query could pull the two
			// figures apart. Each ref alone is valid - only this layer
			// can see the collision, so only this layer can refuse it.
			name:  "two purposes on one name",
			in:    wallet.HouseNames{RoundingRemainder: "house-shared", ClawbackLoss: "house-shared"},
			want:  wallet.ErrHouseNameShared,
			names: []string{"rounding remainder", "clawback loss", `"house-shared"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := wallet.NewHouseAccounts(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewHouseAccounts(%+v) error = %v, want %v", tt.in, err, tt.want)
			}
			for _, fragment := range tt.names {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error %q does not name %q", err, fragment)
				}
			}
		})
	}
}

// TestHouseAccountsCarryTheConfiguredNames pins the seam to the port: each
// accessor returns a house reference carrying its own configured name, so
// what EnsureAccount is eventually handed is exactly what configuration
// said and nothing this file invented.
func TestHouseAccountsCarryTheConfiguredNames(t *testing.T) {
	t.Parallel()

	houses, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() error: %v", err)
	}

	tests := []struct {
		purpose string
		ref     wallet.AccountRef
		want    string
	}{
		{"RoundingRemainder", houses.RoundingRemainder(), "house-rounding"},
		{"ClawbackLoss", houses.ClawbackLoss(), "house-clawback"},
	}
	for _, tt := range tests {
		name, ok := tt.ref.House()
		if !ok {
			t.Fatalf("%s() = %s, want a house reference", tt.purpose, tt.ref)
		}
		if name != tt.want {
			t.Errorf("%s() carries name %q, want %q", tt.purpose, name, tt.want)
		}
	}
}

// TestHouseAccountsResolveToTwoAccounts proves the property the whole file
// exists for: two purposes are two ledger accounts. The proof is the
// port's own invariant rather than a string comparison - money is posted
// from one to the other, and a transfer whose source and destination had
// collapsed onto one account balances by moving nothing, which every
// ledger refuses as ErrNoMovement before recording anything.
func TestHouseAccountsResolveToTwoAccounts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ledger := memory.New()
	houses, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() error: %v", err)
	}

	ids, err := houses.EnsureAll(ctx, ledger, eur)
	if err != nil {
		t.Fatalf("EnsureAll() error: %v", err)
	}
	if ids.RoundingRemainder == ids.ClawbackLoss {
		t.Fatalf("both purposes resolved to %q", ids.RoundingRemainder)
	}
	// Distinct is not enough: a consistent swap - the rounding id in the
	// clawback field and vice versa - keeps the two ids distinct, stable
	// and per-currency, and every posting made through the swapped fields
	// balances just as well. EnsureAccount is idempotent, so re-ensuring
	// each purpose's own ref directly says which id belongs to which
	// field, and that is the seam a swap cannot survive.
	if want, err := ledger.EnsureAccount(ctx, houses.RoundingRemainder(), eur); err != nil {
		t.Fatalf("re-ensuring the rounding remainder: %v", err)
	} else if ids.RoundingRemainder != want {
		t.Fatalf("ids.RoundingRemainder = %q, but the rounding remainder ref resolves to %q", ids.RoundingRemainder, want)
	}
	if want, err := ledger.EnsureAccount(ctx, houses.ClawbackLoss(), eur); err != nil {
		t.Fatalf("re-ensuring the clawback loss: %v", err)
	} else if ids.ClawbackLoss != want {
		t.Fatalf("ids.ClawbackLoss = %q, but the clawback loss ref resolves to %q", ids.ClawbackLoss, want)
	}

	if _, err := ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: "house-two-accounts",
		Postings: []wallet.Posting{
			{Account: ids.RoundingRemainder, Amount: amt(-100, eur)},
			{Account: ids.ClawbackLoss, Amount: amt(100, eur)},
		},
	}); err != nil {
		t.Fatalf("posting between the two house accounts: %v", err)
	}
	if got := balance(t, ledger, ids.RoundingRemainder, eur); got.Minor != -100 {
		t.Errorf("rounding remainder holds %v, want -100 EUR", got)
	}
	if got := balance(t, ledger, ids.ClawbackLoss, eur); got.Minor != 100 {
		t.Errorf("clawback loss holds %v, want 100 EUR", got)
	}
}

// TestHouseAccountsEnsureAllIsIdempotent holds EnsureAll to being safe on
// every startup: re-resolving - from the same value or a fresh one built
// from the same configuration, the shape a process restart takes - yields
// the same ids and touches no balance a previous run accrued.
func TestHouseAccountsEnsureAllIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ledger := memory.New()
	houses, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() error: %v", err)
	}

	first, err := houses.EnsureAll(ctx, ledger, eur)
	if err != nil {
		t.Fatalf("EnsureAll() error: %v", err)
	}
	// Money accrues between the two resolutions, so a re-resolution that
	// quietly opened fresh accounts would be caught by the balance below
	// rather than passing on ids alone.
	if _, err := ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: "house-idempotent-remainder",
		Postings: []wallet.Posting{
			{Account: first.RoundingRemainder, Amount: amt(3, eur)},
			{Account: first.ClawbackLoss, Amount: amt(-3, eur)},
		},
	}); err != nil {
		t.Fatalf("accruing a remainder: %v", err)
	}

	again, err := houses.EnsureAll(ctx, ledger, eur)
	if err != nil {
		t.Fatalf("EnsureAll() again: %v", err)
	}
	restarted, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() after restart: %v", err)
	}
	afterRestart, err := restarted.EnsureAll(ctx, ledger, eur)
	if err != nil {
		t.Fatalf("EnsureAll() after restart: %v", err)
	}
	if again != first || afterRestart != first {
		t.Fatalf("EnsureAll() resolved %+v, then %+v, then %+v; want one set of ids", first, again, afterRestart)
	}
	if got := balance(t, ledger, first.RoundingRemainder, eur); got.Minor != 3 {
		t.Errorf("the accrued remainder reads %v after re-resolution, want 3 EUR", got)
	}
}

// TestHouseAccountsEnsureAllPerCurrency pins the C-6 shape: one call
// resolves one currency, and a deployment trading in two holds two whole
// sets of accounts - never one account asked to mean two currencies.
func TestHouseAccountsEnsureAllPerCurrency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ledger := memory.New()
	houses, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() error: %v", err)
	}

	inEUR, err := houses.EnsureAll(ctx, ledger, eur)
	if err != nil {
		t.Fatalf("EnsureAll(EUR) error: %v", err)
	}
	inGBP, err := houses.EnsureAll(ctx, ledger, gbp)
	if err != nil {
		t.Fatalf("EnsureAll(GBP) error: %v", err)
	}
	if inEUR.RoundingRemainder == inGBP.RoundingRemainder {
		t.Errorf("the rounding remainder resolved to %q in both currencies", inEUR.RoundingRemainder)
	}
	if inEUR.ClawbackLoss == inGBP.ClawbackLoss {
		t.Errorf("the clawback loss resolved to %q in both currencies", inEUR.ClawbackLoss)
	}
}

// TestHouseAccountsZeroValueCannotResolve pins the zero value's promise:
// a HouseAccounts nobody constructed from configuration carries zero refs,
// and the ledger refuses those rather than minting accounts named after
// nothing.
func TestHouseAccountsZeroValueCannotResolve(t *testing.T) {
	t.Parallel()

	var houses wallet.HouseAccounts
	_, err := houses.EnsureAll(context.Background(), memory.New(), eur)
	if !errors.Is(err, wallet.ErrInvalidAccountRef) {
		t.Fatalf("EnsureAll() on the zero value = %v, want %v", err, wallet.ErrInvalidAccountRef)
	}
}

// TestNewHouseAccountsFromConfig pins the one mapping between the
// configuration keys and the wallet's purposes, field by field. Both sides
// of the translation are plain strings, so a swap - HOUSE_ACCOUNT_ROUNDING
// feeding the clawback purpose - would compile and pass every distinctness
// test; only checking each purpose against its own configured name catches
// it. The two names here differ so a swapped mapping cannot pass by
// coincidence.
func TestNewHouseAccountsFromConfig(t *testing.T) {
	t.Parallel()

	houses, err := wallet.NewHouseAccountsFromConfig(config.HouseAccountsConfig{
		Rounding: "house-rounding",
		Clawback: "house-clawback",
	})
	if err != nil {
		t.Fatalf("NewHouseAccountsFromConfig() error: %v", err)
	}
	if got := houses.RoundingRemainder(); got != wallet.HouseAccount("house-rounding") {
		t.Errorf("RoundingRemainder() = %v, want the account HOUSE_ACCOUNT_ROUNDING named", got)
	}
	if got := houses.ClawbackLoss(); got != wallet.HouseAccount("house-clawback") {
		t.Errorf("ClawbackLoss() = %v, want the account HOUSE_ACCOUNT_CLAWBACK named", got)
	}

	// The constructor's refusals must reach a caller going through the
	// translation unchanged - it adds a mapping, never leniency.
	if _, err := wallet.NewHouseAccountsFromConfig(config.HouseAccountsConfig{
		Rounding: "house-shared",
		Clawback: "house-shared",
	}); !errors.Is(err, wallet.ErrHouseNameShared) {
		t.Fatalf("a shared configured name = %v, want %v", err, wallet.ErrHouseNameShared)
	}
}

// brokenLedger delegates to a real ledger but refuses to ensure one
// account, so the test below can reach EnsureAll's second failure exit -
// the ledger going away between its two EnsureAccount calls - without a
// ledger that is actually broken.
type brokenLedger struct {
	wallet.Ledger
	refuse wallet.AccountRef
	err    error
}

func (b brokenLedger) EnsureAccount(ctx context.Context, ref wallet.AccountRef, currency money.Currency) (wallet.LedgerAccountID, error) {
	if ref == b.refuse {
		return "", b.err
	}
	return b.Ledger.EnsureAccount(ctx, ref, currency)
}

// TestHouseAccountsEnsureAllReportsTheFailedPurpose holds each of
// EnsureAll's failure exits to naming the purpose that failed: the
// operator fixing a half-ensured startup needs to know which account never
// came to exist, and the underlying refusal must survive the wrapping for
// errors.Is to keep working.
func TestHouseAccountsEnsureAllReportsTheFailedPurpose(t *testing.T) {
	t.Parallel()

	houses, err := wallet.NewHouseAccounts(houseNames())
	if err != nil {
		t.Fatalf("NewHouseAccounts() error: %v", err)
	}

	tests := []struct {
		name   string
		refuse wallet.AccountRef
		want   string
	}{
		{name: "the rounding remainder fails first", refuse: houses.RoundingRemainder(), want: "rounding remainder"},
		{name: "the clawback loss fails after the rounding succeeded", refuse: houses.ClawbackLoss(), want: "clawback loss"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sentinel := errors.New("the ledger went away")
			ledger := brokenLedger{Ledger: memory.New(), refuse: tt.refuse, err: sentinel}

			ids, err := houses.EnsureAll(context.Background(), ledger, eur)
			if !errors.Is(err, sentinel) {
				t.Fatalf("EnsureAll() = %v, want the ledger's own error wrapped", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the %s", err, tt.want)
			}
			if ids != (wallet.HouseAccountIDs{}) {
				t.Errorf("a failed EnsureAll leaked ids %+v; a caller must get all of them or none", ids)
			}
		})
	}
}
