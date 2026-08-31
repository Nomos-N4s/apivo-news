// What a member is owed from one reported commission (T068, FR-040).
//
// The arithmetic itself lives in platform/money, which holds D6's exactness
// contract - share plus remainder equals the commission, for every rate and
// every mode. What this file adds is the POLICY: which rate applies, which
// direction it rounds, and what rounding in the member's favour cost, stated
// as a number rather than left as a difference nobody computed.

package earnings

import (
	"errors"
	"fmt"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// MemberFavour is the direction a member's share rounds.
//
// Q4's answer, and it is a constant rather than configuration because it is
// the one half of Q4 the plan does NOT leave open: the percentages are
// configuration, the direction is a product promise. Applied to a credit,
// ceiling rounds up; applied to a debit it rounds toward zero, so a reversal
// takes back no more than the exact arithmetic says. Both are the member's
// favour, which is why one constant serves both signs.
const MemberFavour = money.RoundCeil

var (
	// ErrShareOutOfRange reports a snapshot whose member share is not a
	// proportion of the whole. It cannot be produced by anything this code
	// writes - the schema checks the column and clickout checks it again on
	// the way out - so reaching it means a row predates a constraint or was
	// written by something else, and paying against it would credit more than
	// was earned.
	ErrShareOutOfRange = errors.New("earnings: the click's snapshotted member share is not between zero and the whole")
)

// Share is one reported commission divided as the click-time snapshot
// promised.
//
// Member plus Remainder is the commission EXACTLY, for every rate and every
// amount. That identity is the whole point of returning the remainder rather
// than the share alone: rounding that quietly disappears is the classic way a
// ledger stops balancing, and C-1 is held here by arithmetic rather than by
// anybody remembering to check.
type Share struct {
	// Member is what the member is owed, rounded in their favour.
	Member money.Amount
	// Remainder is everything the commission is not the member's. It is what
	// the transfer's other side must carry for the posting to sum to zero.
	Remainder money.Amount
	// Rounding is what rounding in the member's favour cost, and is the part
	// the house account absorbs (D6): the difference between Member and the
	// share exact arithmetic gives. Never more than one minor unit, because
	// one minor unit is the whole of what a rounding step can move.
	//
	// It is computed rather than inferred because the two have different
	// destinations. Remainder is the commission's other side; Rounding is a
	// breakdown of how Member was arrived at, and a caller that had to derive
	// it would be re-doing the arithmetic this function exists to have done
	// once.
	Rounding money.Amount
}

// ShareOf divides a reported commission at the rate the member was promised
// when they clicked.
//
// The rate comes from the CLICK, never from the offer as it stands now
// (FR-013). A member who clicked a published band is owed that band even if
// it has since been withdrawn, lowered, or the retailer has left the network;
// reading the offer here would silently reprice every earning whenever a
// catalogue poll ran.
//
// The commission is what the network ACTUALLY reported, not what the band
// predicted. Those differ routinely - a partial refund, a currency the
// network settled at a different rate, a correction - and the money that
// exists is the money reported.
func ShareOf(commission money.Amount, promised clickout.Promise) (Share, error) {
	if !promised.MemberShare.Valid() {
		return Share{}, fmt.Errorf("%w: %d basis points", ErrShareOutOfRange, int32(promised.MemberShare))
	}

	member, remainder, err := commission.Split(promised.MemberShare, MemberFavour)
	if err != nil {
		return Share{}, fmt.Errorf("earnings: dividing %s at %d basis points: %w",
			commission, int32(promised.MemberShare), err)
	}

	// The same split with the fraction dropped instead of taken. The
	// difference is exactly what the favourable direction moved, which is
	// what the house absorbs - and asking money for it a second time is
	// cheaper and less error-prone than reconstructing it from a fraction
	// this package would have to compute itself.
	exact, _, err := commission.Split(promised.MemberShare, money.RoundTowardZero)
	if err != nil {
		return Share{}, fmt.Errorf("earnings: dividing %s at %d basis points: %w",
			commission, int32(promised.MemberShare), err)
	}
	rounding, err := member.Sub(exact)
	if err != nil {
		return Share{}, fmt.Errorf("earnings: what the rounding of %s cost: %w", commission, err)
	}

	return Share{Member: member, Remainder: remainder, Rounding: rounding}, nil
}
