package translation

import (
	"errors"
	"fmt"
)

// Caps is the budget FR-006 gives translation: the most one article may
// cost, and the most a calendar month may cost. Both are in micro-USD, the
// unit the ledger counts in.
//
// The caller supplies them. They are configuration - the alpha runs at
// $0.02 an article and $25 a month, and either number can change without a
// deployment of anything but config - so they are deliberately NOT in the
// schema. A CHECK (cost_microusd <= 20000) would turn a budget change into
// a migration and, far worse, would refuse to record a cost that had
// already been paid: the provider bills for the call whatever we think of
// the price, and a database that rejects the row destroys the honest
// record of the overspend and leaves the ledger reading low by exactly the
// amount that most needed to be seen.
//
// What the database contributes is the part application code cannot do
// safely: the atomic month counter and the once-per-month halt latch
// (migration 0005).
type Caps struct {
	// PerArticleMicroUSD is the ceiling on a single translation. A result
	// that cost more than this is not recorded as a translation - its
	// spend still is.
	PerArticleMicroUSD int64

	// MonthlyMicroUSD is the cap on a calendar month's total spend. On
	// reaching it the month halts and the pipeline stops translating
	// until the next one.
	MonthlyMicroUSD int64
}

// ErrCapsNotConfigured reports a Caps that cannot govern anything: a
// missing budget is not an unlimited one. Zero values would read as "every
// call is over the ceiling and the month is already over its cap", which
// would silently halt the pipeline and look like a policy decision rather
// than the wiring mistake it is.
var ErrCapsNotConfigured = errors.New("translation: budget caps are not configured")

// ErrOverCeiling reports a result that cost more than one article may
// cost. The translation is refused; the money it cost is still recorded,
// because it was still spent.
var ErrOverCeiling = errors.New("translation: result cost more than the per-article ceiling")

// Validate reports a budget that cannot be enforced. Both limits must be
// positive: a zero ceiling refuses every call and a zero cap halts the
// month before it starts, and neither is a budget anyone chose.
func (c Caps) Validate() error {
	switch {
	case c.PerArticleMicroUSD <= 0:
		return fmt.Errorf("%w: per-article ceiling is %d micro-USD, want a positive amount", ErrCapsNotConfigured, c.PerArticleMicroUSD)
	case c.MonthlyMicroUSD <= 0:
		return fmt.Errorf("%w: monthly cap is %d micro-USD, want a positive amount", ErrCapsNotConfigured, c.MonthlyMicroUSD)
	case c.PerArticleMicroUSD > c.MonthlyMicroUSD:
		return fmt.Errorf("%w: per-article ceiling %d micro-USD exceeds the monthly cap of %d, so a single article could halt the month", ErrCapsNotConfigured, c.PerArticleMicroUSD, c.MonthlyMicroUSD)
	}
	return nil
}

// OverCeiling reports whether one call's cost exceeds the per-article
// ceiling.
//
// Only the priced amount is compared. A result with unpriced attempts
// (Spend.UnmeteredAttempts) may really have cost more, but refusing a
// usable translation on a suspicion would throw away work that was paid
// for on the strength of a number nobody has. The lower bound is recorded
// and carried into the ledger instead, where an operator can see it.
func (c Caps) OverCeiling(costMicroUSD int64) bool {
	return costMicroUSD > c.PerArticleMicroUSD
}

// Reached reports whether a month's spend has met the monthly cap. It is
// >= rather than >: the cap is the amount the month may spend, so landing
// exactly on it is the last call the month gets.
func (c Caps) Reached(spentMicroUSD int64) bool {
	return spentMicroUSD >= c.MonthlyMicroUSD
}
