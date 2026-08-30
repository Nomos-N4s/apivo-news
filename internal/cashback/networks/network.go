// The [Network] port: what the cashback domain requires of an affiliate
// network, and the publisher account one adapter speaks for. The contract
// each implementation is held to is stated on the interface and its methods
// below; contracts/ports.md section 2 is the specification those doc
// comments implement.

package networks

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Sentinel errors for the port's own surface: an adapter that cannot say
// which publisher account it speaks for. The package-wide convention these
// follow is in doc.go.
var (
	// ErrInvalidPublisherAccount reports a [PublisherAccount] that names no
	// row of cashback.network_account: no id, no network, or no publisher
	// identifier at that network. The account is a NOT NULL column on every
	// evidence row and it owns both durable cursors (migration 0011), so an
	// adapter wired without one produces transactions nothing can attribute
	// to a poll and a cursor that never advances.
	ErrInvalidPublisherAccount = errors.New("networks: publisher account names no account at a network")
)

// PublisherAccount names the account at a network that one adapter polls: a
// row of cashback.network_account, keyed by (network_id,
// external_publisher_id) and carrying that account's two durable cursors
// (migration 0011, ADR-0003).
//
// The port carries it because the poller cannot supply what it is never told.
// Every evidence row requires network_account_id NOT NULL, both cursors live
// on the account rather than on the network, and a deployment may run two
// publisher accounts at one network - which the schema models explicitly. An
// adapter identified only by [Network.ID] would collide with its sibling in
// the poller's registry: two Awin adapters, one map entry, and the second
// account never polled, its cursor null forever, its transactions never
// ingested, and nothing raised. Naming the account is what makes that
// unrepresentable rather than merely unlikely.
//
// The credential is deliberately absent, as it is from the table (ADR-0003):
// an adapter is CONSTRUCTED with whatever it needs to authenticate, from
// configuration, and the port never carries it.
//
// The fields are unexported and the value is built by [NewPublisherAccount],
// so an account cannot be half-built. It has no JSON encoding on purpose: it
// is wiring rather than evidence, and the only part of it that ever reaches a
// row or an event is [PublisherAccount.ID].
type PublisherAccount struct {
	id         uuid.UUID
	network    NetworkID
	externalID string
}

// NewPublisherAccount builds the account an adapter polls, from the
// network_account row the composition root read to construct it. It refuses
// an unusable one wrapping [ErrInvalidPublisherAccount] rather than returning
// a value some later INSERT would refuse: this is Apivo's own configuration,
// not a network's answer, so there is no evidence to preserve in getting it
// wrong.
func NewPublisherAccount(id uuid.UUID, network NetworkID, externalID string) (PublisherAccount, error) {
	account := PublisherAccount{id: id, network: network, externalID: externalID}
	if err := account.Validate(); err != nil {
		return PublisherAccount{}, err
	}
	return account, nil
}

// ID is cashback.network_account.id: the value every evidence row this
// account's poll produces carries in network_account_id.
func (a PublisherAccount) ID() uuid.UUID { return a.id }

// Network is the network the account is held at. [ValidateNetwork] requires
// it to agree with the adapter's own [Network.ID], because an adapter polling
// an account at another network would file one network's transactions under
// another's id.
func (a PublisherAccount) Network() NetworkID { return a.network }

// ExternalID is the network's own identifier for the publisher account -
// external_publisher_id - which is what an operator reading a network's own
// dashboard recognises the account by.
func (a PublisherAccount) ExternalID() string { return a.externalID }

// Validate reports whether the account names a real row, returning an error
// wrapping [ErrInvalidPublisherAccount] when it does not. The zero value does
// not: it is what a bare struct literal carries, and an adapter built from
// one would poll on behalf of nobody.
func (a PublisherAccount) Validate() error {
	if a.id == uuid.Nil {
		return fmt.Errorf("%w: no network_account row is named", ErrInvalidPublisherAccount)
	}
	if err := a.network.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPublisherAccount, err)
	}
	if strings.TrimSpace(a.externalID) == "" {
		return fmt.Errorf("%w: account %s carries no publisher id at %s",
			ErrInvalidPublisherAccount, a.id, strconv.Quote(a.network.String()))
	}
	return nil
}

// String renders the account for logs, errors and test failures, naming the
// network and the publisher id an operator would recognise rather than the
// row id alone.
func (a PublisherAccount) String() string {
	if a.id == uuid.Nil {
		return "(no publisher account)"
	}
	return a.network.String() + " account " + strconv.Quote(a.externalID) + " (" + a.id.String() + ")"
}

// ValidateNetwork holds an adapter to what the port says about itself, and is
// what the composition root calls on each adapter it wires: the network id is
// the lowercase word the network table is keyed by, the publisher account
// names a real row, that account is held at this adapter's own network, and
// the limits describe a network that can be queried.
//
// It exists because every one of those is otherwise discovered at the worst
// possible moment. An adapter returning NetworkID("Awin") forms no deeplinks
// but fetches transactions perfectly well, so the mistake surfaces at the
// foreign key of the first INSERT of a window that had already been fully
// fetched - which is precisely what [ErrInvalidNetworkID] says it exists to
// prevent. An adapter whose account is at another network files one network's
// transactions under another's id. An adapter built with an unset [Limits]
// refuses every window it is given. Wiring is where all three are cheap.
func ValidateNetwork(n Network) error {
	id := n.ID()
	if err := id.Validate(); err != nil {
		return fmt.Errorf("networks: adapter: %w", err)
	}
	account := n.Account()
	if err := account.Validate(); err != nil {
		return fmt.Errorf("networks: adapter %s: %w", strconv.Quote(id.String()), err)
	}
	if account.Network() != id {
		return fmt.Errorf("%w: adapter %s polls an account at %s",
			ErrInvalidPublisherAccount, strconv.Quote(id.String()),
			strconv.Quote(account.Network().String()))
	}
	if err := n.Limits().Validate(); err != nil {
		return fmt.Errorf("networks: adapter %s: %w", strconv.Quote(id.String()), err)
	}
	return nil
}

// Network is the port every affiliate network adapter satisfies, and the
// only way any code in this repository speaks to a network (ADR-0003). It is
// small on purpose: an adapter says which network and which publisher account
// it speaks for, a member can be sent to a retailer, what the network
// reported can be read back, the catalogue can be read back, and the network
// says how it may be queried. Everything the cashback domain does with a
// network is built from those abilities, here, identically over every
// adapter.
//
// One adapter serves one publisher account. The schema models two accounts at
// one network explicitly (network_account is unique on (network_id,
// external_publisher_id), and each row carries its own cursors), so a
// deployment running two Awin accounts wires two adapters, and the poller
// keys them by [PublisherAccount.ID] - never by [Network.ID], which both of
// them share.
//
// Contract, held by all adapters and asserted by the shared conformance
// suite (contracts/ports.md section 2):
//
//  1. Every [Reported] carries the verbatim network response fragment it was
//     normalised from. An adapter returning an empty payload fails its
//     conformance test (FR-032).
//  2. Status mapping is total: an unrecognised status word is
//     [ErrUnmappableStatus], never a silent default, so it reaches an
//     operator rather than a member's balance.
//  3. FetchTransactions respects [Network.Limits]: it never issues a window
//     wider than the network allows - refused by [Limits.ValidateWindow],
//     which is the half this package can check - and paces every request it
//     makes to the declared rate, which is the half its limiter holds.
//  4. Iteration is resumable: a caller that stops mid-iteration and starts
//     again from the same [QueryWindow] asks the same question and misses
//     nothing. Cursors advance only after a window is fully persisted, which
//     is the caller's half of FR-031 and the reason an adapter must not
//     treat "I yielded it" as "it was stored". Re-issuing a window is NOT a
//     promise of an identical answer - see FetchTransactions.
//  5. BuildDeeplink places the click reference in the network's own
//     click-reference parameter (FR-021) and returns an error rather than a
//     partially-formed URL. Every refusal wraps [ErrDeeplinkNotFormed].
//  6. Adapters never write to the database and never decide credits. They
//     translate, and nothing else. Nothing in this package can hold an
//     adapter to that, because it is a rule about what an adapter must not
//     contain; what this package does instead is withhold the means. No
//     signature here speaks a database type, this file imports no driver and
//     no generated store, and a test in this package refuses one - so an
//     adapter that wanted to write a row would have to reach for a
//     dependency the port never handed it. The rule that seals an adapter
//     inside its own package (SC-008) is T109's, and is not written yet.
//  7. Every value an adapter yields has passed its own Validate
//     ([Reported.Validate], [ReportedMerchant.Validate]) before it is
//     yielded. The port has no call site of its own for them - the values
//     are built inside the adapter - so this is an obligation the
//     conformance suite asserts, and the reason a mis-mapped currency or a
//     blank click reference is caught at the adapter that made it rather
//     than at an INSERT halfway through a window.
//  8. Iteration that ends early says so. An adapter that stops for its own
//     reason - a cancelled context, a spent retry budget - yields one final
//     pair carrying [AbandonedIteration] and returns; only a caller's own
//     break ends a sequence silently, and the caller already knows about
//     that. A range loop that ends having yielded no error therefore means
//     the answer was whole, which is what rule 4 lets a cursor advance on.
//  9. Failures against the network are classified, so a caller can tell a
//     blip from a wall: [ErrNetworkUnavailable] is retryable,
//     [ErrNetworkRateLimited] is retryable after waiting, and
//     [ErrNetworkRefused] is terminal until somebody changes a credential.
//     Rule 4 offers no resumption point inside a window, so the only
//     response to a mid-window failure is to run the whole window again -
//     correct for the first two, an infinite loop with a frozen cursor for
//     the third.
type Network interface {
	// ID names the network this adapter speaks to, in the vocabulary the
	// network table is keyed by. It is constant for the life of the
	// adapter: the id is how a stored row is traced back to the code that
	// wrote it, so an adapter whose answer varied would strand its own
	// evidence. It is not an identity - two adapters for two publisher
	// accounts at one network return the same id - and [ValidateNetwork] is
	// where it is held to [NetworkID.Validate], at wiring time.
	ID() NetworkID

	// Account names the publisher account this adapter polls, and is the
	// adapter's identity: it is what the poller keys its registry by, what
	// owns the two durable cursors, and what every evidence row this
	// adapter's poll produces carries in network_account_id.
	//
	// It is constant for the life of the adapter, like ID, and for a
	// stronger reason: the cursors belong to the account, so an adapter that
	// changed accounts would resume one account's window from another's
	// watermark.
	Account() PublisherAccount

	// BuildDeeplink returns the URL the click-out redirect sends the member
	// to, with the click reference placed in the network's own
	// click-reference parameter (contract rule 5, FR-021). The parameter's
	// name comes from the target's route rather than from a literal in the
	// adapter, because it is per-network configuration and a wrong value
	// silently loses attribution on every click.
	//
	// What comes back is an absolute http or https URL, ready to be written
	// into a Location header. [ValidateDeeplinkInputs] is what every adapter
	// checks first, and it has already refused a template that is anything
	// else, so an adapter that appends a parameter to it returns something
	// absolute too.
	//
	// It returns an error rather than a partially-formed URL, and every
	// refusal wraps [ErrDeeplinkNotFormed]. That is the load-bearing half of
	// the contract: a half-built URL still redirects, so the member reaches
	// the retailer, buys, and is never credited - a failure that costs a
	// member real money while looking to them exactly like success.
	//
	// A refusal says which kind it is beside that. Everything
	// ValidateDeeplinkInputs refuses also wraps [ErrDeeplinkInputsRefused]
	// and is deterministic - our own routing bug, or a route somebody has to
	// fix - while an adapter that had to reach for the network to sign a URL
	// and could not reports [ErrNetworkUnavailable] or [ErrNetworkRefused].
	// The member is not redirected either way; the difference decides
	// whether the alert points at a network or at us, and whether the offer
	// is one to stop publishing.
	BuildDeeplink(ctx context.Context, target DeeplinkTarget, ref IssuedClickRef) (string, error)

	// FetchTransactions yields every transaction the network reported inside
	// window, each already normalised, each carrying its verbatim payload
	// (contract rule 1) and each already through [Reported.Validate]
	// (contract rule 7). The window is refused before any I/O if it is wider
	// than [Network.Limits] allows, wrapping [ErrWindowTooWide] - never
	// clamped, because a caller that silently received less than it asked
	// for would advance its cursor past transactions it never saw (contract
	// rule 3, FR-031).
	//
	// The result is the standard library's iterator shape rather than a
	// slice, for the same reason the ledger's History is: a window may hold
	// more transactions than fit comfortably in memory, its natural reader
	// is a single pass that persists as it goes, and a caller that stops
	// early - because a write failed, or the process is shutting down -
	// must be able to stop without the adapter fetching the rest.
	//
	// The immediate error covers only what is checkable WITHOUT contacting
	// the network: an unusable or too-wide window. Everything that comes
	// from contacting it is yielded as the pair's error instead, whether it
	// happens on the first page or the fiftieth, and classified per contract
	// rule 9 - so an eager adapter and a lazy one report an expired
	// credential through the same channel, which is what stops a caller
	// needing to know which kind it was given.
	//
	// An adapter that stops before the end of the window yields
	// [AbandonedIteration] once and returns (contract rule 8). This is the
	// one thing an adapter must not get wrong: without it, a poller
	// interrupted at report 400 of 900 sees a loop that ended cleanly,
	// records the window as read, and never asks for the other 500 again.
	//
	// Iteration is resumable (contract rule 4): a caller that stopped
	// halfway may run that window from the beginning. It must, in fact - the
	// port offers no resumption point inside a window, because a cursor that
	// could sit mid-window would advance over transactions that were yielded
	// but not yet stored. Re-reading a window is cheap; losing one is not.
	//
	// Re-issuing a window asks the same question; it does not promise the
	// same answer. What a re-read returns is the network's account of that
	// period AS IT STANDS NOW, and it must be, because that is the entire
	// mechanism by which a pending transaction is ever seen to become
	// confirmed: validation takes up to 90 days, so the poller re-reads a
	// trailing window on a slower schedule rather than waiting (ADR-0003).
	// An adapter that memoised a window's pages would satisfy resumability
	// to the letter and freeze every member's money at pending, with no
	// error anywhere.
	FetchTransactions(ctx context.Context, window QueryWindow) (iter.Seq2[Reported, error], error)

	// FetchCatalogue yields every retailer the publisher account can promote
	// on this network, each carrying its verbatim payload (FR-012) and each
	// already through [ReportedMerchant.Validate]. It takes no window: a
	// catalogue is a current state rather than a period, and the whole of it
	// is re-read on each import so that a retailer who has disappeared from
	// the answer is visible as such.
	//
	// Because absence is read as departure, completeness is the whole
	// contract here, and contract rule 8 is what carries it: an import may
	// reconcile a retailer it did not see to left_network only after
	// iteration ended with no error at all. An adapter whose deadline fired
	// at retailer 400 of 5000 and returned quietly would otherwise have the
	// import mark 4600 live routes departed, every offer on them stop being
	// published, and members see an emptied catalogue - from an import that
	// reported nothing wrong.
	//
	// The same iterator reasoning applies as for FetchTransactions, and so
	// do the same two error channels: the immediate error is for what is
	// checkable without contacting the network, and a catalogue read has no
	// such precondition, so an adapter reports every failure - including one
	// on the very first page - through the sequence. The same rate limit
	// applies too: a catalogue is paginated, and contract rule 3's rate
	// governs every request an adapter makes.
	FetchCatalogue(ctx context.Context) (iter.Seq2[ReportedMerchant, error], error)

	// Limits states what the network documents about how it may be queried
	// (contract rule 3). It is a declaration rather than an enforcement
	// point: the adapter holds itself to it, refusing a window wider than
	// MaxWindow and pacing its requests to RequestsPerSecond, and a caller
	// reads it to split a backfill into windows the network will answer.
	//
	// The values come from the network's row rather than from literals in
	// the adapter, and they are read once, when the composition root builds
	// the adapter. So a network that revises its documented limit is a
	// configuration change and a restart rather than a release - and the
	// signature says as much by taking no context and returning no error: an
	// adapter whose row could not be read is never built, because
	// [ValidateNetwork] refuses it at wiring, so there is no state in which
	// this method has a failure to report.
	Limits() Limits
}
