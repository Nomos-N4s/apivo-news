// What a refused click-out says, and when it says the rule lifts (T066,
// US7 scenario 1).
//
// The Retry-After arithmetic lives here rather than beside the counting,
// because it is the only part of the refusal a member can act on and it is
// the part with a corner in it: a window whose oldest click is falling out
// of it right now.

package clickout

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ErrTooManyClicks reports a click-out refused by the rule. The endpoint
// answers 429 with a Retry-After; nothing is minted and nothing is recorded,
// so a member who waits and tries again is in exactly the state they were.
var ErrTooManyClicks = errors.New("clickout: too many clicks")

// TooManyClicks says which half of the rule refused, and when it lifts.
//
// Both halves matter to the answer. The member sees a plain message and a
// Retry-After they can act on; the log sees which rule fired, which is the
// difference between one member clicking too fast and one device driving
// many accounts (US7).
type TooManyClicks struct {
	// Rule names the half that refused: [RuleMember] or [RuleContext].
	Rule string
	// Allowed is how many clicks that half permits in [ClickWindow].
	Allowed int
	// RetryAfter is how long until the oldest click in the window falls out
	// of it, which is the soonest the next click can be allowed. Never zero:
	// a Retry-After of nothing tells a client to try again immediately, and
	// it would be refused again.
	RetryAfter time.Duration
}

// The two halves of the rule, as they are named in an answer and a log line.
const (
	RuleMember  = "member"
	RuleContext = "context"
)

func (e TooManyClicks) Error() string {
	return fmt.Sprintf("clickout: %s made more than %d clicks in %s; the rule lifts in %s",
		e.Rule, e.Allowed, ClickWindow, e.RetryAfter)
}

// Unwrap makes the verdict matchable without knowing this type.
func (e TooManyClicks) Unwrap() error { return ErrTooManyClicks }

// refusal turns one count into a verdict, and works out when the rule lifts.
//
// The limit is reached at `allowed` rather than passed at `allowed+1`: the
// count is of clicks ALREADY recorded, and this one would be the next. A
// member who has made exactly their allowance has used it.
//
// The rule lifts when the oldest click in the window falls out of it. A
// missing oldest cannot happen while the count is at the limit - there is no
// window with clicks in it and no earliest one - but it is handled rather
// than assumed, because the alternative is a Retry-After computed from the
// zero time, which renders as a delay of two thousand years.
func refusal(rule string, allowed int, clicks int64, oldest pgtype.Timestamptz, at time.Time) error {
	if clicks < int64(allowed) {
		return nil
	}
	retry := ClickWindow
	if oldest.Valid {
		retry = oldest.Time.Add(ClickWindow).Sub(at)
	}
	// Never zero and never negative. A Retry-After of nothing tells a client
	// to try again at once, and it would be refused again; the smallest
	// honest answer is a second.
	if retry < time.Second {
		retry = time.Second
	}
	return TooManyClicks{Rule: rule, Allowed: allowed, RetryAfter: retry}
}
