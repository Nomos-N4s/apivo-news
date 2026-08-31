// Everything GET /wallet answers with, assembled from two different kinds of
// truth (T078, FR-041, FR-050).
//
// Four of the five figures are balances, projected from the ledger on every
// read (see projection.go). The fifth is not a balance at all: money a member
// has been paid has left the business, so no account holds it and there is
// nothing to project - it is summed from the payments that made it happen.
//
// Keeping the two apart in one file is deliberate. They answer the same
// question for a member and they are answered from different places, and a
// later reader who assumed all five came from the ledger would look for a
// paid-out account that does not exist.

package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoProjector reports a wallet built with nothing to read balances
	// from.
	ErrNoProjector = errors.New("wallet: a wallet needs a projector to read the balances from")
	// ErrNoPayouts reports a wallet built with nothing to read settled
	// payments from. Refused rather than defaulted to zero: "you have been
	// paid nothing" is a claim, and a wallet that could not check it would
	// be making it anyway.
	ErrNoPayouts = errors.New("wallet: a wallet needs somewhere to read the settled payouts from")
	// ErrNoThreshold reports a deployment that has not configured the
	// balance a withdrawal is checked against (FR-050).
	//
	// Refused when the wallet is READ rather than when it is built, and that
	// is the whole design of it. Production cannot start without a threshold
	// (config.requireCashbackComplete), so nothing deployed reaches this;
	// the environments that can are the no-Docker loop and CI, which spike
	// S3 pins as complete with four keys and no money configuration at all.
	// Refusing at construction would unmount the endpoint there, and an
	// endpoint that answers 404 says the API is not here - where this says
	// what is missing, and names the keys.
	ErrNoThreshold = errors.New("wallet: this deployment has not configured a withdrawal threshold")
	// ErrNotRead reports a wallet that could not be assembled.
	ErrNotRead = errors.New("wallet: the member's wallet could not be read")
)

// Wallet is what a member is shown: where their money is, what they have
// been paid, and what they must reach before they can ask to be paid again.
type Wallet struct {
	// Totals is the four stage balances, projected from the ledger.
	Totals
	// PaidOut is the lifetime total of settled payments to this member. Not
	// a balance: the money is gone, and this is the record that it went.
	PaidOut money.Amount
	// Threshold is the confirmed balance a withdrawal is checked against
	// (FR-050). Returned beside the totals rather than left to the client,
	// because a client that hard-coded it would show one figure while the
	// server enforced another.
	Threshold money.Amount
}

// Payouts is the read this needs: what one member has actually been paid, in
// one currency. Named here per the boundary rules; *store.Queries satisfies
// it over a pool or a transaction.
type Payouts interface {
	SettledPayoutsFor(ctx context.Context, arg store.SettledPayoutsForParams) (int64, error)
}

// Wallets assembles a member's wallet.
type Wallets struct {
	projector *Projector
	payouts   Payouts
	threshold money.Amount
}

// NewWallets builds the reader. The threshold may be the zero Amount - see
// [ErrNoThreshold] for why that is discovered on read rather than here.
func NewWallets(projector *Projector, payouts Payouts, threshold money.Amount) (*Wallets, error) {
	switch {
	case projector == nil:
		return nil, ErrNoProjector
	case payouts == nil:
		return nil, ErrNoPayouts
	}
	return &Wallets{projector: projector, payouts: payouts, threshold: threshold}, nil
}

// Of assembles the member's wallet.
//
// The currency is the THRESHOLD'S. There is no other candidate that is not a
// guess: the ledger will answer in whatever currency it is asked for, the
// member's entries may span several if a deployment ever publishes in
// several, and picking the one they have most of would change what a member
// is shown as their money moves. The threshold is the one currency the
// deployment has stated, and it is the currency the withdrawal check compares
// in - so a wallet in any other currency would show a member a balance the
// threshold beside it does not apply to.
func (w *Wallets) Of(ctx context.Context, member uuid.UUID) (Wallet, error) {
	if !w.threshold.Currency.Valid() {
		return Wallet{}, ErrNoThreshold
	}
	totals, err := w.projector.Of(ctx, member, w.threshold.Currency)
	if err != nil {
		return Wallet{}, err
	}
	paid, err := w.paidOut(ctx, member)
	if err != nil {
		return Wallet{}, err
	}
	return Wallet{Totals: totals, PaidOut: paid, Threshold: w.threshold}, nil
}

// paidOut reads the lifetime total of settled payments.
func (w *Wallets) paidOut(ctx context.Context, member uuid.UUID) (money.Amount, error) {
	minor, err := w.payouts.SettledPayoutsFor(ctx, store.SettledPayoutsForParams{
		AccountID: pgtype.UUID{Bytes: member, Valid: true},
		Currency:  string(w.threshold.Currency),
	})
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: reading %s's settled payouts: %w", ErrNotRead, member, err)
	}
	// Through money.New rather than assembled, for the reason every other
	// amount here is: this is the last place a total can be checked before
	// somebody is shown it beside a currency symbol (C-6).
	paid, err := money.New(minor, w.threshold.Currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: %s's settled payouts: %w", ErrNotRead, member, err)
	}
	return paid, nil
}
