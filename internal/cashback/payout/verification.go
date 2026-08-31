// Proving a destination belongs to the member who named it (T087, FR-051),
// and the rule that money never moves to one nobody proved.
//
// One file, because the two are the same rule seen from its two ends: the
// write that records the proof, and the check that refuses to pay without
// it. Separating them would let a later caller find the second and forget
// that the first is what makes it mean anything.

package payout

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/store"
)

// The sentinels verification is refused with.
var (
	// ErrDestinationNotVerified reports a destination nobody has proved
	// belongs to the member. The contract answers 409 for it - not 403,
	// which is what an unrelated member's destination gets: this one IS
	// theirs, it is simply not ready, and the two are different things for
	// a member to be told.
	//
	// It exists as a value rather than as a bare boolean because the check
	// happens at the moment money is about to move, and a caller that
	// forgot it would be paying out to an address nobody confirmed.
	ErrDestinationNotVerified = errors.New("payout: this destination has not been verified")
	// ErrNoVerificationMethod reports a verification with no method.
	// payout_destination_verification_all_or_none makes "verified, method
	// unknown" unstorable, and for a reason worth restating here: it is not
	// a verification anyone could defend later.
	ErrNoVerificationMethod = errors.New("payout: a verification says how it was done, or it is not one")
)

// Verify records that a member proved this destination is theirs.
//
// It is idempotent, and deliberately so. Verification is one-way and final -
// the table's guard refuses to change or re-date one that exists, because it
// is the evidence a withdrawal was allowed to name the destination - so a
// second call returns the verification that already stands rather than
// replacing it or failing. A member who submits a confirmation twice has not
// done anything wrong.
//
// A destination belonging to somebody else is [ErrDestinationNotFound], the
// same answer as one that does not exist, for the reason [Destinations.Get]
// gives.
func (d *Destinations) Verify(ctx context.Context, accountID, id uuid.UUID, method string) (Destination, error) {
	if strings.TrimSpace(method) == "" {
		return Destination{}, fmt.Errorf("%w: destination %s", ErrNoVerificationMethod, id)
	}

	// Read first, and not only to be friendly about a repeat: the update
	// below cannot distinguish "not theirs" from "already verified" - both
	// are no rows - and those are a refusal and a success.
	existing, err := d.Get(ctx, accountID, id)
	if err != nil {
		return Destination{}, err
	}
	if existing.Verified() {
		return existing, nil
	}

	row, err := d.q.VerifyPayoutDestination(ctx, store.VerifyPayoutDestinationParams{
		ID:             existing.id(),
		AccountID:      existing.accountID(),
		VerifiedMethod: pgtypeText(method),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Somebody verified it between the read and the write. That is the
		// same outcome as the branch above, reached by a different route,
		// and the destination is verified either way - so it is read back
		// rather than reported as a failure.
		return d.Get(ctx, accountID, id)
	case err != nil:
		return Destination{}, fmt.Errorf("payout: verifying destination %s for member %s: %w", id, accountID, err)
	}
	return destinationFrom(row), nil
}

// RequireVerified refuses a destination nobody has proved belongs to the
// member, wrapping [ErrDestinationNotVerified].
//
// It is a function rather than a comparison each caller writes, because it is
// the last thing standing between a reservation and a payment: FR-051 says an
// unverified destination is rejected and never silently used, and "never
// silently" is a property of there being one place that says so.
func RequireVerified(d Destination) error {
	if !d.Verified() {
		return fmt.Errorf("%w: %s", ErrDestinationNotVerified, strconv.Quote(d.ID.String()))
	}
	return nil
}
