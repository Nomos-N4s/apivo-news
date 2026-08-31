// [Documented], what a network publishes about itself, and the one place the
// facts a cashback.network row is seeded from are written down. One file,
// because every field on it is a column that row has, and the value exists to
// be turned into one.

package networks

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidDocumentedNetwork reports a declaration that could not seed a
// cashback.network row. Every rule it names is a check constraint on that
// table (migration 0011, migration 0026), refused here so an adapter's
// declaration fails at the point somebody can read it rather than as a
// Postgres error with a constraint name in it.
var ErrInvalidDocumentedNetwork = errors.New("networks: the declaration does not describe a network")

// hoursPerDay converts the whole days a network documents its query window in
// into the duration a [QueryWindow] is compared against. It is written once,
// here, for the reason the per-minute rate is converted once: a "day" so
// measured does not follow a clock change, which is the safe direction since
// the arithmetic can only refuse a window the network might have allowed.
const hoursPerDay = 24 * time.Hour

// Documented is what a network publishes about how it may be used: the facts
// an operator would otherwise copy out of the network's documentation into a
// cashback.network row by hand.
//
// It is a declaration and NOT the value anything runs on. What a deployment
// paces to and refuses windows against comes from the row (see [Limits]),
// because a limit a network raises for one account is a row an operator edits
// rather than a release. This is only what that row is seeded with when
// nobody has said otherwise - so an adapter's declaration going stale costs a
// row that is already correct, never a running deployment.
//
// It carries no credential and no account. Those are configuration
// (ADR-0003), and the whole point of this value is that it is the part of a
// network's configuration that is public, identical for every publisher, and
// therefore safe to compile in.
type Documented struct {
	// ID is the network's own identifier, which is the primary key of the
	// row and the value NETWORK_DRIVER names.
	ID NetworkID
	// DisplayName is what an operator sees in a list. Not a member-facing
	// string: no member is shown which network paid them.
	DisplayName string
	// ClickRefParam is the query parameter this network reads the click
	// reference from (FR-021). It is a column rather than a literal in the
	// adapter because a wrong value silently loses attribution on every
	// click, and an operator must be able to correct it without a release.
	ClickRefParam string
	// MaxQueryWindowDays is the widest transaction window the network
	// documents. Awin's is 31.
	MaxQueryWindowDays int
	// RateLimitPerMinute is the request rate the network publishes, in the
	// unit networks publish it in. Awin's is 20 a minute - which is a third
	// of a request a second, and the reason this is not stored per second
	// anywhere.
	RateLimitPerMinute int
}

// Validate refuses a declaration a cashback.network row would refuse,
// wrapping [ErrInvalidDocumentedNetwork].
func (d Documented) Validate() error {
	if err := d.ID.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDocumentedNetwork, err)
	}
	if strings.TrimSpace(d.DisplayName) == "" {
		return fmt.Errorf("%w: network %s has no display name", ErrInvalidDocumentedNetwork, strconv.Quote(d.ID.String()))
	}
	if strings.TrimSpace(d.ClickRefParam) == "" {
		// The one field where a blank is worse than a wrong value: a
		// redirect built without a click reference reaches the retailer and
		// is never attributed to anybody.
		return fmt.Errorf("%w: network %s names no click-reference parameter",
			ErrInvalidDocumentedNetwork, strconv.Quote(d.ID.String()))
	}
	if d.MaxQueryWindowDays <= 0 {
		return fmt.Errorf("%w: network %s documents a maximum query window of %d days",
			ErrInvalidDocumentedNetwork, strconv.Quote(d.ID.String()), d.MaxQueryWindowDays)
	}
	if d.RateLimitPerMinute <= 0 {
		return fmt.Errorf("%w: network %s documents a rate of %d requests a minute",
			ErrInvalidDocumentedNetwork, strconv.Quote(d.ID.String()), d.RateLimitPerMinute)
	}
	return nil
}

// Limits is the declaration in the shape [Network.Limits] answers with. The
// day-to-duration conversion happens here and nowhere else, so there is one
// place for it to be wrong and one place to test.
func (d Documented) Limits() Limits {
	return Limits{
		MaxWindow:         time.Duration(d.MaxQueryWindowDays) * hoursPerDay,
		RequestsPerMinute: d.RateLimitPerMinute,
	}
}
