// [QueryWindow], the period a transaction query covers, and [Limits], what a
// network documents about how it may be asked. One file, because contract
// rule 3's checkable half is a window judged against limits and neither half
// decides anything alone.

package networks

import (
	"errors"
	"fmt"
	"time"
)

// The sentinels a query window and a set of limits are refused with. A
// window wider than the network allows is contract rule 3's half that a type
// can check.
var (
	// ErrInvalidQueryWindow reports a query window that cannot be asked for:
	// one missing a bound, or one whose end lies before its start. Both
	// bounds are required, unlike a ledger history window, because a network
	// query window is the unit of resumption (contract rule 4): an open-ended
	// window cannot be clamped against [Limits], cannot be re-issued as the
	// same question, and cannot be recorded as fully persisted. The name
	// spells out "query" because internal/cashback/wallet has a Window of its
	// own with the opposite zero value, and the poller touches both.
	ErrInvalidQueryWindow = errors.New("networks: query window is not a window a network can be asked for")
	// ErrWindowTooWide reports a window wider than the network documented
	// (contract rule 3, FR-031). It is refused rather than trimmed: a caller
	// that asked for 90 days and silently received 31 believes it has read a
	// period it has not, and would advance its cursor past the 59 days it
	// never saw. Backfill splits the period into windows the network allows,
	// and each is persisted before the cursor moves.
	ErrWindowTooWide = errors.New("networks: query window is wider than the network allows")
	// ErrInvalidLimits reports limits that describe no usable network: a
	// non-positive maximum window, or a non-positive request rate. Both
	// mirror the network table's own checks
	// (network_max_query_window_positive, network_rate_limit_positive). Zero
	// is the dangerous value rather than an obviously silly one, because it
	// is what an unset struct carries: without this refusal a forgotten
	// Limits would make every window too wide and every request unpermitted,
	// and the ingestion would simply stop with no operator ever told why.
	ErrInvalidLimits = errors.New("networks: limits describe no usable network")
)

// QueryWindow is the period a transaction query covers: half-open, taking
// every transaction with From <= TransactedAt < To.
//
// Half-open is what lets adjacent windows partition a backfill with no
// transaction counted twice and none falling in a seam, which is half of
// FR-031's "never loses or double-counts a window". The other half is the
// cursor, which advances only after a window is fully persisted, and lives
// on the publisher account rather than here.
//
// Both bounds are required, unlike a ledger history window where an open end
// means "everything since". A network query window is the unit of
// resumption: contract rule 3 clamps its width against [Limits], and
// contract rule 4 promises that re-issuing the same window asks the same
// question. An unbounded window can satisfy neither - there is no width to
// clamp, and "everything up to now" is a different question every time it is
// asked.
//
// The name is deliberately not Window. internal/cashback/wallet has a Window
// of its own, reachable from the same poller, whose zero bounds mean the
// OPPOSITE of these: there a zero From is "from the first posting" and a zero
// To is "no upper bound", while here either is refused. Two same-named types
// with inverted zero values, one import line apart, is a mistake worth making
// unspellable.
type QueryWindow struct {
	// From is the inclusive lower bound.
	From time.Time
	// To is the exclusive upper bound.
	To time.Time
}

// Validate reports whether the window is one a network can be asked for,
// returning an error wrapping [ErrInvalidQueryWindow] for a missing bound or
// an end before its start. An empty-but-ordered window (From equal to To) is
// legal: it is a genuine question with a genuinely empty answer, and a
// backfill that has caught up asks it.
func (w QueryWindow) Validate() error {
	if w.From.IsZero() || w.To.IsZero() {
		return fmt.Errorf("%w: %s", ErrInvalidQueryWindow, w)
	}
	if w.To.Before(w.From) {
		return fmt.Errorf("%w: %s ends before it starts", ErrInvalidQueryWindow, w)
	}
	return nil
}

// Duration is how wide the window is, which is what [Limits.ValidateWindow]
// judges. It is only meaningful for a window that has passed
// [QueryWindow.Validate] - a missing bound makes it a distance from the year
// 1 - and a span too large for a time.Duration saturates rather than
// wrapping, so an absurd window reads as absurdly wide rather than as
// negative.
func (w QueryWindow) Duration() time.Duration { return w.To.Sub(w.From) }

// Contains reports whether an instant falls inside the window: at or after
// From, strictly before To. It is the single definition of membership,
// shared so that every adapter filters identically and the conformance suite
// can hold them to it - an adapter whose network returns a transaction
// outside the window it was asked for is a bug worth catching in one place
// rather than in each poller.
func (w QueryWindow) Contains(at time.Time) bool {
	return !at.Before(w.From) && at.Before(w.To)
}

// String renders the window for errors and test failures, in UTC so two
// windows built in different zones compare by eye.
func (w QueryWindow) String() string {
	return w.From.UTC().Format(time.RFC3339) + " to " + w.To.UTC().Format(time.RFC3339)
}

// Limits is what a network documents about how it may be queried: how wide a
// transaction window it will answer, and how many requests a minute it will
// accept. Awin's numbers - 31 days and 20 requests per minute - are the
// reference case (ADR-0003), and both are stored per network
// (max_query_window_days, rate_limit_per_minute) rather than compiled in.
//
// The rate is per MINUTE and not per second, in the unit the column carries
// and the networks themselves publish. A per-second integer cannot express
// Awin's published limit at all: twenty a minute is a third of a request a
// second, and the smallest positive integer in that unit is three times too
// fast. Migration 0026 moved the column for that reason; a declaration in
// the old unit would have reintroduced the same bug one layer up, where it
// would look like a correctly-read row.
//
// The port declares the limits; it does not enforce the rate. Window width
// is refused here because it is a property of one call and can be judged
// before the call is made, while a request rate is a property of a stream of
// calls over time and belongs to the limiter the adapter is built with (that
// limiter is T056's, and until it exists the rate half of contract rule 3 is
// declared and not yet held). Both halves are contract rule 3, and an adapter
// satisfies the second by holding its limiter to this declaration rather than
// to a number of its own.
type Limits struct {
	// MaxWindow is the widest transaction query the network will answer.
	// The network table stores it as whole days; it is a duration here
	// because a [QueryWindow] is bounded by instants, and it is instants
	// that have to be compared. A deployment converting the column
	// multiplies by 24 hours, and accepts that a "day" so measured does not
	// follow a clock change - which is the safe direction, since the
	// arithmetic can only ever refuse a window the network might have
	// allowed.
	MaxWindow time.Duration
	// RequestsPerMinute is the documented request rate, as the network
	// states it and the network table stores it (rate_limit_per_minute). It
	// is what the adapter's limiter is configured from; nothing in this file
	// consumes it, because no single call can be judged against a rate.
	RequestsPerMinute int
	// ReportingLag is how far behind the present this network's answers
	// are: the age a transaction must reach before the network will report
	// it at all. Zero means the network answers up to the moment, which is
	// what every adapter declared before this field existed.
	//
	// It exists because the forward sweep asks for a window ending now, and
	// a network that is behind answers cleanly and emptily for the part it
	// has not reached. The cursor then advances past ground nobody has
	// reported, and only the trailing sweep revisits it - roughly a hundred
	// days later. Nothing is lost, but a member waits a quarter for a credit
	// that was earned today, which is SC-001 broken in the one way no error
	// stream shows.
	//
	// UNLIKE the two fields above, zero is the ORDINARY value here, not the
	// dangerous one. Those refuse zero because an unset Limits would make
	// every window too wide and every request unpermitted; this one is
	// simply the absence of a lag. Negative is what is refused, because a
	// network cannot report the future.
	ReportingLag time.Duration
}

// Validate reports whether the limits describe a network that can be
// queried at all, returning an error wrapping [ErrInvalidLimits] when a
// bound is not positive.
//
// Zero is the value worth refusing, and not because anybody would write it
// deliberately: it is what an unset Limits carries. A forgotten declaration
// would otherwise make every window too wide and every request unpermitted,
// and ingestion would stop dead with no operator ever told why.
func (l Limits) Validate() error {
	if l.MaxWindow <= 0 {
		return fmt.Errorf("%w: maximum query window of %s", ErrInvalidLimits, l.MaxWindow)
	}
	if l.RequestsPerMinute <= 0 {
		return fmt.Errorf("%w: request rate of %d per minute", ErrInvalidLimits, l.RequestsPerMinute)
	}
	if l.ReportingLag < 0 {
		return fmt.Errorf("%w: reporting lag of %s, and a network cannot report the future", ErrInvalidLimits, l.ReportingLag)
	}
	return nil
}

// ValidateWindow is contract rule 3's checkable half: the window is one a
// network can be asked for, and it is no wider than this network documented.
// Adapters call it at the top of FetchTransactions, before any I/O, so a
// caller learns its window is too wide instead of learning it from a
// network's error page - or worse, from a truncated answer.
//
// The width is refused rather than clamped. A caller that asked for 90 days
// and silently received 31 believes it has read a period it has not, and
// advances its cursor past 59 days of transactions it never saw - which is
// precisely the loss FR-031 forbids. Backfill splits the period into windows
// the network allows and persists each before the cursor moves.
//
// The limits themselves are checked first, so an adapter built with an unset
// Limits is reported as the misconfiguration it is rather than as a caller
// asking for too much. That order is load-bearing: a zero MaxWindow makes
// every window too wide, and reporting an operator's forgotten row as
// "you asked for more than the network allows" sends the investigation to the
// caller instead of to the configuration.
func (l Limits) ValidateWindow(w QueryWindow) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := w.Validate(); err != nil {
		return err
	}
	if w.Duration() > l.MaxWindow {
		return fmt.Errorf("%w: %s spans %s, and the network allows %s",
			ErrWindowTooWide, w, w.Duration(), l.MaxWindow)
	}
	return nil
}
