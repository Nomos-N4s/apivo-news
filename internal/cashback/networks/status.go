// The two closed sets a network's own vocabulary is mapped into - [Status]
// for a transaction, [MerchantStatus] for a route - with their parsers and
// the sentinel a word nobody mapped is refused with. One file, because
// contract rule 2's totality is one rule and both sets are held to it.

package networks

import (
	"errors"
	"fmt"
	"strconv"
)

// The sentinel an unrecognised status word is refused with. Rule 2 of the
// contract is that this mapping is total, so this is raised rather than a
// default being chosen quietly.
var (
	// ErrUnmappableStatus reports a status word no mapping recognised
	// (contract rule 2). It is what an adapter returns instead of guessing,
	// and what [ParseStatus] returns for a value outside the domain's closed
	// set. Unknown statuses must reach an operator: a network that invents a
	// state is telling us something about money we are about to promise
	// someone, and the two available guesses - withhold it or pay it - are
	// each wrong in a way nobody would notice.
	ErrUnmappableStatus = errors.New("networks: status has no mapping in the domain state machine")
)

// Status is the normalised transaction state: the single domain state
// machine every network's vocabulary maps into (FR-033). Pending leads to
// confirmed or declined, and reversed is reachable from either.
//
// The values are the schema's own words (migration 0012,
// network_transaction_status_known) rather than an enum with a rendering
// step, and they are kept verbatim for the same reason the catalogue keeps
// its rate kinds verbatim: these strings are written into immutable evidence
// rows, and a renamed constant would make every row already stored
// unreadable by the code that stored it.
//
// The zero value is the empty string, which is deliberately not a status. A
// forgotten mapping is then an error rather than a silently pending
// transaction - which is the whole of contract rule 2 held in the type.
type Status string

const (
	// StatusPending is reported and awaiting the advertiser's validation.
	// Visible to a member, never spendable, and it can sit here for as long
	// as the network's validation takes - up to 90 days at Awin, which is
	// why the poller re-reads a trailing window rather than waiting.
	StatusPending Status = "pending"
	// StatusConfirmed is validated by the advertiser: the only state whose
	// money counts toward a withdrawal (FR-050).
	StatusConfirmed Status = "confirmed"
	// StatusDeclined is refused by the advertiser. No money follows.
	StatusDeclined Status = "declined"
	// StatusReversed is money the network took back after reporting it,
	// reachable from either of the two states above - including from
	// confirmed, which is why a payout can outrun a reversal and why the
	// loss is absorbed by a house account rather than clawed back from the
	// member.
	StatusReversed Status = "reversed"
)

// Valid reports whether s is one of the four domain states. The zero value
// is not.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusConfirmed, StatusDeclined, StatusReversed:
		return true
	default:
		return false
	}
}

// String names the status, so an error about one says which it was.
func (s Status) String() string { return string(s) }

// ParseStatus turns a domain status word back into a [Status], refusing
// anything outside the closed set with an error wrapping
// [ErrUnmappableStatus].
//
// It parses the DOMAIN's vocabulary, never a network's. A network's own
// words are mapped by its adapter, from a table in its own package
// (ADR-0003), and an adapter that finds no entry for a word returns this
// package's [ErrUnmappableStatus] too - so the poller, the reconciliation
// and this function all report an unrecognised status the same way, and one
// operator alert covers every source of it.
func ParseStatus(s string) (Status, error) {
	status := Status(s)
	if !status.Valid() {
		return "", fmt.Errorf("%w: %s is not one of %s, %s, %s, %s",
			ErrUnmappableStatus, strconv.Quote(s),
			StatusPending, StatusConfirmed, StatusDeclined, StatusReversed)
	}
	return status, nil
}

// MerchantStatus is whether a retailer is still reachable through a network:
// the normalised form of what a catalogue poll says about one route. Like
// [Status] it is a closed set carrying the schema's own words
// (merchant_network_status_known) and has no valid zero value, for the same
// reason: a retailer who has left a network is exactly the fact a catalogue
// poll exists to discover, and defaulting it to active would keep publishing
// an offer that can no longer pay.
type MerchantStatus string

const (
	// MerchantStatusActive is a route that is live and can be clicked
	// through.
	MerchantStatusActive MerchantStatus = "active"
	// MerchantStatusPaused is a route the network reports as temporarily
	// not accepting traffic.
	MerchantStatusPaused MerchantStatus = "paused"
	// MerchantStatusLeftNetwork is a retailer no longer on this network at
	// all. Other routes to the same retailer are unaffected, which is why
	// this is a property of the route rather than of the merchant.
	MerchantStatusLeftNetwork MerchantStatus = "left_network"
)

// Valid reports whether s is one of the three route states. The zero value
// is not.
func (s MerchantStatus) Valid() bool {
	switch s {
	case MerchantStatusActive, MerchantStatusPaused, MerchantStatusLeftNetwork:
		return true
	default:
		return false
	}
}

// String names the status, so an error about one says which it was.
func (s MerchantStatus) String() string { return string(s) }

// ParseMerchantStatus turns a domain route status back into a
// [MerchantStatus], refusing anything outside the closed set with an error
// wrapping [ErrUnmappableStatus]. Contract rule 2's totality is not only
// about transactions: a catalogue word nobody mapped is the same class of
// silence, and it is reported the same way.
func ParseMerchantStatus(s string) (MerchantStatus, error) {
	status := MerchantStatus(s)
	if !status.Valid() {
		return "", fmt.Errorf("%w: %s is not one of %s, %s, %s",
			ErrUnmappableStatus, strconv.Quote(s),
			MerchantStatusActive, MerchantStatusPaused, MerchantStatusLeftNetwork)
	}
	return status, nil
}
