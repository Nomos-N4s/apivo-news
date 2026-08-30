// Applying the click rule: counting what is already recorded (T066, US7
// scenario 1).

package clickout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
)

// ErrNoClickRates reports a limiter built with nothing to count with.
var ErrNoClickRates = errors.New("clickout: the click rule needs somewhere to count clicks")

// ClickRates is the counting this file needs, named here per the boundary
// rules. *store.Queries satisfies it.
type ClickRates interface {
	CountRecentClicksByAccount(ctx context.Context, arg store.CountRecentClicksByAccountParams) (store.CountRecentClicksByAccountRow, error)
	CountRecentClicksByContext(ctx context.Context, arg store.CountRecentClicksByContextParams) (store.CountRecentClicksByContextRow, error)
}

// Limiter applies the click rule. Build it with [NewLimiter].
type Limiter struct {
	rates ClickRates
	rule  ClickRule
}

// NewLimiter builds the limiter over the given counts and rule.
func NewLimiter(rates ClickRates, rule ClickRule) (*Limiter, error) {
	if rates == nil {
		return nil, ErrNoClickRates
	}
	return &Limiter{rates: rates, rule: rule}, nil
}

// Allow reports whether a click at this moment is within the rule, returning
// a [TooManyClicks] when it is not.
//
// It counts what is already recorded rather than keeping a counter of its
// own. That is the whole design: the click table is the record of what
// happened, it is append-only, and it is shared by every replica - so the
// rule cannot be evaded by hitting another process, cannot be reset by a
// restart, and cannot drift from the evidence an operator would look at
// while deciding whether this was abuse.
//
// The member half is checked first and independently of the context half: a
// member over their own limit is refused whether or not this deployment can
// tell devices apart.
func (l *Limiter) Allow(ctx context.Context, member uuid.UUID, digest ContextDigest, at time.Time) error {
	since := at.Add(-ClickWindow)

	if l.rule.PerMember > 0 {
		row, err := l.rates.CountRecentClicksByAccount(ctx, store.CountRecentClicksByAccountParams{
			AccountID: pgtype.UUID{Bytes: member, Valid: true},
			Since:     pgtype.Timestamptz{Time: since, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("clickout: counting recent clicks for %s: %w", member, err)
		}
		if refused := refusal(RuleMember, l.rule.PerMember, row.Clicks, row.Oldest, at); refused != nil {
			return refused
		}
	}

	// Only where both a rule and a digest exist. A click with no context
	// digested is not a device, and counting the clicks that share "no
	// context" would bracket every member who arrived without one.
	if l.rule.PerContext > 0 && digest.Recorded() {
		row, err := l.rates.CountRecentClicksByContext(ctx, store.CountRecentClicksByContextParams{
			ContextDigest: pgtype.Text{String: digest.String(), Valid: true},
			Since:         pgtype.Timestamptz{Time: since, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("clickout: counting recent clicks for a context: %w", err)
		}
		if refused := refusal(RuleContext, l.rule.PerContext, row.Clicks, row.Oldest, at); refused != nil {
			return refused
		}
	}
	return nil
}
