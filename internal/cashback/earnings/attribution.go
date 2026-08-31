// Resolving what a network reported back to the click that earned it (T067).
//
// This is the step that decides whether a purchase belongs to a member. It
// reads and records; it never credits. What a match is worth is T068's
// question and what it becomes is T069's, and keeping those apart is what
// stops a mis-resolved reference from being able to move money on its own.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

var (
	// ErrNoClicks reports a matcher built with nothing to resolve references
	// against. Refused at construction for the reason [ErrNoUnmatchedStore]
	// is: every report it was then handed would queue as unattributed, which
	// is a silent, plausible and entirely wrong answer.
	ErrNoClicks = errors.New("earnings: matching a reference needs somewhere to look a click up")
	// ErrNoReference reports a match asked about a report the network
	// attached no reference to.
	//
	// A refusal rather than an unattributed answer, because that report is
	// the networks module's to queue and it has already queued it. Answering
	// "unattributed" here would be true and would still be a defect: one
	// purchase would take two queue rows and an operator would see the same
	// money twice.
	ErrNoReference = errors.New("earnings: the report carries no reference to match")
)

// Report is the stored report a match is asked about: which evidence row,
// and what the network attached to it.
//
// It carries the reference rather than being looked up, because the caller
// translating a window already holds both and a second read would be a
// second answer. The type is [networks.ClickRef] - what a network ECHOED,
// never what Apivo minted - so "the network reported nothing" is a state the
// caller has to have handled rather than an empty string.
type Report struct {
	// ID is the evidence row, and what a queue row or an entry cites.
	ID uuid.UUID
	// Ref is the reference the network echoed against it.
	Ref networks.ClickRef
}

// Attribution is what one report resolved to.
type Attribution struct {
	// Report is the evidence row this answers about.
	Report uuid.UUID
	// Click is the click the reference named, and is meaningful only when
	// Matched. It carries the click-time rate snapshot, which is what governs
	// the credit (FR-013) - not the offer as it stands when the money is paid.
	Click clickout.Click
	// Matched says whether the reference named a click at all.
	Matched bool
	// Queued is the queue row written when nothing matched. It is the zero
	// uuid when the observation was already recorded, which is ordinary after
	// a crash and is not an error.
	Queued uuid.UUID
}

// Clicks is the one read this file needs. Named here rather than taken as
// *clickout.Clicks so a caller can supply its own transaction's view, and so
// this package states what it depends on rather than importing a shape.
type Clicks interface {
	ByRef(ctx context.Context, reported networks.ClickRef) (clickout.Click, error)
}

// Matcher resolves a reported reference to the click that earned it, and
// records a reference that matched nothing as unattributed (FR-034).
type Matcher struct {
	clicks    Clicks
	unmatched UnmatchedStore
}

// NewMatcher builds the matcher, refusing one that is missing a part.
func NewMatcher(clicks Clicks, unmatched UnmatchedStore) (*Matcher, error) {
	if clicks == nil {
		return nil, ErrNoClicks
	}
	if unmatched == nil {
		return nil, ErrNoUnmatchedStore
	}
	return &Matcher{clicks: clicks, unmatched: unmatched}, nil
}

// Match answers the click a report's reference names, queueing the report as
// unattributed when nothing answers to it.
//
// A reference matching nothing is ORDINARY, not exceptional: networks echo
// references minted by other publishers and by links that predate a
// deployment, so the miss is a routine outcome with a routine handling. It is
// reported as a value rather than an error precisely so a caller processing a
// window cannot mistake it for a failure and stop.
//
// The queue write happens here rather than being left to the caller because
// the two are one decision. A caller that resolved a reference, found
// nothing, and forgot the second call would leave money in no queue at all,
// and nothing downstream would ever notice - the report simply would not be
// credited and nobody would be looking for it.
func (m *Matcher) Match(ctx context.Context, report Report) (Attribution, error) {
	if _, present := report.Ref.Ref(); !present {
		return Attribution{}, fmt.Errorf("%w: %s", ErrNoReference, report.ID)
	}

	click, err := m.clicks.ByRef(ctx, report.Ref)
	switch {
	case errors.Is(err, clickout.ErrNoSuchClick):
		queued, _, err := queueUnmatched(ctx, m.unmatched, report.ID)
		if err != nil {
			return Attribution{}, err
		}
		return Attribution{Report: report.ID, Queued: queued.ID}, nil
	case err != nil:
		// A read that FAILED is not a read that found nothing. Queueing here
		// would turn a dropped connection into a permanent record that this
		// purchase went unattributed - a record 0013 freezes and nothing
		// later re-examines.
		return Attribution{}, fmt.Errorf("earnings: resolving %s for report %s: %w", report.Ref, report.ID, err)
	}
	return Attribution{Report: report.ID, Click: click, Matched: true}, nil
}
