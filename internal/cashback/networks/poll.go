// What one poll reads and what it did: the window arithmetic, kept pure and
// apart from the transaction that acts on it (T055, FR-031).

package networks

import (
	"fmt"
	"time"
)

// PollOutcome counts what became of the reports one window carried. It is
// the poll's answer to "was this worth doing", and the three numbers say
// different things to an operator: first reports are new money to attribute,
// superseded ones are money that has changed, and unchanged ones are the
// bulk of every trailing re-read and mean the network has nothing new to say.
type PollOutcome struct {
	// FirstReports is transactions the network had never reported before.
	FirstReports int
	// Superseded is transactions whose facts changed, each now a new row
	// naming the one it replaced.
	Superseded int
	// Unchanged is reports whose facts matched a row already stored. Nothing
	// was written for them and nothing is wrong.
	Unchanged int
	// Unattributed is how many of this window's stored reports arrived with
	// no click reference and are now recorded for an operator (FR-034).
	//
	// It counts a subset of FirstReports and Superseded rather than a fourth
	// kind, so [PollOutcome.Reports] deliberately does not add it and
	// [PollOutcome.Changed] deliberately does not mention it: every report
	// counted here already changed what somebody may be owed. It is here so
	// an operator reading one poll's line can see money arriving that
	// nobody can be credited for yet.
	Unattributed int
}

// Reports is how many the window carried in total.
func (o PollOutcome) Reports() int { return o.FirstReports + o.Superseded + o.Unchanged }

// Changed reports whether this poll altered what anybody is owed. It is what
// decides whether the work that follows a poll - earnings, events - has
// anything to do (T062).
func (o PollOutcome) Changed() bool { return o.FirstReports > 0 || o.Superseded > 0 }

func (o PollOutcome) String() string {
	return fmt.Sprintf("%d report(s): %d first, %d superseded, %d unchanged, %d unattributed",
		o.Reports(), o.FirstReports, o.Superseded, o.Unchanged, o.Unattributed)
}

// Poll is what one poll decided and did. A poll that found nothing to read
// is [Poll.Ran] false and is not a failure: a cursor that has caught up with
// the present, or a trailing sweep with nothing old enough to be worth
// re-reading, are both the ordinary steady state.
type Poll struct {
	// Ran reports whether there was a window to read at all.
	Ran bool
	// Window is the period that was read, when one was.
	Window QueryWindow
	// Outcome counts what the window's reports became.
	Outcome PollOutcome
	// CursorAdvancedTo is where the durable cursor now stands. It equals
	// Window.To on a completed poll and is zero when nothing ran, because a
	// cursor is advanced only after the whole window is persisted (FR-031).
	CursorAdvancedTo time.Time
}

func (p Poll) String() string {
	if !p.Ran {
		return "nothing to read"
	}
	return fmt.Sprintf("%s: %s", p.Window, p.Outcome)
}

// nextForwardWindow is the next period nobody has read, or false when the
// cursor has caught up with the present.
//
// A window never ends in the future. A network cannot report what has not
// happened, and a cursor advanced past the present would carry it over a
// period whose transactions arrive afterwards - which would then never be
// read again by the forward sweep. That is precisely the gap the trailing
// re-read exists to close, and it can only close it if the forward cursor
// stops at now rather than beyond it.
//
// A cursor already at or ahead of now yields nothing rather than a backwards
// window. Ahead is possible: clocks move, and a restored backup or a
// mis-set clock would otherwise produce a window whose end precedes its
// start, which the port refuses and the poller would report as a defect in
// the adapter rather than in the clock.
func nextForwardWindow(cursor time.Time, cursorSet bool, backfillFrom, now time.Time, maxWindow, reportingLag time.Duration) (QueryWindow, bool) {
	from := backfillFrom
	if cursorSet {
		from = cursor
	}
	// The horizon is as far as this network has reported, which is not
	// always now. Ending the window at now instead would ask a lagging
	// network about ground it has not covered; it would answer cleanly and
	// emptily, and the cursor would advance past transactions nobody has
	// reported yet. Only the trailing sweep would ever come back for them,
	// about a hundred days later.
	//
	// Stopping short is the same discipline as refusing a too-wide window
	// rather than clamping it: the cursor may only pass ground that has
	// actually been read. A lag of zero leaves this exactly as it was.
	horizon := now.Add(-reportingLag)
	if !horizon.After(from) {
		return QueryWindow{}, false
	}
	to := from.Add(maxWindow)
	if to.After(horizon) {
		to = horizon
	}
	return QueryWindow{From: from, To: to}, true
}

// nextTrailingWindow is the next period old enough to be worth re-reading,
// or false when nothing has aged past the lag yet.
//
// Validation takes up to 90 days (ADR-0003), so a transaction read as
// pending is confirmed long after the forward cursor has moved past it. The
// trailing sweep walks the same ground again, once, at a distance: it reads
// only what the main cursor passed at least `lag` ago, so by the time a
// period is re-read the network has had that long to make up its mind.
//
// It walks FORWARD and never wraps. A sweep that cycled over a fixed
// trailing span would re-read the same recent windows again and again while
// older ones aged out of it unseen; walking forward re-reads every period
// exactly once, at the right remove, and needs no state beyond the cursor
// the schema already carries. It also keeps the schema's own rule true
// without restating it - the end never reaches the main cursor, let alone
// passes it.
//
// It yields nothing until the account has been polled forward at least once.
// A trailing cursor with no main cursor is a re-read of history that was
// never read, which the schema refuses outright.
func nextTrailingWindow(trailing time.Time, trailingSet bool, backfillFrom, cursor time.Time, cursorSet bool, lag, maxWindow time.Duration) (QueryWindow, bool) {
	if !cursorSet {
		return QueryWindow{}, false
	}
	from := backfillFrom
	if trailingSet {
		from = trailing
	}
	// Only ground the forward sweep passed at least `lag` ago is old enough.
	horizon := cursor.Add(-lag)
	if !horizon.After(from) {
		return QueryWindow{}, false
	}
	to := from.Add(maxWindow)
	if to.After(horizon) {
		to = horizon
	}
	return QueryWindow{From: from, To: to}, true
}
