// Package fixture implements the [networks.Network] port against recorded
// network responses held in testdata rather than against a live publisher
// account. It is the adapter that un-blocks the build from the network
// decision (research Q1): the founder's choice of affiliate network is a
// business process with an unknown lead time, and until it lands there is no
// credential anywhere in this repository - so without a working adapter the
// poller, the supersede logic, the earnings path and the click-out redirect
// would all be written against nothing.
//
// It is an adapter, not a stub. Every one of the port's nine contract rules
// applies to it exactly as it applies to a real network's package: the
// payload it yields is the verbatim bytes of a recorded response, its status
// mapping is total and refuses a word nobody mapped, it refuses a window
// wider than the limits it declares, it classifies its failures, it says so
// when it stops early, and it never writes a row. What differs from a real
// adapter is only where the bytes come from - a file instead of a socket -
// and that difference is confined to one file (recording.go). A conformance
// suite that passes against this package is therefore evidence about the
// port, not about a mock somebody wrote to agree with it.
//
// # What is recorded
//
// One transaction is observed across four successive polls, which is the
// lifecycle the whole ingestion chain exists to handle: click, pending,
// approved, reversed ([Stage]). The network reports the sale before it has
// joined it to a click reference, so the transaction first appears
// unattributed and gains its reference on the next poll (FR-034 makes an
// unattributed report ordinary rather than an error); it then validates, and
// is finally taken back. A second transaction shares the window, is never
// attributed at all, is re-reported once completely unchanged, and then
// changes its status and both its amounts at once - which is what US2
// scenario 3 (no duplicate) and scenario 4 (a superseding record) need to be
// demonstrated end to end against something.
//
// Everything else recorded here is deliberate awkwardness, because a fixture
// that models only the happy path lets every later bug through: the
// transactions of one window arrive on different pages, the last page of the
// final observation is empty as a real network's last page usually is, the
// two transactions are denominated in different currencies, one merchant is
// bound to no country, and a status word nobody mapped is reachable through
// [WithUnmappableStatus] so contract rule 2's totality can be proved against
// a real adapter rather than asserted about a mock.
//
// # The clock is polls, not wall time
//
// Which of the four observations a read returns is the fixture's own state,
// advanced by each read that runs to completion ([Network.Stage]). Wall time
// would be useless here: a network takes up to 90 days to validate a
// transaction (ADR-0003), and a test that had to wait for one would never be
// written. Contract rule 4 is what this implements - re-issuing a window asks
// the same question and returns the network's account of that period as it
// stands NOW - and it is the only mechanism by which a pending transaction is
// ever seen to become confirmed. An adapter that answered a re-read from a
// memo would satisfy resumability to the letter and freeze every member's
// money at pending, so a fixture that did the same would prove the poller
// worked when it did not.
//
// The advance is on COMPLETION, never on the call. A caller that breaks out
// of the range - because a write failed, or the process is shutting down -
// gets the same observation again when it runs that window from the
// beginning, which is the other half of rule 4 and the reason a durable
// cursor may only move on a window that was fully persisted (FR-031).
//
// # Failures on demand
//
// [Network.SetFailure] makes the next reads report [networks.ErrNetworkUnavailable],
// [networks.ErrNetworkRateLimited] or [networks.ErrNetworkRefused], which is
// how the retry paths of the conformance suite (T051), the limiter (T056) and
// the poller get driven with no network to be unwell. The three are the
// port's contract rule 9 classification, and the distinction is the whole
// point: two of them mean run the window again, and the third means stop and
// raise the account to an operator, because re-running it would loop forever
// against a credential nobody has fixed.
//
// # What this package must not do
//
// Contract rule 6: adapters translate, and nothing else. This package opens
// no database transaction, writes no row and decides no credit - it reads
// bytes it was compiled with and turns them into the port's values.
// TestFixtureNamesNoDatabase in port_test.go reads this package's own imports
// and refuses a driver or a generated store, which is the same narrow,
// structural rule the port keeps on itself.
//
// # Money
//
// The recorded responses state amounts as int64 minor units beside an
// explicit ISO-4217 code, rather than as the decimal strings a real network
// tends to send. That is a deliberate limit on what this package pretends to
// be: constitution C-6 admits no float and no decimal string, this repository
// therefore ships no decimal parser (internal/platform/money), and a fixture
// that invented one would be the place a rounding error entered a product
// built specifically to have none. An adapter for a network that really does
// send "49.99" owns that parsing problem in its own package, where the
// network's own quirks belong (ADR-0003).
package fixture
