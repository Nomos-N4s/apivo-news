// Where a member's bank details actually go (T095, ADR-0003, FR-051).
//
// cashback.payout_destination stores a REFERENCE and never the details. That
// is a rule about the database, and it needs somewhere for the details to be
// instead - so this is the port that names it, declared here because this is
// the package that has to hand them over.
//
// The details pass through one function call and are never stored in a value
// this package keeps, logged, or put in an error. [Destination] carries no
// details and never will; [NewDestination] takes a reference and says raw
// details must not reach it. This file is the seam that makes both true
// rather than aspirational: the handler reads them off the wire, gives them
// to the vault, and keeps only what comes back.
//
// No implementation ships in the alpha, and that is deliberate rather than
// unfinished. A vault is somebody's KMS, somebody's secrets manager, or a
// payment processor's own tokenisation endpoint, and picking one here would
// be picking it for every deployment. A deployment that has not chosen
// answers 503 on the one endpoint that needs it, exactly as one with no
// withdrawal threshold answers 503 on the one that needs that - the rest of
// the surface keeps working, including reading destinations that already
// exist and withdrawing to them.

package payout

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	// ErrNoVault reports a deployment that has nowhere to put bank details,
	// so it cannot record a destination.
	//
	// Discovered on the request rather than at construction, for the reason
	// [ErrNoThreshold] is: refusing to build would take the whole member
	// surface down, and every other route here works without a vault -
	// including withdrawing to a destination recorded before this
	// deployment forgot how.
	ErrNoVault = errors.New("payout: this deployment has nowhere to store payout details")
	// ErrDetailsRefused reports a vault that would not take the details:
	// malformed for the kind, too large, or a rail's tokenisation endpoint
	// saying no. It is the member's to act on, so it is distinct from a
	// vault that broke.
	ErrDetailsRefused = errors.New("payout: the payout details were refused")
)

// DetailsVault holds what this database must not.
//
// One method, and it is deliberately one-way: this port can put details in
// and get a reference back, and it cannot read them out again. Nothing in
// this product ever needs to - a rail resolves the reference itself, which
// is why [DestinationRef] carries the reference rather than the details
// (contract rule 3). A port that could read them would be a port somebody
// eventually reads them through.
type DetailsVault interface {
	// Store puts the details somewhere this database is not and answers the
	// reference cashback.payout_destination.details_ref will hold.
	//
	// The kind travels with them because what counts as valid details
	// differs by rail - an IBAN for sepa, a note for manual - and the vault
	// is the only thing positioned to check. Details it will not take are
	// refused wrapping [ErrDetailsRefused]; anything else is a failure.
	//
	// The reference must be stable and must not embed the details. A
	// "reference" that was the details in another encoding would put them
	// back in the column this whole arrangement exists to keep them out of.
	Store(ctx context.Context, kind Kind, details json.RawMessage) (string, error)
}
