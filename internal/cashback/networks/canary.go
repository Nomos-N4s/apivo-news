// The first-attribution canary (#524): the one check that can tell a
// misconfigured click side from a slow week.
//
// A network's click side can be wrong in two ways that look identical from
// outside. The tracking URL may carry Apivo's reference in a parameter the
// network does not read, in which case every transaction comes back with no
// reference at all; or the parameter may truncate it, in which case every
// transaction comes back with a reference that matches no click. Either
// way every redirect works, every member reaches the shop, every purchase
// is reported and stored, and nobody is credited a cent. The reports go to
// the unattributed queue (FR-034), so the failure is visible to an operator
// who looks - but nothing says LOOK, and a queue that grows by three rows
// in the product's first week reads like a product in its first week.
//
// This is what says look. While a network has never once credited a click
// - not one entry citing one - it counts the distinct transactions that
// went unattributed since Apivo's first click through that network, and
// once there are enough of them to rule out chance it refuses, every
// forward sweep, until somebody fixes the parameter or the first credit
// lands. After that first credit it retires for good: the reference has
// round-tripped, the parameter is right, and how many LATER transactions
// go unattributed is a different question with a different owner.
//
// It runs inside the forward sweep rather than as a job of its own. The
// sweep is the moment new evidence arrives, so it is the moment the answer
// can change; a job of its own would cost a lock, two pool connections and
// a line in the capacity arithmetic in cmd/apivo/main.go, and would run at
// moments when nothing has changed.

package networks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// AttributionCanaryThreshold is how many distinct transactions must go
// unattributed, with none ever attributed, before the canary refuses.
//
// Three, and the arithmetic is the reason. One stray transaction is
// ordinary: a network echoes a reference it was handed by another
// publisher's link, or a purchase made through a link that predates the
// deployment lands in a backfill window. Refusing on one would be refusing
// on every network's first week. But three in a row with NONE attributed is
// not chance if the parameter works at all - even at a one-in-two
// attribution rate that is one chance in eight, and at the nine-in-ten a
// working network actually shows it is one in a thousand. Past three the
// count stops adding information and starts adding delay, which is the
// wrong direction for a check whose whole value is being early.
const AttributionCanaryThreshold = 3

// ErrAttributionNeverSucceeded reports the canary's refusal: enough
// transactions have gone unattributed since Apivo's first click through this
// network, and not one has ever been credited to a click, that the click
// reference is almost certainly not making the round trip.
//
// Returned every forward sweep for as long as it stands, because an
// incident logged once and scrolled away is an incident forgotten - the
// same reason the C-1 check keeps saying so. It never stops the sweep: the
// evidence must keep being stored, because the day the parameter is fixed
// the queued reports are what an operator attributes by hand.
var ErrAttributionNeverSucceeded = errors.New("networks: attribution has never succeeded at this network")

// ErrNoCanaryStore reports a canary built with nowhere to count from.
var ErrNoCanaryStore = errors.New("networks: the attribution canary needs a database to count from")

// AttributionCanary asks the database, per network, whether a click
// reference has ever come back matched.
type AttributionCanary struct {
	queries *store.Queries
}

// NewAttributionCanary builds the canary over the database the evidence is
// in. A *pgxpool.Pool satisfies db, and so does a transaction, which is what
// lets the check be exercised against one that is rolled back.
func NewAttributionCanary(db store.DBTX) (*AttributionCanary, error) {
	if db == nil {
		return nil, ErrNoCanaryStore
	}
	return &AttributionCanary{queries: store.New(db)}, nil
}

// AttributionVerdict is what the count found, and what it means.
type AttributionVerdict struct {
	// Network is the network the counts are about.
	Network NetworkID
	// FirstClickAt is when Apivo first sent a member through this network.
	// Zero when it never has, in which case there is nothing to judge and
	// both counts below are meaningless.
	FirstClickAt time.Time
	// Attributed is how many distinct transactions have ever been credited
	// to a click at this network. One is enough: the reference has made
	// the round trip and the parameter is right.
	Attributed int64
	// Unattributed is how many distinct transactions dated after the first
	// click have gone unattributed - reported with no reference, or with
	// one that matched no click.
	Unattributed int64
}

// Idle reports that no member has yet clicked through this network, so the
// question has no answer yet.
func (v AttributionVerdict) Idle() bool { return v.FirstClickAt.IsZero() }

// Retired reports that a reference has round-tripped at least once. The
// canary has done its job and says nothing further about this network.
func (v AttributionVerdict) Retired() bool { return v.Attributed > 0 }

// Suspect reports the state the canary exists to catch: members have
// clicked, enough purchases have been reported to rule out chance, and not
// one has been credited to a click.
func (v AttributionVerdict) Suspect() bool {
	return !v.Idle() && !v.Retired() && v.Unattributed >= AttributionCanaryThreshold
}

// Attribution canary states, in the order [AttributionVerdict.State] tests
// them. Four, because "watching" - clicked through, nothing back yet, not
// enough of them to rule out chance - is a real state and not the absence of
// the other three.
const (
	// AttributionIdle: nobody has clicked through this network yet.
	AttributionIdle = "idle"
	// AttributionRetired: a click reference has round-tripped, so
	// attribution demonstrably works here and the canary is done.
	AttributionRetired = "retired"
	// AttributionSuspect: enough references have come back unmatched, and
	// none matched, that chance no longer explains it.
	AttributionSuspect = "suspect"
	// AttributionWatching: clicked through, nothing matched yet, not enough
	// unmatched to judge.
	AttributionWatching = "watching"
)

// State names which of the four this verdict is.
//
// One definition, read by both the log line and the measurement, so the two
// cannot come to disagree about what "watching" means - which they would, the
// first time somebody adjusted one switch and not the other.
func (v AttributionVerdict) State() string {
	switch {
	case v.Idle():
		return AttributionIdle
	case v.Retired():
		return AttributionRetired
	case v.Suspect():
		return AttributionSuspect
	default:
		return AttributionWatching
	}
}

// Check counts, and refuses with [ErrAttributionNeverSucceeded] when the
// counts say attribution has never worked. The verdict comes back alongside
// either way, so a caller can log what was found rather than only whether
// it was refused.
//
// A count that could not be taken is a failure of the CANARY and is
// returned as such, never as a clean bill: the silent pass is the one
// outcome this check exists to prevent.
func (c *AttributionCanary) Check(ctx context.Context, network NetworkID) (AttributionVerdict, error) {
	if err := network.Validate(); err != nil {
		return AttributionVerdict{}, err
	}
	row, err := c.queries.CountAttributionOutcomes(ctx, network.String())
	if err != nil {
		return AttributionVerdict{}, fmt.Errorf("networks: the attribution canary could not count for %s, which is a failure of the canary and never a clean bill: %w", network, err)
	}
	verdict := AttributionVerdict{
		Network:      network,
		Attributed:   row.Attributed,
		Unattributed: row.UnattributedSinceFirstClick,
	}
	if row.FirstClickAt.Valid {
		verdict.FirstClickAt = row.FirstClickAt.Time.UTC()
	}
	if !verdict.Suspect() {
		return verdict, nil
	}
	// Everything an operator needs is in the one line the scheduler will
	// log, because that line is the whole of the alarm. The remedy is
	// named, and so is the issue that says how to confirm it.
	return verdict, fmt.Errorf("%w: %d distinct transaction(s) dated after the first click (%s) went unattributed and 0 were ever credited to a click; the click reference is not reaching %s or is being cut short on the way back - check cashback.network.click_ref_param and the maximum length of that parameter (#524); the sweep itself succeeded and the reports are in the unattributed queue",
		ErrAttributionNeverSucceeded, verdict.Unattributed, verdict.FirstClickAt.Format(time.RFC3339), network)
}
