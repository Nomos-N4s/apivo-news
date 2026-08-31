// What a member's money adds up to, read from the ledger every time (T077,
// FR-041, D7, SC-006).
//
// Nothing here is stored and nothing here is cached. A member's totals are a
// projection of the postings behind them, computed on each read, because the
// alternative - a balance column somebody remembers to update - is the exact
// disagreement between the wallet and the ledger that C-1 exists to prevent.
// A stale column looks right until the day it does not, and the day it does
// not is a day somebody is paid the wrong amount.
//
// FR-041 is what these totals MEAN: a member's balance is a claim on a future
// rebate, not money the business is holding on their behalf. The stages say
// how far along that claim is, and only confirmed counts toward a withdrawal
// (FR-050).
//
// What is NOT here is the lifetime total a member has been paid. No account
// holds it: paid money has left the business, so there is no balance to
// project and nothing in the ledger a member's own history can be summed
// from. It is read from the settled payouts that made it happen, which is
// evidence rather than a cached balance, and it belongs with the endpoint
// that renders it.

package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoLedgerToProject reports a projector built with no ledger to read.
	// Refused at construction, because one discovered later is discovered
	// while answering a member asking where their money is.
	ErrNoLedgerToProject = errors.New("wallet: a projection needs a ledger to read the postings from")
	// ErrNoMemberToProject reports a projection asked for nobody. The zero
	// uuid is not a member, and the accounts it names are accounts nobody
	// meant to open.
	ErrNoMemberToProject = errors.New("wallet: a projection needs the member it is about")
	// ErrNotProjected reports totals that could not be read. It wraps the
	// ledger's own failure: a caller must not render a partial wallet, because
	// a total silently short by one stage is a number the member will act on.
	ErrNotProjected = errors.New("wallet: the member's totals could not be read")
)

// Totals is what one member's money adds up to in one currency, by stage.
//
// Every field is a [money.Amount] rather than a bare integer, so a total can
// never be rendered against a currency nobody chose (C-6). All four carry the
// currency the projection was asked for, including the ones that are zero:
// "nothing, in euros" is a different statement from "nothing".
type Totals struct {
	// Held is money credited under a hold rule (US7). Deliberately absent
	// from the member-facing contract - GET /wallet names pending, confirmed,
	// reserved and paid out, and not this - because a hold is the business
	// deciding not to count money yet, and showing it as a fifth total would
	// invite the member to count it. It is projected all the same: an
	// operator asked where a member's money is needs the whole picture, and a
	// total that exists nowhere cannot be reconciled against the ledger.
	Held money.Amount
	// Pending is money a network has reported and not yet confirmed. Visible
	// to the member, never spendable.
	Pending money.Amount
	// Confirmed is money a network has confirmed AND a statement has
	// accounted for (FR-043). The only bucket that counts toward the
	// withdrawal threshold (FR-050).
	Confirmed money.Amount
	// Reserved is money a withdrawal request has claimed (D9). Still the
	// member's, and no longer available to a second request - which is the
	// whole reason reserving happens at request time rather than at approval.
	Reserved money.Amount
}

// Claim is everything the member could eventually be paid: the four stages
// added up.
//
// Named a claim rather than a balance, because that is what FR-041 says it
// is. It is not money the business holds for the member, and calling it a
// balance in the one place the four are combined is how that distinction
// would start to erode.
func (t Totals) Claim() (money.Amount, error) {
	claim, err := money.Sum(t.Held, t.Pending, t.Confirmed, t.Reserved)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: %w", ErrNotProjected, err)
	}
	return claim, nil
}

// Balances is the slice of the ledger a projection needs: naming an account
// and reading what it holds.
//
// Narrower than [Ledger] on purpose. A projection is a READ, and a type that
// held the whole port would be one refactor away from posting a transfer to
// make a total come out right - which is the one thing a projection must
// never do.
type Balances interface {
	EnsureAccount(ctx context.Context, ref AccountRef, currency money.Currency) (LedgerAccountID, error)
	Balance(ctx context.Context, account LedgerAccountID, currency money.Currency) (money.Amount, error)
}

// Projector reads a member's totals from the ledger.
type Projector struct {
	ledger Balances
}

// NewProjector builds the projector over the given ledger, refusing a nil one.
func NewProjector(ledger Balances) (*Projector, error) {
	if ledger == nil {
		return nil, ErrNoLedgerToProject
	}
	return &Projector{ledger: ledger}, nil
}

// Of answers what the member's money adds up to, in the given currency.
//
// One currency per call, and not a total across currencies, because there is
// no such total: adding euros to zloty needs a rate, a rate needs a moment,
// and a member's wallet is not the place to invent either (C-6). A deployment
// that publishes in two currencies asks twice and shows two wallets.
//
// The accounts are NAMED rather than looked up, and naming one creates it if
// it does not exist. That is the port's own contract - the id is derived from
// the pair, so the insert is deterministic and idempotent - and it is what
// makes a member who has never earned anything read as four zeroes instead of
// four unknown-account errors. The rows it leaves behind are empty accounts
// that were always going to exist the moment that member earned.
func (p *Projector) Of(ctx context.Context, member uuid.UUID, currency money.Currency) (Totals, error) {
	if member == uuid.Nil {
		return Totals{}, ErrNoMemberToProject
	}
	if !currency.Valid() {
		return Totals{}, fmt.Errorf("%w: %w: %q", ErrNotProjected, money.ErrInvalidCurrency, string(currency))
	}

	var totals Totals
	into := map[Stage]*money.Amount{
		StageHeld:      &totals.Held,
		StagePending:   &totals.Pending,
		StageConfirmed: &totals.Confirmed,
		StageReserved:  &totals.Reserved,
	}
	// Ranged over the stages rather than the map, because a map's order is
	// not one - and a partial failure should name the same stage every time
	// it happens, or two reports of one outage read as two different faults.
	for _, stage := range []Stage{StageHeld, StagePending, StageConfirmed, StageReserved} {
		held, err := p.balanceOf(ctx, member, stage, currency)
		if err != nil {
			return Totals{}, err
		}
		*into[stage] = held
	}
	return totals, nil
}

// balanceOf reads one stage account, naming the stage in whatever it refuses
// with: "the member's confirmed balance could not be read" is actionable and
// "the member's totals could not be read" is not.
func (p *Projector) balanceOf(ctx context.Context, member uuid.UUID, stage Stage, currency money.Currency) (money.Amount, error) {
	account, err := p.ledger.EnsureAccount(ctx, MemberAccount(member, stage), currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: naming %s's %s account: %w", ErrNotProjected, member, stage, err)
	}
	held, err := p.ledger.Balance(ctx, account, currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: reading %s's %s balance: %w", ErrNotProjected, member, stage, err)
	}
	// The ledger answers in the currency it was asked for; this catches an
	// implementation that does not, before the number reaches a member beside
	// the wrong symbol.
	if held.Currency != currency {
		return money.Amount{}, fmt.Errorf("%w: %s's %s balance came back in %s, not %s",
			ErrNotProjected, member, stage, held.Currency, currency)
	}
	return held, nil
}
