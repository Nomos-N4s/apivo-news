// The adapter itself: what a fixture [Network] is made of, the facts it
// states about the network it speaks for, and the knobs a caller turns to put
// it in the state a test needs. [New] is deliberately not here - see port.go
// for why it cannot be.

package fixture

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// ID is the network id this adapter answers with, and the id an operator
// wires a fixture publisher account under. It is a constant rather than
// configuration because the id is what a stored evidence row is traced back
// to: a deployment that could rename it would have rows attributed to a
// network nobody can find the code for.
//
// It is exported so a caller can build the account this adapter requires -
// [networks.ValidateNetwork] refuses an adapter whose account is held at
// another network, and a caller that had to guess the word would find that
// out at wiring rather than at compile time.
const ID networks.NetworkID = "fixture"

// RecordedClickRef is the click reference the recording echoes back against
// the lifecycle transaction from [StagePending] onwards. It is exported so
// that an end-to-end demonstration - issue a redirect, poll, match the report
// to the click - can mint the reference the recording is about to report,
// which is the only way the attribution half of the chain can be shown
// working before a real network exists.
//
// The second recorded transaction is never attributed at all, deliberately:
// FR-034 makes an unattributed report ordinary rather than an error, and a
// recording in which everything matched would let the branch that must store
// one without crediting anybody go untested.
const RecordedClickRef = "Zml4dHVyZS1jbGljay0wMDAwMDAwMQ"

const (
	// deeplinkTemplate is the route the recording's own retailer is published
	// on, in the shape offer.deeplink_template holds. The host is under
	// .invalid (RFC 2606), which is guaranteed never to resolve: a fixture
	// that shipped a resolvable host would eventually be the reason a test
	// suite sent traffic somewhere.
	//
	// It already carries a query parameter of its own, because that is the
	// case worth having in a fixture - an adapter that only ever appended to
	// a bare path builds a working URL right up to the first operator who
	// pastes in a template with a campaign id on it.
	deeplinkTemplate = "https://track.fixture.invalid/c/9f3?merchant=42"
	// clickRefParam is the query parameter this network reads the click
	// reference from (FR-021). It is the reference network's own name for it
	// (ADR-0003), and it reaches [BuildDeeplink] through the target rather
	// than as a literal, because it is per-network configuration and a wrong
	// value silently loses attribution on every click.
	clickRefParam = "clickref"
)

// Network is the fixture implementation of [networks.Network]. Build one with
// [New]; the zero value carries no recording and no account, and is not
// usable.
//
// One adapter serves one publisher account, exactly as a real one does
// (contract rule 10). Two fixture adapters in a process are two accounts at
// the same network, they share the recording, and each keeps its own place in
// the lifecycle - which is the arrangement a poller keyed by
// [networks.PublisherAccount.ID] has to be exercised against.
//
// The two pieces of mutable state - where the lifecycle has got to, and what
// failure has been injected - carry their own locks and are described where
// they are declared. Everything else is fixed at construction, so the fields
// below can be read without one.
type Network struct {
	// account is the publisher account this adapter polls, and the adapter's
	// identity.
	account networks.PublisherAccount
	// limits is what this adapter declares about how it may be queried
	// (contract rule 3).
	limits networks.Limits
	// recorded is the shared, immutable set of responses this adapter serves.
	recorded *recording
	// unmappable is whether [WithUnmappableStatus] was asked for.
	unmappable bool

	// clock holds which observation the next completed read answers from.
	clock stageClock
	// failures holds what to report instead of answering from the recording.
	failures injector
}

// The sentinel an option that cannot configure this adapter is refused with.
// Its rule is that a knob handed a value the recording has no answer for is a
// mistake in the test that turned it, and is reported as one.
var (
	// ErrOptionRefused reports an option that does not describe a state this
	// adapter can be built in: a stage outside the recorded lifecycle, or an
	// option that is nil. It is separate from [ErrRecordingUnreadable]
	// because the two send an investigation to opposite places - one says the
	// caller asked for something the recording never held, the other says the
	// recording itself is broken - and a single error for both would have
	// every failing test start by re-reading the testdata.
	ErrOptionRefused = errors.New("fixture: option does not describe a state this adapter can be built in")
)

// Option configures a [Network] at construction. It returns an error so that
// a knob handed a value it cannot use - a stage outside the recorded
// lifecycle - is refused where the mistake was made, rather than at the first
// read, where it would look like the recording being wrong.
type Option func(*Network) error

// WithLimits replaces what the adapter declares about how it may be queried.
//
// The default is the reference network's documented pair (ADR-0003): a 31-day
// maximum query window and 6 requests per second. A test about window
// clamping wants to shrink the first so it can ask for something too wide
// without constructing a 32-day window, and the limiter's tests (T056) want
// to lower the second; both are declarations the adapter holds itself to, so
// changing them changes what the adapter refuses rather than what it fetches.
func WithLimits(limits networks.Limits) Option {
	return func(n *Network) error {
		n.limits = limits
		return nil
	}
}

// WithStage starts the adapter partway through the recorded lifecycle, so a
// test about confirmed money does not have to poll its way there first.
//
// It refuses a stage the recording does not have. A Stage is an int, and an
// adapter started outside the four would index past the recording on its
// first read - a panic in a package whose whole job is to be the thing other
// packages are tested against.
func WithStage(stage Stage) Option {
	return func(n *Network) error {
		if !stage.Valid() {
			return fmt.Errorf("%w: no observation was recorded for %s", ErrOptionRefused, stage)
		}
		n.clock.set(stage)
		return nil
	}
}

// WithFailure makes the adapter report a network failure instead of answering
// from the recording, for the next times network-touching calls, where
// [FailureAlways] means until [Network.SetFailure] says otherwise. It is the
// construction-time form of that method; see [Failure] for why the three
// kinds are named separately.
func WithFailure(kind Failure, times int) Option {
	return func(n *Network) error {
		n.failures.set(kind, times)
		return nil
	}
}

// WithUnmappableStatus makes the adapter serve one further recorded page, on
// which a transaction and a merchant carry status words its mapping tables
// have no entry for.
//
// It exists so that contract rule 2's totality can be PROVED rather than
// asserted. Every adapter claims its mapping is total; the claim is only
// worth something if a caller can make a real adapter meet a word nobody
// mapped and watch it return [networks.ErrUnmappableStatus] instead of
// guessing. Doing that against a mock proves only that the mock was written
// to agree, which is why the knob is here rather than in the conformance
// suite.
//
// The page comes last, after the reports that mapped cleanly, because that is
// the shape of the real failure: a network invents a word part way through a
// window that was otherwise being read correctly.
func WithUnmappableStatus() Option {
	return func(n *Network) error {
		n.unmappable = true
		return nil
	}
}

// newAdapter assembles a fixture adapter: the shared recording, the reference
// limits, then whatever the options change. It is separate from [New] because
// New's extra job - handing the finished adapter to
// [networks.ValidateNetwork], which is what the composition root would do -
// mentions the whole port at once, and so cannot be written until every
// method the port names exists (see port.go). Splitting the two is what lets
// this file and its tests stand on their own.
func newAdapter(account networks.PublisherAccount, opts ...Option) (*Network, error) {
	recorded, err := loadRecording()
	if err != nil {
		return nil, err
	}
	adapter := &Network{
		account:  account,
		limits:   defaultLimits(),
		recorded: recorded,
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: a nil option was passed to New", ErrOptionRefused)
		}
		if err := opt(adapter); err != nil {
			return nil, err
		}
	}
	return adapter, nil
}

// defaultLimits are the reference network's documented numbers (ADR-0003): a
// 31-day maximum query window, which is why backfill is inherently windowed,
// and 6 requests a second. They are a function rather than a package
// variable because a [networks.Limits] handed out from a shared variable is a
// value one test can edit for every other one.
func defaultLimits() networks.Limits {
	const maxQueryWindowDays = 31
	return networks.Limits{
		MaxWindow:         maxQueryWindowDays * 24 * time.Hour,
		RequestsPerSecond: 6,
	}
}

// ID names the network this adapter speaks to. It is constant for the life of
// the adapter, as the port requires: the id is how a stored row is traced
// back to the code that wrote it.
func (n *Network) ID() networks.NetworkID { return ID }

// Account names the publisher account this adapter polls, which is the
// adapter's identity, what the poller keys its registry by, and what every
// evidence row this adapter's poll produces carries in network_account_id.
func (n *Network) Account() networks.PublisherAccount { return n.account }

// Limits states what this network documents about how it may be queried
// (contract rule 3). It takes no context and returns no error for the reason
// the port gives: an adapter whose declaration could not be read is never
// built.
func (n *Network) Limits() networks.Limits { return n.limits }

// Stage reports which observation the next completed read of a transaction
// window will answer from. It is the fixture's clock, and it is readable so
// that a test can say what it is asserting about - "at the approved poll" -
// rather than counting reads and hoping.
func (n *Network) Stage() Stage { return n.clock.now() }

// SetFailure tells the adapter to report kind on its next times
// network-touching calls, where [FailureAlways] means every call until
// SetFailure is called again, and [FailureNone] clears the injection.
//
// It is a setter on a live adapter, which no real adapter would have, and it
// is here because the retry paths cannot be driven any other way: "fail
// twice, then answer" is the behaviour a backoff loop exists for, and it
// cannot be expressed by constructing a different adapter halfway through the
// test that is watching this one.
//
// A failure is reported through whichever channel the call has. The two
// iterators YIELD it rather than returning it immediately, because contract
// rule 9 says the immediate error carries only what is checkable without
// contacting the network - so a caller cannot tell an eager adapter from a
// lazy one by where an expired credential surfaced.
func (n *Network) SetFailure(kind Failure, times int) { n.failures.set(kind, times) }

// DeeplinkTarget is the route the recording's retailer is published on, as
// the offer row would carry it: this adapter's network, the click-reference
// parameter it reads, and a template.
//
// It is here because a unit test has no catalogue row to read a route out of,
// and a caller left to invent one would invent the template that happens to
// work - a bare path with no query of its own - and never exercise the case
// [BuildDeeplink] actually has to get right. The offer id is the caller's,
// because it is the click's own band and nothing about the redirect is built
// from it.
func (n *Network) DeeplinkTarget(offerID uuid.UUID) networks.DeeplinkTarget {
	return networks.DeeplinkTarget{
		OfferID:       offerID,
		NetworkID:     ID,
		ClickRefParam: clickRefParam,
		Template:      deeplinkTemplate,
	}
}
