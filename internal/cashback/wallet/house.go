package wallet

// House accounts are the accounts the business itself owns inside the
// ledger: the design names exactly two, and this file is the one place the
// rest of the wallet gets either of them from. The rounding remainder
// account is where the sub-minor-unit fraction of a percent earning
// accrues, so that every commission split still sums to zero and the
// rounding never "disappears" (D6, FR-040). The clawback loss account is
// where money the business absorbs after a post-payout reversal lands:
// the founder decision is to record the loss and never chase the member
// (Q3), and recording it means it must have an account of its own to be
// read out of.
//
// The names come from configuration, never from literals in domain code
// (data-model.md 2.6) - so the type here is constructed once from the
// configured names, validated, and handed around. Bootstrap code builds it
// with [NewHouseAccountsFromConfig], resolves the ledger ids through
// [HouseAccounts.EnsureAll] and everything else asks the type, which is
// what keeps a house name from ever being spelled twice: a name spelled
// twice is a name that can be spelled two ways, and the ledger would
// obligingly open an account for each of them.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// ErrHouseNameShared reports two house purposes configured to one account
// name. It extends the sentinel set in ledger.go under the same rule -
// every failure this package reports wraps exactly one sentinel - and it
// exists because this is the one misconfiguration the port cannot catch:
// EnsureAccount's naming is injective over distinct names, but two
// purposes handed the same name are, to the port, legitimately the same
// account. Only the layer that knows the purposes can see the collision,
// so it is refused here, at construction, before any account exists.
var ErrHouseNameShared = errors.New("wallet: two house purposes share one configured account name")

// HouseNames carries the configured house account names into
// [NewHouseAccounts]: one exported field per purpose the design names,
// nothing more. It is a struct rather than positional strings because the
// fields are the same type, and a caller that swapped two positional names
// would compile - and would then post every absorbed loss into the
// rounding account, silently.
type HouseNames struct {
	// RoundingRemainder names the account the sub-minor-unit remainder
	// of every percent earning accrues to (D6, FR-040).
	RoundingRemainder string
	// ClawbackLoss names the account an absorbed post-payout clawback is
	// recorded against (Q3).
	ClawbackLoss string
}

// HouseAccounts is the resolved set of house accounts: one method per
// purpose, each returning the [AccountRef] for the configured name. Build
// one with [NewHouseAccounts]; the fields are unexported and the zero
// value carries zero refs, which every ledger refuses at EnsureAccount, so
// a HouseAccounts that was never constructed from configuration cannot
// quietly resolve to anything.
type HouseAccounts struct {
	rounding AccountRef
	clawback AccountRef
}

// NewHouseAccounts validates the configured names and returns the one
// value the rest of the wallet names house accounts through.
//
// A name that names no account - empty, or padded with the whitespace a
// hand-assembled configuration picks up - is refused wrapping
// [ErrInvalidAccountRef], with the purpose it was configured for named so
// the operator knows which key to fix. The rule itself is
// [AccountRef.Validate]'s; it is applied here, at construction, rather
// than left to the first EnsureAccount, because the first EnsureAccount
// for the clawback account may be months after the deploy that broke it.
//
// Two purposes configured to the SAME name are refused wrapping
// [ErrHouseNameShared], and the refusal is deliberate rather than a
// tolerated aliasing. The two accounts exist to keep two different
// figures readable: D6 makes the rounding remainder an explicit posting
// precisely so it is never lost, and Q3's posture is to absorb a clawback
// AND record it. Balances derive from postings and nothing else (D7), so
// one account holding both purposes' postings is one figure that answers
// neither question - how much rounding has accrued, and how much loss has
// been absorbed - and no later query can pull the two apart again. A
// shared name is therefore the misconfiguration this constructor exists
// to catch, not a layout an operator may choose.
func NewHouseAccounts(names HouseNames) (HouseAccounts, error) {
	// The purposes are walked as data so the empty-name and shared-name
	// rules are stated once each: a third house account, should the
	// design ever name one, is a new row here and inherits both.
	purposes := []struct {
		purpose string
		name    string
	}{
		{"rounding remainder", names.RoundingRemainder},
		{"clawback loss", names.ClawbackLoss},
	}
	owner := make(map[string]string, len(purposes))
	for _, p := range purposes {
		if HouseAccount(p.name).Validate() != nil {
			return HouseAccounts{}, fmt.Errorf("%w: the %s house account is configured with name %q", ErrInvalidAccountRef, p.purpose, p.name)
		}
		if taken, shared := owner[p.name]; shared {
			return HouseAccounts{}, fmt.Errorf("%w: %q is configured as both the %s account and the %s account, and one balance cannot carry two meanings", ErrHouseNameShared, p.name, taken, p.purpose)
		}
		owner[p.name] = p.purpose
	}
	return HouseAccounts{
		rounding: HouseAccount(names.RoundingRemainder),
		clawback: HouseAccount(names.ClawbackLoss),
	}, nil
}

// NewHouseAccountsFromConfig is the one translation from the configuration
// keys to the wallet's purposes: HOUSE_ACCOUNT_ROUNDING becomes the
// rounding remainder account and HOUSE_ACCOUNT_CLAWBACK the clawback loss
// account. The mapping is written here, once, rather than left to the
// composition root, because both sides of it are plain strings - a caller
// assembling [HouseNames] by hand could swap them and would compile, and
// every absorbed loss would then post to the rounding account, silently.
// Bootstrap code calls this and never touches the field pairing at all.
func NewHouseAccountsFromConfig(cfg config.HouseAccountsConfig) (HouseAccounts, error) {
	return NewHouseAccounts(HouseNames{
		RoundingRemainder: cfg.Rounding,
		ClawbackLoss:      cfg.Clawback,
	})
}

// RoundingRemainder returns the account the sub-minor-unit remainder of a
// percent earning accrues to (D6, FR-040). The member's share is rounded
// in the member's favour and whatever fraction is left posts here, which
// is what keeps every split summing to zero instead of quietly shedding
// the difference.
func (h HouseAccounts) RoundingRemainder() AccountRef { return h.rounding }

// ClawbackLoss returns the account an absorbed loss is recorded against
// when a transaction reverses after its payout (Q3). The member is never
// chased, so the money has to land somewhere the business can read its
// losses from - this account is that somewhere.
func (h HouseAccounts) ClawbackLoss() AccountRef { return h.clawback }

// HouseAccountIDs is what [HouseAccounts.EnsureAll] resolves: the ledger's
// id for each house account, in the currency it was ensured in, one
// exported field per purpose. The ids are distinct by construction -
// [NewHouseAccounts] refused a shared name, and EnsureAccount's naming is
// injective over distinct names - so a transfer between two purposes can
// never collapse onto one account and be refused as [ErrNoMovement].
type HouseAccountIDs struct {
	RoundingRemainder LedgerAccountID
	ClawbackLoss      LedgerAccountID
}

// EnsureAll resolves every house account in the given currency, creating
// whichever did not exist yet, and returns their ids. It exists so
// bootstrap code makes one call and never spells a house name at all -
// the names live in configuration and in the [HouseAccounts] built from
// it, nowhere else.
//
// It is as idempotent as the EnsureAccount it is built on: the same
// HouseAccounts over the same ledger and currency yields the same ids on
// every call, however many callers race, so it is safe to run on every
// startup. One call resolves one currency (C-6) - a deployment trading in
// two calls it twice and holds two sets of ids, because a house account,
// like any account, holds exactly one currency.
func (h HouseAccounts) EnsureAll(ctx context.Context, ledger Ledger, currency money.Currency) (HouseAccountIDs, error) {
	rounding, err := ledger.EnsureAccount(ctx, h.rounding, currency)
	if err != nil {
		return HouseAccountIDs{}, fmt.Errorf("wallet: ensuring the rounding remainder house account: %w", err)
	}
	clawback, err := ledger.EnsureAccount(ctx, h.clawback, currency)
	if err != nil {
		return HouseAccountIDs{}, fmt.Errorf("wallet: ensuring the clawback loss house account: %w", err)
	}
	return HouseAccountIDs{RoundingRemainder: rounding, ClawbackLoss: clawback}, nil
}
