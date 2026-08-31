// Which accounts a transition moves money between (T069, D7, FR-040).
//
// A file of its own for the reason state.go is: this is the other half of the
// design a person reasons about, and neither should have to be read through
// the machinery that carries it out. Every rule here is "an entry in state X
// becoming Y moves its amount from this account to that one", and every one
// of them is derived from what the state MEANS rather than chosen.

package earnings

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
)

// stages maps the four states that hold money to the ledger stage account
// holding it. Paid and reversed are absent, and that absence is the design:
// neither is a place money SITS.
//
// Paid money has left for a destination the payout path owns, and reversed
// money has gone back to the network - so a transition into either moves out
// of a stage account and into something this package does not name on its
// own. See [postingsFor].
var stages = map[State]wallet.Stage{
	StateHeld:      wallet.StageHeld,
	StatePending:   wallet.StagePending,
	StateConfirmed: wallet.StageConfirmed,
	StateReserved:  wallet.StageReserved,
}

// ErrNotThisPackagesToPost reports a transition that is lawful but whose
// money leaves the accounts this package can name.
//
// Only reserved to paid: the destination is a payout rail's account, chosen
// by the withdrawal the payment belongs to, and an entry does not know which
// withdrawal claimed it. The payout path performs that move and records the
// transition through the same machine; refusing it here is what keeps this
// package from inventing an account name to fill a gap it cannot see.
var ErrNotThisPackagesToPost = fmt.Errorf("earnings: paying an entry out is the payout path's to post, because only it knows the destination")

// movement is where an entry's amount comes from and where it goes for one
// transition.
type movement struct{ from, to wallet.AccountRef }

// postingsFor answers which accounts a transition moves between.
//
// The three shapes, and why each is what it is:
//
//   - OPENING. Money comes out of the commission the network reported and
//     into the member's opening stage. The receivable is where that
//     commission sits; taking the member's share from anywhere else would
//     credit money the business had not received.
//   - STAGE TO STAGE. Both sides are the member's own accounts. The member's
//     total does not change - what changes is which bucket counts toward the
//     withdrawal threshold - so both sides being theirs is the property that
//     makes a confirmation not a credit.
//   - REVERSING. Money goes back where it came from, out of whichever stage
//     was holding it. Never to the clawback account: that one absorbs a loss
//     Apivo has ALREADY PAID OUT and cannot recover (Q3), which is a
//     different fact from a network withdrawing a commission it never
//     settled. Conflating them would show the business a loss it did not
//     take.
func postingsFor(member uuid.UUID, from, to State, receivable string) (movement, error) {
	source, held := stages[from]
	target, holds := stages[to]

	switch {
	// Opening: there is no state the entry came from.
	case from == "":
		if !holds {
			return movement{}, fmt.Errorf("earnings: an entry cannot open as %s", to)
		}
		return movement{
			from: wallet.HouseAccount(receivable),
			to:   wallet.MemberAccount(member, target),
		}, nil

	case to == StateReversed:
		if !held {
			return movement{}, fmt.Errorf("earnings: an entry that is %s holds nothing to reverse", from)
		}
		return movement{
			from: wallet.MemberAccount(member, source),
			to:   wallet.HouseAccount(receivable),
		}, nil

	case to == StatePaid:
		return movement{}, ErrNotThisPackagesToPost

	case held && holds:
		return movement{
			from: wallet.MemberAccount(member, source),
			to:   wallet.MemberAccount(member, target),
		}, nil
	}
	return movement{}, ErrIllegalTransition{From: from, To: to}
}
