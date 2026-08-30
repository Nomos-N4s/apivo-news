// Package blnk implements the [wallet.Ledger] port over Blnk, the
// open-source double-entry ledger this repository carries members' money on
// (ADR-0002). It is the production driver: the in-memory ledger is the
// reference every implementation is measured against, the Postgres ledger is
// the documented exit route, and this one is what a member's balance is
// actually made of.
//
// Nothing here escapes this directory. The vendor SDK is imported by this
// package and by no other - the architecture test in internal/arch fails the
// build otherwise - so every signature below speaks the port's own types and
// the substrate stays swappable inside the constitution's five-day budget.
// [New] takes an endpoint and options rather than a client, for the same
// reason: the composition root wires this ledger from configuration strings
// and never learns the vendor's name.
//
// # How the port's model lands on Blnk's
//
// Blnk has ledgers, balances and transactions. A balance is an account; a
// transaction moves one currency's worth of value from one source to one
// destination, from one to several, or from several to one. The mapping is
// therefore:
//
//   - a [wallet.AccountRef] in a currency is one Blnk balance, addressed by
//     a deterministic indicator derived from the pair AND from the Blnk
//     ledger this value is configured for (see [Ledger.EnsureAccount]);
//   - a [wallet.LedgerAccountID] is that indicator, "@" and all, rather
//     than Blnk's own balance id. Blnk creates a balance the first time a
//     transaction names one and only that way, so there is no id to hand
//     out until money has moved;
//   - a [wallet.Transfer] is one Blnk transaction, whose reference is the
//     transfer's idempotency key - which is what makes a replay a duplicate
//     reference rather than a second movement of money;
//   - a [wallet.Posting] is one movement the caller wrote, read back from
//     the document this package records on the transaction rather than
//     from what the ledger stored, which is netted per account.
//
// # Accounts are named, never created
//
// Blnk's create-a-balance endpoint takes a ledger, an identity, a currency
// and a precision, and no indicator at all: a balance created through it is
// nameless, and the endpoint that finds a balance by name would never find
// it again. The only writer of an indicator is the transaction path, which
// resolves a source or destination beginning with "@" to the balance
// carrying that name and creates it - in Blnk's own General Ledger - when
// there is none.
//
// So EnsureAccount writes nothing. It validates the reference, resolves the
// Blnk ledger whose id namespaces the indicator, derives the indicator and
// returns it; the balance behind it appears when the first transfer names
// it. Two things follow, and both are load-bearing:
//
//   - the balance sits in Blnk's General Ledger, never in the ledger this
//     value is configured for. That ledger is a namespace and nothing else,
//     so nothing here may judge a balance by which ledger it belongs to.
//   - an indicator is unique across the whole server: the lookup filters on
//     the indicator and the currency, and on nothing else. The ledger id
//     inside the name is therefore the only thing keeping two deployments
//     over one Blnk server apart, which is why it is part of every name.
//
// # How a split transfer is recorded and read back
//
// A transfer touching more than one account on a side becomes one Blnk
// transaction carrying legs, and Blnk splits it into one child transaction
// per leg: each child names one leg account at one end and the whole
// transfer's other end at the other. ONLY the children are recorded. There
// is no parent row - the parent exists in the request and in the response
// and is never persisted - so the transfer's identity is the id its
// children point at, which is the id the create answered with.
//
// A split also rewrites the reference: each child is stored under the
// transfer's reference with an ordinal appended, and no row carries the
// original. That is why an idempotency key is looked up by probing for the
// first child as well as for the key itself, and why the postings of one
// transfer are gathered by grouping rows on the id they point at rather
// than by reading a row that does not exist.
//
// Every child carries a copy of the whole transfer's annotation, so the
// document that says which postings the caller wrote is read from ONE child
// of a group. Reading it once per child would multiply a rounded earning -
// one funding account, a member bucket and a rounding remainder (D6) - by
// the number of legs it had.
//
// # Money never touches a float
//
// The SDK's transaction carries both a float amount and an
// arbitrary-precision integer one, and Blnk refuses a request that sets
// both. This package sets the integer and never the float: the float keeps
// its zero value on every request this package sends, and no code path here
// reads it back. Split legs carry a precise distribution, which is a
// decimal integer string, rather than the SDK's distribution, which is a
// percentage or a decimal number. Every integer crossing the boundary is checked for exactness -
// a big.Int that will not fit an int64 is refused wrapping
// [money.ErrOverflow] rather than truncated - because a silent narrowing
// here is a wrong balance, not a wrong log line (C-6).
//
// # What this ledger cannot do
//
// A Blnk transaction is denominated in a single currency, so a transfer
// spanning currencies cannot be recorded in one atomic act. It is refused
// wrapping [wallet.ErrUnsupportedTransfer] rather than split into several
// transactions, because splitting would promise an atomicity this package
// would not be delivering. Callers split such a movement themselves, one
// transfer per currency, each with its own key.
//
// A transfer with several accounts giving AND several receiving is refused
// the same way. Blnk splits exactly one side of a transaction: it takes the
// sources when there are sources and the destinations otherwise, and hands
// every child the other side's scalar - which a transfer split on both
// sides does not have. Such a request is not recorded wrongly, it is not
// recorded at all, and refusing it here says so in the port's own
// vocabulary rather than as whatever the ledger answers when it cannot
// resolve an empty account. One side may have as many accounts as it likes.
//
// Seven smaller divergences from the in-memory reference are real and are
// stated here rather than left to be discovered:
//
//   - PostedAt is the ledger's instant, not a clock this process can set.
//     [WithClock] moves the settle deadline and nothing else, so a test
//     that needs postings at chosen instants draws its windows from what
//     History returns instead of from instants it invented.
//   - The postings of one SPLIT transfer can carry instants a shade apart
//     when they are read from different accounts. The ledger writes a
//     split's children one at a time, each stamped as it lands, and a
//     History read sees only the children naming the account being read;
//     the transfer's instant is the earliest of those, which is the whole
//     truth for the account on the split's single side and one child's
//     truth for an account on the other. Every account's own history stays
//     ordered, stable across re-reads and partitioned by adjacent windows,
//     which is what a watermark resume needs - a watermark is per account -
//     and closing the gap would cost a second query per split on every read
//     to fetch rows that account is no part of.
//   - History's tie-break is the ledger's transaction id. It is stable
//     across repeated reads, which is what a watermark resume needs, but
//     the ids are not ordered, so it is not the order the ledger recorded
//     them in. Postings sharing an instant are the ordinary case within one
//     transfer, and those all carry one reference anyway.
//   - [wallet.ErrUnknownAccount] is judged by reading the account id, not
//     by asking the ledger. An id EnsureAccount issued is an indicator this
//     package's derivation could have written, in this deployment's ledger
//     namespace; anything else was never issued here, whether or not some
//     balance somewhere answers to it. The other two implementations ask
//     their storage, because their storage is where accounts are created.
//     Here nothing is created, so a lookup would only answer whether money
//     has moved yet - which is a different question, and answering it as
//     "no such account" would refuse the first transfer into a bucket.
//   - An account whose postings cancel inside a transfer has no History
//     entry for it here, where the other two keep one per posting. Such an
//     account is no leg of the transaction - it moves nothing - so nothing
//     on the ledger names it and no filter can find it. The port permits
//     the shape and every balance still agrees; recording a leg that moved
//     nothing to make the read symmetrical would be putting a fiction on
//     the ledger to tidy up a report.
//   - A transaction written on one of these balances by something other
//     than this package carries none of its annotations, so there is
//     nothing to read but what the ledger recorded: it comes back as the
//     one netted posting the rows describe. The other two implementations
//     have no such transactions to answer for.
//   - A transfer's Reference travels as the transaction's description,
//     which Blnk requires to be non-empty. The port does not, so a blank
//     Reference is sent as the idempotency key instead: the description is
//     a courtesy pointer for whoever reads the ledger directly, and the key
//     is the truest pointer this package has when the caller named nothing.
//     What a replay compares is unaffected - that is the annotation, not
//     the description.
//
// One promise the port makes cannot be kept over a network and is stated
// here as well: a failure with no answer from the ledger - a transport
// error, a client timeout - leaves the key's fate unknown, because the
// transfer may have been recorded by the call whose answer was lost. See
// [Ledger.Post] on the only safe recovery.
//
// # What only a live ledger can settle
//
// Docker is unavailable on the founder's machine, so the suite in
// integration_test.go skips there and first runs for real in the cashback
// CI job. Five readings of the substrate are encoded here and cannot be
// confirmed anywhere else, and each is written to fail loudly rather than
// guess:
//
//   - that naming an account in a transaction creates a balance under that
//     name, with the "@" kept, so the same name finds it afterwards. If it
//     did not, a member's balance would read as zero while the ledger's own
//     figure was right, so the live suite reads a balance back immediately
//     after the transfer that created it.
//   - which ordinal a split appends to the reference. The queued path and
//     the synchronous path in the server spell it differently, and this
//     package sends only synchronous transactions - so it probes the
//     spelling that path writes first and the other second, and the live
//     suite posts a split and replays its key to prove the probe finds it.
//   - that a filtered page arrives under the key this package reads. A page
//     under any other key is an error, never an empty history.
//   - that a transaction the ledger declines to apply frees the reference
//     it holds. This package treats a refused key as unclaimed, which is
//     what the port requires; where the ledger keeps it, the next post
//     under that key fails saying exactly that, and never as a conflict
//     that would invite a caller to mint a new key for money it may owe.
//   - which structured codes the ledger refuses with. They are constants
//     here, and only the codes are branched on where a response carries
//     one - never the raw body, which is whatever answered.
//
// The SDK's calls carry no context: it builds its requests without one, so
// a call already in flight cannot be cancelled. Every method here checks
// the caller's context before it starts and between the steps of a
// multi-call operation, and the per-request timeout ([WithTimeout]) is what
// bounds a single call. A cancelled context therefore ends the operation
// promptly but not instantly.
package blnk

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	blnkgo "github.com/blnkfinance/blnk-go"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The port is satisfied by *Ledger or not at all; this line is where the
// compiler says so.
var _ wallet.Ledger = (*Ledger)(nil)

// Defaults for the knobs [New] exposes. Each is a value a deployment may
// need to move, and none of them is a money decision: the amounts, the
// accounts and the refusals are the same whatever these are set to.
const (
	// DefaultLedgerName is the name of the Blnk ledger this package
	// namespaces its account names under when no id or name is configured.
	// The balances themselves are created by Blnk, in its own General
	// Ledger, so this names a namespace rather than a container - and it is
	// the whole of what keeps this deployment's accounts apart from
	// anything else pointed at the same server, because an indicator is
	// unique across the server rather than within a ledger.
	DefaultLedgerName = "cashback"
	// DefaultPrecision is the minor units per major unit Blnk is told each
	// transaction uses. It affects only how the ledger renders its own
	// decimal view of an amount: this package sends and reads integer
	// minor units, so the stored figure is exact whatever this is.
	DefaultPrecision = 100
	// DefaultTimeout bounds one call to the ledger. The SDK applies it per
	// request; it is set explicitly rather than inherited so a version bump
	// cannot quietly change how long a money call may hang.
	DefaultTimeout = 10 * time.Second
	// DefaultSettleTimeout bounds how long Post waits for a transfer it
	// created to become readable. See [Ledger.Post] on why it waits at all.
	DefaultSettleTimeout = 15 * time.Second
	// DefaultSettleInterval is how often Post re-asks while it waits.
	DefaultSettleInterval = 50 * time.Millisecond
)

// historyPageSize is how many transactions one History page asks for. It is
// a page rather than an unbounded read because a history is unbounded, and
// it is as large as the ledger will honour because the alternative to one
// round trip per few postings is a member's wallet view costing dozens.
//
// A hundred is that maximum: the server clamps a larger page down to its
// own default of twenty rather than refusing it, so asking for more than
// this would quietly cost five times the round trips.
const historyPageSize = 100

// historyMaxPages caps the paging loop. A server that ignored the offset
// would otherwise return page one forever, and a wallet view that never
// returns is worse than one that says why.
const historyMaxPages = 1000

// metaKeyTransfer is the single meta_data key this package writes on a
// transaction. Under it sits the JSON document that decides replay
// identity: the port's Reference, its Metadata and the postings as a
// canonical multiset.
//
// It is one key rather than several because the document is compared as a
// whole, and a half-written annotation would be a transfer whose identity
// could not be judged. The name is deliberately generic: it says which
// port wrote it, not which product deployed it.
const metaKeyTransfer = "wallet_transfer"

// Indicator vocabulary. A Blnk balance's indicator is this package's name
// for the account, derived from the resolved Blnk ledger and the (ref,
// currency) pair EnsureAccount was handed - it is how the same pair finds
// the same balance on every call, across processes and restarts, with no
// mapping table anywhere, and it is the account id this package hands out.
//
// The parts are joined with a full stop and every part is restricted to
// characters that need no escaping, because the indicator travels in a URL
// path when the balance is looked up. A separator that could appear inside
// a part would let two different accounts spell one indicator, which is a
// member's money merged with somebody else's.
//
// The first part is the Blnk ledger the name belongs to, and it is what
// makes [WithLedgerID] and [WithLedgerName] isolate anything at all: an
// indicator is unique across the whole server rather than within a ledger,
// so two deployments pointed at one Blnk server would otherwise resolve one
// another's house accounts - the configured names are the same strings in
// every environment - and post each other's money. The resolved ledger id
// is used rather than the configured name, because a deployment may name
// its ledger or pin its id and both spellings must derive one account.
const (
	// indicatorPrefix is what makes Blnk treat a source or destination as a
	// name to resolve rather than as a balance id, and it is stored as part
	// of the name: the server passes the raw source through to the balance
	// it creates, so the lookup must ask for the "@" too. It is part of the
	// account id this package hands out for that reason - an id that had to
	// be decorated before use would be an id whose spelling every caller
	// had learned.
	indicatorPrefix    = "@"
	indicatorSeparator = "."
	// memberIndicator marks a member's claim bucket:
	// <ledger>.member.<uuid>.<stage>.<CUR>. Every part is fixed-format, so
	// the spelling is injective and no member identity beyond the opaque
	// account uuid reaches the ledger.
	memberIndicator = "member"
	// houseIndicator marks an operational account:
	// <ledger>.house.<hex of the configured name>.<CUR>. The name is
	// hex-encoded rather than written through, because it comes from
	// configuration and may hold any byte at all - a separator, a slash, a
	// space - and an encoding that is injective and path-safe is what keeps
	// two configured names two accounts.
	houseIndicator = "house"
)

// Blnk's own vocabulary, named here so a comparison against it reads as a
// decision rather than as a string literal in an if.
const (
	// fieldSource and fieldDestination are the transaction columns
	// History filters on. A posting on an account is a transaction that
	// named it as one end or the other.
	fieldSource      = "source"
	fieldDestination = "destination"
	// sortByCreatedAt orders a filtered page by the instant Blnk recorded
	// the transaction. History re-sorts what it receives anyway - the
	// contract's ordering is this port's, not the substrate's - but asking
	// for the order we want keeps paging stable.
	sortByCreatedAt = "created_at"
	sortAscending   = "asc"
)

// Ledger is the Blnk implementation of [wallet.Ledger]. Build one with
// [New]; the zero value carries no client and is not usable.
//
// It holds no money of its own. Accounts, transfers and postings live in
// Blnk, so any number of Ledger values over one Blnk server are one ledger,
// across processes and restarts alike. What it does hold is the resolved id
// of the Blnk ledger that namespaces its account names - an immutable fact
// about a server that neither renames nor deletes one, memoised so the
// common path costs no round trip at all.
type Ledger struct {
	client *blnkgo.Client

	// ledgerName and ledgerIDOpt are how the Blnk ledger is chosen: by id
	// when configuration knows one, by name otherwise. Exactly one of them
	// is used, and which is settled in New.
	ledgerName  string
	ledgerIDOpt string

	// secretKey and timeout configure the client [New] builds. They are
	// held on the struct because the options that set them run before the
	// client exists.
	secretKey string
	timeout   time.Duration

	precision      int64
	settleTimeout  time.Duration
	settleInterval time.Duration

	// now is read when Post starts waiting for a transfer to become
	// readable, and nowhere else. PostedAt comes from Blnk, which stamps
	// its own transactions - see [WithClock].
	now func() time.Time

	// mu guards the resolved ledger below.
	mu sync.Mutex
	// ledgerID is the resolved Blnk ledger, empty until first resolved.
	ledgerID string
}

// Option configures the ledger [New] returns.
type Option func(*Ledger)

// WithSecretKey supplies the credential the Blnk sidecar authenticates
// calls with. An empty key is sent as an empty header value, which is what
// an unauthenticated sidecar on a private network expects; configuration
// decides whether that is acceptable, not this package.
func WithSecretKey(key string) Option {
	return func(l *Ledger) { l.secretKey = key }
}

// WithLedgerID pins the Blnk ledger account names are namespaced under,
// skipping the lookup by name. Use it where the ledger is provisioned
// outside this process and its id is configuration. The ledger it names is
// part of every account name this value derives, so two deployments over
// one Blnk server keep their accounts apart by choosing different ledgers
// and by nothing else.
func WithLedgerID(id string) Option {
	return func(l *Ledger) { l.ledgerIDOpt = strings.TrimSpace(id) }
}

// WithLedgerName names the Blnk ledger account names are namespaced under,
// to be resolved on first use and created if it does not exist. An empty
// name keeps [DefaultLedgerName]: a nameless ledger would be found by
// nothing. The resolved ledger is part of every account name this value
// derives, so two deployments sharing one Blnk server keep their accounts
// apart by naming different ledgers.
func WithLedgerName(name string) Option {
	return func(l *Ledger) {
		if strings.TrimSpace(name) != "" {
			l.ledgerName = strings.TrimSpace(name)
		}
	}
}

// WithPrecision sets the minor units per major unit Blnk is told each
// transaction uses. It changes the ledger's own decimal rendering and
// nothing else - amounts cross this boundary as integers - so a wrong value
// is a cosmetic problem in the ledger's UI, not a wrong balance. A
// non-positive value keeps [DefaultPrecision].
func WithPrecision(precision int64) Option {
	return func(l *Ledger) {
		if precision > 0 {
			l.precision = precision
		}
	}
}

// WithTimeout bounds one call to the ledger. A non-positive value keeps
// [DefaultTimeout].
func WithTimeout(timeout time.Duration) Option {
	return func(l *Ledger) {
		if timeout > 0 {
			l.timeout = timeout
		}
	}
}

// WithSettleWindow bounds how long Post waits for a transfer it created to
// become readable, and how often it re-asks while it waits. A non-positive
// bound keeps [DefaultSettleTimeout] or [DefaultSettleInterval]. See
// [Ledger.Post] on what the wait is for.
func WithSettleWindow(timeout, interval time.Duration) Option {
	return func(l *Ledger) {
		if timeout > 0 {
			l.settleTimeout = timeout
		}
		if interval > 0 {
			l.settleInterval = interval
		}
	}
}

// WithClock replaces the source the settle deadline is measured from, so a
// test can make the wait in [Ledger.Post] elapse without sleeping.
//
// It does NOT change PostedAt, and no option could: Blnk stamps its own
// transactions and this package reports the instant the ledger recorded.
// That is the honest divergence from the in-memory and Postgres ledgers,
// both of which take their instants from an injected clock - a test that
// needs postings at instants of its own choosing cannot get them here, and
// draws its windows from what History returns instead. A nil clock keeps
// the default, [time.Now].
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// New returns a ledger speaking to the Blnk server at baseURL, which must be
// an absolute http or https URL. It performs no I/O: the Blnk ledger the
// balances sit in is resolved on first use, so a miswired endpoint surfaces
// on the first account rather than at construction, exactly as the Postgres
// ledger's schema does.
func New(baseURL string, opts ...Option) (*Ledger, error) {
	l := &Ledger{
		ledgerName:     DefaultLedgerName,
		precision:      DefaultPrecision,
		timeout:        DefaultTimeout,
		settleTimeout:  DefaultSettleTimeout,
		settleInterval: DefaultSettleInterval,
		now:            time.Now,
	}
	for _, opt := range opts {
		opt(l)
	}

	// Parsed and judged here rather than left to the SDK, which panics on
	// a base URL it does not like. A configuration mistake is an error a
	// composition root can report, not a crash.
	//
	// The endpoint is never repeated in the error. It travels beside a
	// secret key in configuration, and a value pasted into the wrong key
	// would otherwise print itself into a startup log somebody keeps.
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("blnk: the ledger endpoint must be an absolute http or https URL (the value is not repeated here: it may carry a credential)")
	}

	// The retry count is set explicitly at one - no retries. The SDK never
	// retries a POST, so this governs reads only, and a read that failed is
	// better re-asked by the loop that wanted it than by a layer that
	// cannot see whether the caller's context is still alive.
	key := l.secretKey
	l.client = blnkgo.NewClient(parsed, &key,
		blnkgo.WithTimeout(l.timeout),
		blnkgo.WithRetry(1),
		blnkgo.WithLogger(silentLogger{}),
	)
	return l, nil
}

// silentLogger swallows the SDK's own chatter. The SDK logs every transport
// error through the standard logger by default, which would put a money
// endpoint's URL and its failures into a stream this repository's logging
// package does not own or redact. Failures reach the caller as errors,
// which is where a decision can be made about them.
type silentLogger struct{}

func (silentLogger) Info(string)  {}
func (silentLogger) Error(string) {}

// EnsureAccount resolves ref in currency to the name Blnk knows the account
// by. It creates nothing, and it is the one method here that could not
// create anything if it wanted to.
//
// Identity is an indicator derived deterministically from the Blnk ledger
// this value is configured for and from (ref, currency) -
// @<ledger>.member.<uuid>.<stage>.<CUR> or @<ledger>.house.<hex of the
// name>.<CUR> - and that string IS the returned account id. The same pair
// spells the same id in every process and after every restart, with no
// mapping table on either side, which is what makes the call idempotent and
// safe under any number of racing callers: there is nothing to race for.
//
// The balance behind the name appears when the first transfer names it.
// Blnk creates a balance only along that path - its create-a-balance
// endpoint takes no indicator, so a balance made through it could never be
// found by name again - and it creates it in its own General Ledger. An
// account nothing has posted to yet therefore has no balance row at all,
// which is not an error: it holds nothing, and Balance and History say so.
//
// The ledger is part of the name because an indicator is unique across the
// whole server rather than within a ledger: two deployments pointed at one
// Blnk server would otherwise resolve one another's accounts, and house
// names come from configuration, so "commission" is the same string in
// staging and in production. Member accounts are no safer - a database
// restored from production carries the same member ids. Naming the ledger
// is what makes [WithLedgerName] and [WithLedgerID] isolate anything.
//
// The derivation is injective. Member ids are fixed-format uuids and stages
// come from a closed set; house names are hex-encoded, so a configured name
// holding a separator, a slash or a space cannot collide with another name
// - two configured names are two accounts, which is what stops a
// configuration typo quietly sharing a balance with the account it was
// mistyped from. No member identity beyond the opaque account uuid reaches
// the ledger.
//
// An unusable ref is refused wrapping [wallet.ErrInvalidAccountRef] and a
// malformed currency wrapping [money.ErrInvalidCurrency], both before any
// I/O. The only I/O this method can do is resolving the Blnk ledger the
// name is scoped to, once per process.
func (l *Ledger) EnsureAccount(ctx context.Context, ref wallet.AccountRef, currency money.Currency) (wallet.LedgerAccountID, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	// ParseCurrency rather than a local check, so a malformed code is
	// refused with the same error text everywhere money refuses one.
	if _, err := money.ParseCurrency(string(currency)); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("blnk: %w", err)
	}

	// The ledger is resolved before the name is derived: the name carries
	// it, so it cannot be spelled without one.
	ledgerID, err := l.resolveLedgerID(ctx)
	if err != nil {
		return "", err
	}
	return wallet.LedgerAccountID(indicatorFor(ledgerID, ref, currency)), nil
}

// balanceByIndicator looks one balance up by the name Blnk created it
// under, reporting a nil balance and no error when no transfer has named it
// yet. An account with no balance row is an account holding nothing, which
// is an ordinary answer here and not an unknown account: EnsureAccount
// issued the name, and the row appears when money first moves.
//
// What came back is checked against what was asked for. The check is not
// ceremony: the lookup is the whole of this package's account identity, and
// a server answering with some other balance would be money read off the
// wrong account. Which Blnk ledger the balance sits in is NOT checked -
// every balance this package touches is auto-created in the General Ledger,
// and the configured ledger lives inside the name instead.
func (l *Ledger) balanceByIndicator(indicator string, currency money.Currency) (*blnkgo.LedgerBalance, error) {
	balance, resp, err := l.client.LedgerBalance.GetByIndicator(indicator, string(currency))
	closeResponse(resp)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if balance == nil || balance.BalanceID == "" {
		return nil, fmt.Errorf("blnk: the ledger answered for account %q with no balance id", indicator)
	}
	if balance.Indicator != indicator {
		return nil, fmt.Errorf("blnk: looking up account %q answered with an account named %q", indicator, balance.Indicator)
	}
	if balance.Currency != string(currency) {
		return nil, fmt.Errorf("blnk: account %q holds %s, not %s", indicator, balance.Currency, currency)
	}
	return balance, nil
}

// resolveLedgerID answers with the Blnk ledger this package's account names
// are scoped to, resolving it once per process. A configured id is taken as
// given; a name is looked up and created if absent, and the list is re-read
// after a create so two processes that raced converge on one ledger rather
// than each keeping its own.
func (l *Ledger) resolveLedgerID(ctx context.Context) (string, error) {
	id, err := l.knownLedgerID(ctx)
	if err != nil || id != "" {
		return id, err
	}

	if err := l.createLedger(ctx); err != nil {
		return "", err
	}
	if id, err = l.findLedger(); err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("blnk: the ledger %q was created but cannot be listed", l.ledgerName)
	}
	l.rememberLedgerID(id)
	return id, nil
}

// knownLedgerID answers with the Blnk ledger this package's account names
// are scoped to when there already is one, and with the empty string when
// the configured name names no ledger yet. It creates nothing, which is
// what makes it usable from the read paths: a Balance or a History over an
// id nobody issued must not bring a ledger into existence as a side effect.
func (l *Ledger) knownLedgerID(ctx context.Context) (string, error) {
	l.mu.Lock()
	cached := l.ledgerID
	l.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if l.ledgerIDOpt != "" {
		l.rememberLedgerID(l.ledgerIDOpt)
		return l.ledgerIDOpt, nil
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("blnk: %w", err)
	}
	id, err := l.findLedger()
	if err != nil || id == "" {
		return "", err
	}
	l.rememberLedgerID(id)
	return id, nil
}

// rememberLedgerID memoises the resolved ledger. A Blnk ledger is neither
// renamed nor deleted, so the entry can only save a round trip.
func (l *Ledger) rememberLedgerID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ledgerID = id
}

// findLedger returns the id of the ledger carrying this package's name, or
// empty when there is none. Where two carry it - two processes that raced a
// create - the oldest wins, with the id breaking a tie, so every process
// picks the same one.
func (l *Ledger) findLedger() (string, error) {
	ledgers, resp, err := l.client.Ledger.List()
	closeResponse(resp)
	if err != nil {
		return "", fmt.Errorf("blnk: listing ledgers: %w", err)
	}
	winner := ""
	var oldest time.Time
	for _, candidate := range ledgers {
		if candidate.Name != l.ledgerName || candidate.LedgerID == "" {
			continue
		}
		switch {
		case winner == "":
		case candidate.CreatedAt.Before(oldest):
		case candidate.CreatedAt.Equal(oldest) && candidate.LedgerID < winner:
		default:
			continue
		}
		winner, oldest = candidate.LedgerID, candidate.CreatedAt
	}
	return winner, nil
}

// createLedger creates the named ledger. A failure is reported rather than
// retried: findLedger runs again afterwards, and a create that lost a race
// to another process is indistinguishable from one that worked.
func (l *Ledger) createLedger(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("blnk: %w", err)
	}
	_, resp, err := l.client.Ledger.Create(blnkgo.CreateLedgerRequest{Name: l.ledgerName})
	closeResponse(resp)
	if err != nil {
		return fmt.Errorf("blnk: creating the ledger %q: %w", l.ledgerName, err)
	}
	return nil
}

// indicatorFor derives this package's name for the account (ref, currency)
// inside the Blnk ledger ledgerID. See [Ledger.EnsureAccount] for why the
// derivation is injective, why the house name is encoded rather than
// written through, and why the ledger is part of the name at all.
func indicatorFor(ledgerID string, ref wallet.AccountRef, currency money.Currency) string {
	scope := indicatorPrefix + hex.EncodeToString([]byte(ledgerID))
	if memberID, stage, ok := ref.Member(); ok {
		return strings.Join([]string{scope, memberIndicator, memberID.String(), stage.String(), string(currency)}, indicatorSeparator)
	}
	// Validate passed in EnsureAccount, so a ref that is not a member
	// reference is a house reference.
	name, _ := ref.House()
	return strings.Join([]string{scope, houseIndicator, hex.EncodeToString([]byte(name)), string(currency)}, indicatorSeparator)
}

// accountKind separates the two shapes an account can have, which is the
// only thing Post's solvency rule needs to know about one: a member's stage
// account may not be left holding less than nothing, and a house account
// may.
type accountKind uint8

const (
	// kindUnknown is what an indicator this package did not write parses
	// to. It is not a third kind of account; it means the balance was
	// never issued by EnsureAccount.
	kindUnknown accountKind = iota
	kindMember
	kindHouse
)

// stageNames is the closed set of stage spellings an indicator may carry.
// Parsing against the set rather than accepting any word is what keeps the
// derivation injective in both directions: a balance whose name looks like
// a member account but names no stage this port has was not written here.
var stageNames = map[string]bool{
	wallet.StageHeld.String():      true,
	wallet.StagePending.String():   true,
	wallet.StageConfirmed.String(): true,
	wallet.StageReserved.String():  true,
}

// parseIndicator reads back what indicatorFor wrote for the ledger
// ledgerID: which shape the account has and which currency it was ensured
// in. It reports ok false for anything this package did not write, and for
// anything it wrote for another ledger.
//
// This is the whole of how an id EnsureAccount never issued is told from
// one it did, and it is a divergence from the other two implementations,
// which ask their storage. Nothing is created here, so the ledger could
// only answer whether money has moved through the account yet - a different
// question, and answering it as "no such account" would refuse the first
// transfer into every bucket. What the port promises is that ids it issued
// work and ids it did not are refused, and the derivation is injective, so
// reading the name answers exactly that.
func parseIndicator(ledgerID, indicator string) (kind accountKind, currency money.Currency, ok bool) {
	parts := strings.Split(indicator, indicatorSeparator)
	if len(parts) < 2 || parts[0] != indicatorPrefix+hex.EncodeToString([]byte(ledgerID)) {
		return kindUnknown, "", false
	}
	parts = parts[1:]
	switch {
	case len(parts) == 4 && parts[0] == memberIndicator:
		// The uuid is not re-parsed into a member id - nothing here needs
		// one - but its shape is checked, because a part that is not a
		// uuid means this name was not written by indicatorFor.
		if len(parts[1]) != len("00000000-0000-0000-0000-000000000000") || !stageNames[parts[2]] {
			return kindUnknown, "", false
		}
		parsed, err := money.ParseCurrency(parts[3])
		if err != nil {
			return kindUnknown, "", false
		}
		return kindMember, parsed, true
	case len(parts) == 3 && parts[0] == houseIndicator:
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return kindUnknown, "", false
		}
		parsed, err := money.ParseCurrency(parts[2])
		if err != nil {
			return kindUnknown, "", false
		}
		return kindHouse, parsed, true
	default:
		return kindUnknown, "", false
	}
}

// account is what Post, Balance and History need to know about one ledger
// account: the currency it holds, whether the solvency rule governs it,
// what it holds right now, and the balance id Blnk gave it if a transfer
// has named it yet.
//
// balanceID is empty for an account no money has reached. That is an
// ordinary state, not a missing one, and the two readers treat it as such:
// there is nothing to hold and nothing to have a history of.
type account struct {
	id        wallet.LedgerAccountID
	kind      accountKind
	currency  money.Currency
	held      money.Amount
	balanceID string
}

// readAccount turns an account id into this package's view of the account:
// what its name says it is, and what the ledger holds under that name.
//
// The name decides whether the id was ever issued here. An id that is not
// an indicator this package's derivation could have written, in this
// deployment's ledger namespace, is refused wrapping
// [wallet.ErrUnknownAccount] - see [parseIndicator] on why that judgement
// is made by reading rather than by asking. A name this deployment did
// write that the ledger has no balance for is an account holding nothing:
// the balance appears when the first transfer names it, so its absence
// means no money has moved yet and never that the account is unknown.
func (l *Ledger) readAccount(ctx context.Context, id wallet.LedgerAccountID) (account, error) {
	if err := ctx.Err(); err != nil {
		return account{}, fmt.Errorf("blnk: %w", err)
	}
	// Resolved without creating: a read must not bring a ledger into
	// existence, and where there is no ledger yet there is no account this
	// port ever issued.
	ledgerID, err := l.knownLedgerID(ctx)
	if err != nil {
		return account{}, err
	}
	if ledgerID == "" {
		return account{}, fmt.Errorf("%w: %q, and this port has issued no account at all yet", wallet.ErrUnknownAccount, id)
	}
	kind, currency, ok := parseIndicator(ledgerID, string(id))
	if !ok {
		return account{}, fmt.Errorf("%w: %q is not a name this ledger issues", wallet.ErrUnknownAccount, id)
	}

	empty, err := money.Zero(currency)
	if err != nil {
		return account{}, fmt.Errorf("blnk: account %q: %w", id, err)
	}
	acct := account{id: id, kind: kind, currency: currency, held: empty}

	balance, err := l.balanceByIndicator(string(id), currency)
	if err != nil {
		return account{}, fmt.Errorf("blnk: reading account %q: %w", id, err)
	}
	if balance == nil {
		return acct, nil
	}

	// The one place a ledger figure becomes an Amount. A balance too large
	// for an int64 is refused rather than truncated: a balance that reads
	// as its own negation is worse than an error (C-6).
	minor, err := exactInt64(balance.Balance)
	if err != nil {
		return account{}, fmt.Errorf("blnk: the balance of account %q: %w", id, err)
	}
	acct.held = money.Amount{Minor: minor, Currency: currency}
	acct.balanceID = balance.BalanceID
	return acct, nil
}

// Balance returns what Blnk says the account holds as of the call: the
// ledger's own figure, which is the sum of the postings it recorded on that
// balance. Nothing is stored on this side, so there is no second truth that
// could drift from it (D7).
//
// The currency argument is the caller's assertion of what the account holds
// (C-6): an account of a different currency is refused wrapping
// [money.ErrCurrencyMismatch], an id EnsureAccount never issued wrapping
// [wallet.ErrUnknownAccount]. An account with no postings is zero in its
// currency - including one Blnk has no balance row for at all, because a
// row appears only when a transfer names the account and its absence means
// no money has moved rather than that the account is unknown. A figure too
// large for an int64 is refused wrapping [money.ErrOverflow] rather than
// wrapped around.
func (l *Ledger) Balance(ctx context.Context, id wallet.LedgerAccountID, currency money.Currency) (money.Amount, error) {
	acct, err := l.readAccount(ctx, id)
	if err != nil {
		return money.Amount{}, err
	}
	if acct.currency != currency {
		return money.Amount{}, fmt.Errorf("blnk: account %q: %w: holds %s, asked for %s",
			id, money.ErrCurrencyMismatch, acct.currency, currency)
	}
	return acct.held, nil
}

// movement is one account's net movement in a transfer. Post nets the
// postings per account before building the request, because Blnk's
// transaction has one leg per account per side: an account given twice
// would be two legs on one balance, and an account that gives and receives
// in one transfer would be a leg on both sides of it.
//
// Netting is a decision about the request only. What makes two transfers
// the same is still the multiset of postings the caller wrote, recorded
// verbatim on the transaction and compared on replay.
type movement struct {
	account wallet.LedgerAccountID
	net     money.Amount
}

// decomposition is one transfer's postings arranged the way a Blnk
// transaction wants them: one currency, the accounts that give, the
// accounts that receive, and the total that flows between them.
type decomposition struct {
	currency     money.Currency
	sources      []movement
	destinations []movement
	total        int64
}

// decompose turns a validated transfer into the shape a Blnk transaction
// takes, or refuses it.
//
// A transfer spanning currencies is refused wrapping
// [wallet.ErrUnsupportedTransfer]: a Blnk transaction carries a single
// currency, so recording one would take several transactions, and several
// transactions are not one atomic act. Refusing is the contract; splitting
// would be promising an atomicity this package could not deliver.
//
// A transfer with several accounts giving AND several receiving is refused
// the same way, and for the same reason: Blnk splits one side of a
// transaction into a child per leg, taking the sources when there are
// sources and the destinations otherwise, and gives every child the other
// side's single account. A transaction split on both sides has no such
// account to give them, so the ledger cannot record it - not wrongly, at
// all. Refusing here names the shape in the port's own vocabulary rather
// than passing the ledger a request it will fail to resolve.
//
// Within one currency the postings are netted per account, in the order the
// transfer introduced them, and an account whose postings cancel is dropped
// - it moves nothing, so it is not a leg. What remains is at least one
// giver and at least one receiver: the postings sum to zero and at least
// one account moves ([wallet.Transfer.Validate] refuses anything else), so
// the nets cannot all be zero and cannot all share a sign.
func decompose(postings []wallet.Posting) (decomposition, error) {
	var currency money.Currency
	for _, p := range postings {
		switch {
		case currency == "":
			currency = p.Amount.Currency
		case p.Amount.Currency != currency:
			return decomposition{}, fmt.Errorf(
				"%w: it spans %s and %s, and a transaction on this ledger carries one currency; post one transfer per currency, each with its own key",
				wallet.ErrUnsupportedTransfer, currency, p.Amount.Currency)
		}
	}

	nets := make(map[wallet.LedgerAccountID]money.Amount, len(postings))
	order := make([]wallet.LedgerAccountID, 0, len(postings))
	for _, p := range postings {
		net, seen := nets[p.Account]
		if !seen {
			nets[p.Account] = p.Amount
			order = append(order, p.Account)
			continue
		}
		// Add can only fail on overflow: every posting under one key
		// shares that key's currency, checked above.
		sum, err := net.Add(p.Amount)
		if err != nil {
			return decomposition{}, fmt.Errorf("blnk: netting the postings on account %q: %w", p.Account, err)
		}
		nets[p.Account] = sum
	}

	out := decomposition{currency: currency}
	total, err := money.Zero(currency)
	if err != nil {
		return decomposition{}, err
	}
	for _, id := range order {
		net := nets[id]
		switch {
		case net.IsZero():
			// Cancels on the account it names: it is not a leg.
		case net.IsNegative():
			out.sources = append(out.sources, movement{account: id, net: net})
		default:
			out.destinations = append(out.destinations, movement{account: id, net: net})
			if total, err = total.Add(net); err != nil {
				return decomposition{}, fmt.Errorf("blnk: totalling the transfer: %w", err)
			}
		}
	}
	if len(out.sources) == 0 || len(out.destinations) == 0 {
		// Unreachable behind Validate, which refuses a transfer that
		// balances by moving nothing - and reported rather than swallowed
		// in case that ever stops being true.
		return decomposition{}, fmt.Errorf("%w: it has %d account(s) giving and %d receiving",
			wallet.ErrNoMovement, len(out.sources), len(out.destinations))
	}
	if len(out.sources) > 1 && len(out.destinations) > 1 {
		return decomposition{}, fmt.Errorf(
			"%w: %d accounts give and %d receive, and this ledger splits one side of a transaction into a leg each while handing every leg the other side's single account; post one transfer per side, each with its own key",
			wallet.ErrUnsupportedTransfer, len(out.sources), len(out.destinations))
	}
	out.total = total.Minor
	return out, nil
}

// legsOf renders one side's movements as Blnk transaction legs. The amount
// travels as precise_distribution, a decimal integer string, and never as
// the SDK's distribution, which is a percentage or a decimal number: money
// crosses this boundary as an integer or not at all (C-6).
func legsOf(movements []movement) ([]blnkgo.Source, error) {
	legs := make([]blnkgo.Source, len(movements))
	for i, m := range movements {
		magnitude, err := m.net.Abs()
		if err != nil {
			return nil, fmt.Errorf("blnk: the movement on account %q: %w", m.account, err)
		}
		legs[i] = blnkgo.Source{
			Identifier:          string(m.account),
			PreciseDistribution: strconv.FormatInt(magnitude.Minor, 10),
		}
	}
	return legs, nil
}

// Post records transfer atomically: every posting or none, as one Blnk
// transaction. [wallet.Transfer.Validate] runs first, before any I/O, so a
// transfer that would create or destroy money is refused before it can
// leave the process (C-1, checked twice by design).
//
// The order of the judgements is the contract's, not convenience:
//
//  1. What this port can record at all. A transfer spanning currencies is
//     refused wrapping [wallet.ErrUnsupportedTransfer] with no I/O, because
//     no key could ever have recorded one - a Blnk transaction carries a
//     single currency, and several transactions are not one atomic act. A
//     transfer with several accounts giving and several receiving is
//     refused the same way and as early, because this ledger splits one
//     side of a transaction and has nothing to give the legs of a transfer
//     split on both. A transfer in which a house account that must go
//     below nothing gives beside a member's stage account is refused the
//     same way too, once the accounts have been read: the overdraft this
//     ledger grants covers a whole transaction, so recording that shape
//     would mean giving up the member's protection. See below.
//  2. The idempotency key, before anything the ledger knows about
//     accounts. A key already recorded answers about what it recorded: the
//     same transfer returns the original reference and records nothing, a
//     different transfer under that key is refused wrapping
//     [wallet.ErrIdempotencyConflict]. "The same transfer" is the multiset
//     of (account, amount) movements the caller wrote, plus Reference and
//     Metadata by content, with nil and empty metadata both annotating
//     nothing - compared against the document this package recorded on the
//     transaction, not against the legs Blnk stored, so netting the legs
//     for the wire cannot change what a replay means.
//  3. The accounts. Every posting must name a balance EnsureAccount issued
//     ([wallet.ErrUnknownAccount]) in that balance's own currency
//     ([money.ErrCurrencyMismatch]), judged over the whole transfer before
//     anything is sent.
//  4. The money. A transfer that would leave a member's stage account
//     holding less than nothing is refused wrapping
//     [wallet.ErrInsufficientFunds], judged on the transfer's net movement
//     per account. House accounts are exempt: they are the boundary of the
//     closed set of accounts, and a ledger in which nothing may go negative
//     has nothing able to fund its first credit.
//
// The solvency rule is enforced twice, and the second is the one that
// matters under a race. Blnk refuses a source it would leave negative
// unless the transaction asks for an overdraft, and that permission is per
// transaction rather than per leg. So no transfer in which a member's stage
// account gives ever asks for it: the ledger itself refuses such a transfer
// atomically, against the balance it finds at the moment it applies it,
// with no window between reading a balance and spending it. That is the
// refusal the withdrawal reservation D9 leans on and it is exact.
//
// A house account gives without the permission too, then, whenever a member
// gives beside it - which costs nothing while the house account holds what
// it is giving. Where it does not, the two rules cannot both be kept in one
// Blnk transaction, and the transfer is refused wrapping
// [wallet.ErrUnsupportedTransfer] rather than recorded with the member's
// protection quietly switched off. A transfer drawing only on house
// accounts asks for the permission as before: that is how money enters the
// closed set of accounts.
//
// A refused Post changes no balance and leaves the key exactly as it found
// it, so a caller may correct the transfer and post it again under the same
// key. That holds for every refusal above and for a transfer the ledger
// itself refused, including one it accepted and then rejected. It cannot
// hold for a failure with no answer at all - a transport error, a client
// timeout - where the ledger may have recorded the transfer this call
// cannot see. The only safe recovery from one of those is to post the SAME
// transfer under the SAME key, which replays; a
// [wallet.ErrIdempotencyConflict] after such a failure means the original
// was recorded and must be read back rather than re-keyed, because minting
// a fresh key for the correction would record both.
//
// The reference returned is Blnk's transaction id for the whole transfer,
// and it is readable the moment Post returns. For a transfer the ledger
// split it is the id every child points at, which is the id the create
// answered with and the id History reports for each of them, so all a
// transfer's postings carry one reference whatever shape it had - which is
// what the audit join from a domain row to the money needs (D7).
//
// Blnk queues transactions by default, which would make a wallet recomputed
// straight after a transition show the total from before it, so every
// transaction this package creates asks to skip the queue and Post does not
// return until the ledger reports it applied. If it has not applied inside
// the settle window the call fails rather than claiming a visibility it
// does not have - and because the key is already recorded, the caller's
// retry is a replay, not a second movement.
func (l *Ledger) Post(ctx context.Context, transfer wallet.Transfer) (wallet.TransferRef, error) {
	if err := transfer.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("blnk: %w", err)
	}
	shape, err := decompose(transfer.Postings)
	if err != nil {
		return "", err
	}
	recorded := recordOf(transfer)

	// The key is judged first, so a Post under a recorded key answers about
	// what that key recorded - even where posting the same transfer fresh
	// would now be refused for want of funds. A key whose transaction the
	// ledger refused recorded nothing, so it is posted afresh.
	replay, state, err := l.judgeRecordedKey(transfer, recorded)
	switch {
	case err != nil:
		return "", err
	case state == keyClaimed:
		// A replay still waits: the post that first recorded this key may
		// be a concurrent one that has not applied yet, and both callers
		// are promised a reference they can immediately read behind.
		if err := l.awaitApplied(ctx, transfer.IdempotencyKey, replay); err != nil {
			return "", l.explainRefusal(ctx, transfer, err)
		}
		return wallet.TransferRef(replay.id), nil
	}

	accounts, err := l.checkAccounts(ctx, transfer.Postings)
	if err != nil {
		return "", err
	}
	// Judged before the money is: a transfer this ledger cannot record
	// without giving up the port's solvency rule is refused for its shape,
	// whatever its amounts happen to be.
	overdraft, err := overdraftFor(accounts, shape.sources)
	if err != nil {
		return "", err
	}
	touched, err := memberMovements(accounts, transfer.Postings)
	if err != nil {
		return "", err
	}
	if err := checkFunds(accounts, touched); err != nil {
		return "", err
	}

	request, err := l.buildRequest(transfer, shape, recorded, overdraft)
	if err != nil {
		return "", err
	}

	created, resp, err := l.client.Transaction.Create(request)
	closeResponse(resp)
	if err != nil {
		// A create that lost a race to another post of the same key is a
		// replay, not a failure: the winner's reference comes back and
		// nothing was recorded twice.
		winner, explained := l.explainCreateFailure(transfer, recorded, err)
		if explained != nil {
			return "", explained
		}
		if err := l.awaitApplied(ctx, transfer.IdempotencyKey, winner); err != nil {
			return "", l.explainRefusal(ctx, transfer, err)
		}
		return wallet.TransferRef(winner.id), nil
	}
	if created == nil || created.TransactionID == "" {
		return "", fmt.Errorf("blnk: the transfer under key %q was accepted but no transaction id came back", transfer.IdempotencyKey)
	}
	accepted := recordedTransfer{id: created.TransactionID, status: created.Status, metadata: created.MetaData}
	if err := l.awaitApplied(ctx, transfer.IdempotencyKey, &accepted); err != nil {
		return "", l.explainRefusal(ctx, transfer, err)
	}
	return wallet.TransferRef(accepted.id), nil
}

// explainRefusal turns a transaction the ledger declined to apply into the
// port's own answer. Everything else passes through unchanged.
//
// A refusal that arrives this way carries no reason: the transaction is
// simply not applied. The one reason the port has a word for is a source
// with too little in it, and the way to tell is to look - so the accounts
// are read again and the transfer judged against what they hold now. A
// refusal for want of money is exactly the race the reservation D9 leans on
// losing, and it must reach the caller as [wallet.ErrInsufficientFunds]
// rather than as an outage, because the two ask for opposite handling.
//
// Anything else is reported as itself. Guessing at a reason for a money
// movement that did not happen would be worse than saying so.
func (l *Ledger) explainRefusal(ctx context.Context, transfer wallet.Transfer, err error) error {
	var declined refusedTransfer
	if !errors.As(err, &declined) {
		return err
	}
	accounts, readErr := l.checkAccounts(ctx, transfer.Postings)
	if readErr != nil {
		return fmt.Errorf("%w, and its accounts could not be read to say why: %w", declined, readErr)
	}
	touched, netErr := memberMovements(accounts, transfer.Postings)
	if netErr != nil {
		return fmt.Errorf("%w: %w", declined, netErr)
	}
	if shortErr := checkFunds(accounts, touched); shortErr != nil {
		return fmt.Errorf("the ledger refused transfer %q as %s: %w", declined.key, declined.status, shortErr)
	}
	return fmt.Errorf("%w; every account it names holds enough, so the reason is not one this port has a word for - nothing moved and the key is free, so the transfer may be corrected and posted again", declined)
}

// buildRequest renders the transfer as the transaction Blnk will record.
//
// A side with one account travels as the scalar source or destination and a
// side with several as legs, because the ledger refuses a request carrying
// both spellings of one side. Exactly one side can carry legs - decompose
// refuses the rest - and that side's transaction is marked atomic, which is
// what makes its legs succeed or fail together rather than one at a time.
//
// The description is the transfer's Reference, or the idempotency key when
// the caller named nothing: the ledger requires a description and the port
// does not require a Reference, so a blank one would make every transfer
// with no domain pointer unpostable. The key is the truest pointer there is
// in that case, and what a replay compares is the annotation rather than
// this.
//
// allowOverdraft is asked for only when a house account gives: see
// [Ledger.Post] on why that permission cannot simply always be granted.
func (l *Ledger) buildRequest(transfer wallet.Transfer, shape decomposition, recorded record, allowOverdraft bool) (blnkgo.CreateTransactionRequest, error) {
	annotation, err := json.Marshal(recorded)
	if err != nil {
		// Unreachable for a document of strings and integers - and
		// returned rather than swallowed in case the type ever changes.
		return blnkgo.CreateTransactionRequest{}, fmt.Errorf("blnk: recording the transfer's identity: %w", err)
	}

	request := blnkgo.CreateTransactionRequest{
		ParentTransaction: blnkgo.ParentTransaction{
			// Blnk's reference IS the idempotency key: a second
			// transaction under it is refused by the ledger, which is what
			// makes a replayed dispatch safe (C-5). A split rewrites it
			// into one reference per child, and the rewriting is
			// deterministic, so the guard survives the shape - see
			// [splitOrdinals].
			Reference: transfer.IdempotencyKey,
			Currency:  string(shape.currency),
			Precision: l.precision,
			// The integer, and only the integer. Amount keeps its zero
			// value: Blnk refuses a request carrying both, and a float in
			// a money position is a rounding error waiting for an audit.
			PreciseAmount: big.NewInt(shape.total),
			Description:   cmp.Or(strings.TrimSpace(transfer.Reference), transfer.IdempotencyKey),
			// Inline rather than queued. The port requires what Post
			// recorded to be readable the moment it returns, and a queued
			// transaction is readable when a worker gets to it.
			SkipQueue: true,
			MetaData:  map[string]any{metaKeyTransfer: string(annotation)},
		},
		AllowOverdraft: allowOverdraft,
	}

	if len(shape.sources) == 1 {
		request.Source = string(shape.sources[0].account)
	} else if request.Sources, err = legsOf(shape.sources); err != nil {
		return blnkgo.CreateTransactionRequest{}, err
	}
	if len(shape.destinations) == 1 {
		request.Destination = string(shape.destinations[0].account)
	} else if request.Destinations, err = legsOf(shape.destinations); err != nil {
		return blnkgo.CreateTransactionRequest{}, err
	}
	request.Atomic = len(shape.sources) > 1 || len(shape.destinations) > 1
	return request, nil
}

// explainCreateFailure turns Blnk's refusal into the port's own answer: the
// transaction another post already recorded when the refusal was really a
// replay of its key, an error otherwise.
//
// A conflict is judged rather than assumed: the reference is the
// idempotency key, so a duplicate means the key is already recorded, and
// what it recorded decides between a replay and a collision. Everything
// else is matched on the structured error code the ledger returns, falling
// back to the message - older responses carry only a flat string, and the
// SDK maps those to the code UNKNOWN, so a package that read the code alone
// would stop recognising an overdraft the day the server was downgraded.
func (l *Ledger) explainCreateFailure(transfer wallet.Transfer, recorded record, err error) (*recordedTransfer, error) {
	switch {
	case isConflict(err):
		winner, state, judged := l.judgeRecordedKey(transfer, recorded)
		switch {
		case judged != nil:
			return nil, judged
		case state == keyRefused:
			// The ledger holds the reference for a transaction it refused
			// to apply. Nothing moved, so the port would have the key free
			// for a corrected retry, and this substrate will not free it.
			// Reported rather than papered over: a caller that minted a
			// fresh key here would be posting money the ledger may yet
			// decide it has seen.
			return nil, fmt.Errorf(
				"blnk: idempotency key %q is held by transaction %q, which the ledger refused to apply and will not accept again; nothing moved, but this key cannot be re-used and the transfer needs a decision outside this process: %w",
				transfer.IdempotencyKey, winner.id, err)
		case state == keyUnclaimed:
			return nil, fmt.Errorf("blnk: the ledger refused idempotency key %q as already used, but no transfer is recorded under it: %w", transfer.IdempotencyKey, err)
		default:
			return winner, nil
		}
	case isNotFound(err):
		// Judged before the refusal below, so that a not-found answer is
		// read by its status rather than by whatever words its message
		// happens to carry.
		return nil, fmt.Errorf("%w: the ledger does not hold one of the accounts this transfer names: %w", wallet.ErrUnknownAccount, err)
	case isInsufficientFunds(err):
		return nil, fmt.Errorf("%w: the ledger refused to leave a source account short: %w", wallet.ErrInsufficientFunds, err)
	default:
		return nil, fmt.Errorf("blnk: recording the transfer under key %q: %w", transfer.IdempotencyKey, err)
	}
}

// checkAccounts reads every account the transfer names, once each, and
// refuses the whole transfer when any of them is unknown or holds another
// currency. It runs before anything is sent, so a refused Post changes no
// balance and consumes no key.
func (l *Ledger) checkAccounts(ctx context.Context, postings []wallet.Posting) (map[wallet.LedgerAccountID]account, error) {
	accounts := make(map[wallet.LedgerAccountID]account, len(postings))
	for i, p := range postings {
		if _, seen := accounts[p.Account]; !seen {
			acct, err := l.readAccount(ctx, p.Account)
			if err != nil {
				if errors.Is(err, wallet.ErrUnknownAccount) {
					return nil, fmt.Errorf("%w: posting %d of %d names %q", wallet.ErrUnknownAccount, i+1, len(postings), p.Account)
				}
				return nil, err
			}
			accounts[p.Account] = acct
		}
		if held := accounts[p.Account].currency; p.Amount.Currency != held {
			return nil, fmt.Errorf("blnk: posting %d of %d: %w: account %q holds %s, the posting moves %s",
				i+1, len(postings), money.ErrCurrencyMismatch, p.Account, held, p.Amount.Currency)
		}
	}
	return accounts, nil
}

// memberMovements nets the transfer per member stage account, in the order
// the postings introduce them, and leaves house accounts out: they are
// exempt from the solvency rule, so they are not judged by it.
//
// Every member account the transfer touches is included, not only the ones
// it draws on, because the rule is about the balance a transfer leaves
// behind: an account already holding less than nothing is not made lawful
// by a credit too small to clear it. That is what the in-memory reference
// judges, and the three implementations answer one question the same way
// or the conformance suite is measuring nothing.
func memberMovements(accounts map[wallet.LedgerAccountID]account, postings []wallet.Posting) ([]movement, error) {
	nets := make(map[wallet.LedgerAccountID]money.Amount, len(postings))
	order := make([]wallet.LedgerAccountID, 0, len(postings))
	for _, p := range postings {
		if accounts[p.Account].kind != kindMember {
			continue
		}
		net, seen := nets[p.Account]
		if !seen {
			nets[p.Account] = p.Amount
			order = append(order, p.Account)
			continue
		}
		// Add can only fail on overflow: checkAccounts has established
		// that every posting shares its account's currency.
		sum, err := net.Add(p.Amount)
		if err != nil {
			return nil, fmt.Errorf("blnk: netting the postings on account %q: %w", p.Account, err)
		}
		nets[p.Account] = sum
	}
	out := make([]movement, 0, len(order))
	for _, id := range order {
		out = append(out, movement{account: id, net: nets[id]})
	}
	return out, nil
}

// checkFunds refuses the transfer whole, wrapping
// [wallet.ErrInsufficientFunds], when applying it would leave any member
// stage account holding less than nothing. The judgement is on the net
// movement per account, so a transfer that takes 5000 from a bucket holding
// 1000 and hands 4500 straight back never leaves the account short of
// anything.
//
// It is the first of two refusals and the weaker one: the balances it reads
// were read before the transfer was sent. The refusal that closes the race
// is the ledger's own, which is why [Ledger.Post] never asks for a
// transaction-wide overdraft while a member account is giving.
func checkFunds(accounts map[wallet.LedgerAccountID]account, touched []movement) error {
	for _, m := range touched {
		held := accounts[m.account].held
		after, err := held.Add(m.net)
		if err != nil {
			return fmt.Errorf("blnk: applying %s to account %q: %w", m.net, m.account, err)
		}
		if after.IsNegative() {
			return fmt.Errorf("%w: account %q holds %s and this transfer would leave it at %s",
				wallet.ErrInsufficientFunds, m.account, held, after)
		}
	}
	return nil
}

// overdraftFor decides whether the transaction may ask Blnk for permission
// to leave a source negative - and refuses the one shape where the honest
// answer is neither yes nor no.
//
// The permission is per transaction, not per leg (it is one field on the
// create request), so granting it for a house source grants it for every
// member source in the same transaction as well. That would switch off
// precisely the atomic refusal the port's rule 6 rests on and leave a
// balance this package read moments earlier as the only guard - a
// read-then-write with a window wide enough to drive a member's stage
// account negative under two racing posts. So:
//
//   - no member account gives: the permission is asked for, because a house
//     account is where money enters the closed set and must be able to go
//     below nothing;
//   - a member account gives and no house source needs to go negative: the
//     permission is NOT asked for, and Blnk judges every source against the
//     balance it actually finds. The house sources are judged too, which
//     costs nothing when they hold what they are giving;
//   - a member account gives and a house source would be left negative:
//     refused wrapping [wallet.ErrUnsupportedTransfer]. Blnk cannot exempt
//     one source and not another, so this transfer cannot be recorded in
//     one atomic act without giving up the member's protection, and
//     refusing is the contract. The caller splits it: the house movement is
//     a transfer of its own, with a key of its own.
//
// The last case is judged on the balances read a moment ago, so it can also
// fire for a transfer the ledger would have accepted. That costs a refusal
// on a transfer nothing has moved for, which the port permits and the
// caller may retry; letting it through would cost the rule.
func overdraftFor(accounts map[wallet.LedgerAccountID]account, sources []movement) (bool, error) {
	memberGives := false
	for _, m := range sources {
		if accounts[m.account].kind == kindMember {
			memberGives = true
			break
		}
	}
	if !memberGives {
		return true, nil
	}
	for _, m := range sources {
		if accounts[m.account].kind == kindMember {
			continue
		}
		held := accounts[m.account].held
		after, err := held.Add(m.net)
		if err != nil {
			return false, fmt.Errorf("blnk: applying %s to account %q: %w", m.net, m.account, err)
		}
		if after.IsNegative() {
			return false, fmt.Errorf(
				"%w: the house account %q holds %s and would be left at %s while a member's stage account gives in the same transfer; this ledger grants an overdraft to a whole transaction or to none of it, so recording this would switch off the refusal that keeps the member's account above nothing - post the house movement as a transfer of its own",
				wallet.ErrUnsupportedTransfer, m.account, held, after)
		}
	}
	return false, nil
}

// refusedTransfer reports a transaction the ledger accepted the request for
// and then declined to apply. It is a distinct type rather than a message
// because Post has to answer for it in the port's own vocabulary, and only
// Post can tell a refusal for want of money from any other.
type refusedTransfer struct {
	key    string
	status blnkgo.PryTransactionStatus
}

func (e refusedTransfer) Error() string {
	return fmt.Sprintf("blnk: the ledger did not apply transfer %q: it is %s", e.key, e.status)
}

// settling reports whether a status is one a transaction passes through on
// its way to being applied. Anything outside this set and the applied and
// refused sets is a status this package does not know, and waiting out the
// settle window on one only turns an unknown answer into a slow one.
func settling(status blnkgo.PryTransactionStatus) bool {
	switch status {
	case "", blnkgo.PryTransactionStatusQueued, blnkgo.PryTransactionStatusInFlight, blnkgo.PryTransactionStatusCommit:
		return true
	default:
		return false
	}
}

// refused reports whether a status means the ledger declined to apply the
// transaction. All three are terminal: nothing moved and nothing will.
func refused(status blnkgo.PryTransactionStatus) bool {
	switch status {
	case blnkgo.PryTransactionStatusRejected, blnkgo.PryTransactionStatusVoid, blnkgo.PryTransactionStatusExpired:
		return true
	default:
		return false
	}
}

// awaitApplied does not return until the ledger reports the transaction
// under key applied, which is what makes the port's read-your-writes
// promise true over a substrate that queues by default.
//
// The transfer the create returned is passed in when there is one, so the
// ordinary case - skip_queue accepted, applied inline - costs no extra
// round trip. Anything else is re-read until it applies or the settle
// window closes. A transaction the ledger declined to apply is reported as
// a [refusedTransfer] for Post to answer for, and a status this package
// does not know is reported immediately rather than polled for: the money
// question behind an unrecognised answer does not get clearer by asking the
// same question again for fifteen seconds.
func (l *Ledger) awaitApplied(ctx context.Context, key string, created *recordedTransfer) error {
	status := blnkgo.PryTransactionStatus("")
	if created != nil {
		status = created.status
	}
	if status == blnkgo.PryTransactionStatusApplied {
		return nil
	}

	deadline := l.now().Add(l.settleTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("blnk: waiting for transfer %q to be readable: %w", key, err)
		}
		switch {
		case status == blnkgo.PryTransactionStatusApplied:
			return nil
		case refused(status):
			return refusedTransfer{key: key, status: status}
		case !settling(status):
			return fmt.Errorf(
				"blnk: the ledger reports transfer %q as %q, which is not a status this package knows how to read; it is reported rather than guessed at, because the transfer may or may not have moved money",
				key, status)
		}
		if !l.now().Before(deadline) {
			return fmt.Errorf(
				"blnk: transfer %q is %q after %s and is not readable yet; the port requires a transfer to be readable when Post returns, so this is reported rather than answered - the transfer may still be applied afterwards, so the only safe recovery is to post the same key again, which is a replay and not a second movement",
				key, statusOrUnknown(status), l.settleTimeout)
		}
		if err := sleep(ctx, l.settleInterval); err != nil {
			return fmt.Errorf("blnk: waiting for transfer %q to be readable: %w", key, err)
		}
		found, err := l.transferUnder(key)
		if err != nil {
			return fmt.Errorf("blnk: waiting for transfer %q to be readable: %w", key, err)
		}
		if found != nil {
			status = found.status
		}
	}
}

// statusOrUnknown renders a status for an error message, naming the empty
// one rather than printing nothing.
func statusOrUnknown(status blnkgo.PryTransactionStatus) string {
	if status == "" {
		return "unreported"
	}
	return string(status)
}

// sleep waits for d or until the context ends, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// record is what one idempotency key remembers, stored as JSON on the
// transaction Blnk recorded: the port's Reference, its Metadata, and the
// postings as the caller wrote them, canonicalised so two spellings of one
// movement compare equal.
//
// It is stored rather than derived from the transaction's own legs because
// the legs are netted per account for the wire, and netting is not
// identity: two postings of 100 on one account and one posting of 200 on it
// record different postings, and the port says so.
type record struct {
	Reference string            `json:"reference"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Postings  []recordedPosting `json:"postings"`
}

// recordedPosting is one movement, stripped to what it means. Provenance is
// required blank on input by the port's Validate, so there is nothing else
// to keep.
type recordedPosting struct {
	Account  string `json:"account"`
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// recordOf renders a transfer's identity. The postings are kept in the
// order the caller wrote them, because this document is also what History
// reads a posting back from and the port orders the postings of one
// transfer by the order they were recorded. Order is still not identity -
// see [record.matches] - so a retry that rebuilt the same movements in
// another order is still the same transfer. Empty metadata is dropped, so a
// nil map and an empty map record identically: both annotate nothing.
func recordOf(transfer wallet.Transfer) record {
	postings := make([]recordedPosting, len(transfer.Postings))
	for i, p := range transfer.Postings {
		postings[i] = recordedPosting{
			Account:  string(p.Account),
			Minor:    p.Amount.Minor,
			Currency: string(p.Amount.Currency),
		}
	}

	out := record{Reference: transfer.Reference, Postings: postings}
	if len(transfer.Metadata) > 0 {
		out.Metadata = maps.Clone(transfer.Metadata)
	}
	return out
}

// sortedPostings returns the postings in one fixed order, so two documents
// can be compared as the multisets the port says they are. It sorts a copy:
// the order in the document is the order the caller wrote, and History
// answers in it.
func (r record) sortedPostings() []recordedPosting {
	postings := slices.Clone(r.Postings)
	slices.SortFunc(postings, func(a, b recordedPosting) int {
		if c := cmp.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Currency, b.Currency); c != 0 {
			return c
		}
		return cmp.Compare(a.Minor, b.Minor)
	})
	return postings
}

// matches reports whether two transfers are the same transfer, as
// [Ledger.Post] defines it: the postings as a multiset, the reference byte
// for byte, and the metadata by content.
func (r record) matches(other record) bool {
	if r.Reference != other.Reference {
		return false
	}
	// maps.Equal compares content, so nil and empty metadata - both of
	// which annotate nothing - count as the same annotations.
	if !maps.Equal(r.Metadata, other.Metadata) {
		return false
	}
	return slices.Equal(r.sortedPostings(), other.sortedPostings())
}

// keyState is what an idempotency key means to the ledger right now.
type keyState uint8

const (
	// keyUnclaimed is a key nothing has recorded a transfer under.
	keyUnclaimed keyState = iota
	// keyClaimed is a key a transfer was recorded under, and the transfer
	// is the one being posted.
	keyClaimed
	// keyRefused is a key whose transaction the ledger declined to apply.
	// It moved no balance, so it claims nothing: the port says a refused
	// Post leaves the key exactly as it found it, and a caller whose
	// transfer was refused may correct it and post it again.
	keyRefused
)

// judgeRecordedKey answers for an idempotency key that may already be in
// the ledger: the recorded transfer for a true replay, a conflict for the
// same key carrying a different transfer, and keyUnclaimed when the key is
// not recorded at all. The transfer is handed back rather than only its id,
// so the caller can see whether it has applied without asking again.
//
// A transaction the ledger refused is judged before its contents are, and
// answers keyRefused whatever it was carrying. Nothing was recorded under
// the key, so there is no transfer for a second post to collide with -
// reporting a conflict there would strand a caller that corrected a refused
// transfer, and reporting the refusal again would strand one that simply
// retried it. Keys are derived from the domain fact that caused the
// transfer (D8), so neither caller can escape by minting a new one.
func (l *Ledger) judgeRecordedKey(transfer wallet.Transfer, incoming record) (*recordedTransfer, keyState, error) {
	found, err := l.transferUnder(transfer.IdempotencyKey)
	if err != nil {
		return nil, keyUnclaimed, fmt.Errorf("blnk: reading idempotency key %q: %w", transfer.IdempotencyKey, err)
	}
	if found == nil {
		return nil, keyUnclaimed, nil
	}
	if refused(found.status) {
		return found, keyRefused, nil
	}

	stored, err := recordedOn(found.metadata)
	if err != nil {
		return nil, keyClaimed, fmt.Errorf("blnk: the transfer recorded under key %q: %w", transfer.IdempotencyKey, err)
	}
	if !stored.matches(incoming) {
		return nil, keyClaimed, fmt.Errorf("%w: key %q", wallet.ErrIdempotencyConflict, transfer.IdempotencyKey)
	}
	return found, keyClaimed, nil
}

// recordedTransfer is one transfer as the ledger holds it, whatever number
// of rows it kept for it: the id every caller of this key must be told, the
// status that says whether it moved money, and the annotation that decides
// what a replay of the key means.
//
// It exists because a split transfer has no row of its own. Its id is the
// id its children point at, its status and its annotation are read from one
// of them, and nothing above this type needs to know which of the two
// shapes it came from.
type recordedTransfer struct {
	id       string
	status   blnkgo.PryTransactionStatus
	metadata map[string]any
}

// splitOrdinals are the suffixes the ledger appends to a transfer's
// reference when it splits it, in the order transferUnder probes them.
//
// A split rewrites the reference of every child and records no row under
// the original, so a key that recorded a split cannot be found by asking
// for it. The children are numbered from one, which makes the first child's
// reference deterministic - but the ledger spells the join differently
// depending on which path recorded the transaction, and the two paths are
// not distinguishable from here with certainty. The synchronous path, which
// is the only one this package asks for, writes the hyphen; the queued path
// rewrites it to the underscore. Both are probed, cheapest first, because a
// key whose transfer this package failed to find would be posted a second
// time - which is the one mistake idempotency exists to prevent.
var splitOrdinals = []string{"-1", "_1"}

// transferUnder answers with the transfer recorded under an idempotency
// key, or nil when nothing is recorded under it.
//
// The key itself is asked for first, which is the whole answer for a
// transfer the ledger did not split. A split records no row under the key,
// so the first child is probed for instead, and the transfer's id is the
// parent the child names.
//
// A row found under a probed reference that names no parent is somebody
// else's transfer whose key happens to spell this one's first child, and it
// is skipped rather than read: it belongs to a different key, and answering
// with it would replay a movement this caller never made.
func (l *Ledger) transferUnder(key string) (*recordedTransfer, error) {
	found, err := l.transactionByReference(key)
	if err != nil {
		return nil, err
	}
	if found != nil {
		return &recordedTransfer{id: found.TransactionID, status: found.Status, metadata: found.MetaData}, nil
	}

	for _, ordinal := range splitOrdinals {
		child, err := l.transactionByReference(key + ordinal)
		switch {
		case err != nil:
			return nil, err
		case child == nil, child.ParentTransactionID == "":
			continue
		}
		return &recordedTransfer{id: child.ParentTransactionID, status: child.Status, metadata: child.MetaData}, nil
	}
	return nil, nil
}

// transactionByReference reads the one transaction carrying a reference,
// answering with nil where there is none. The lookup is an exact match, so
// "none" is genuinely none rather than a prefix that missed.
func (l *Ledger) transactionByReference(reference string) (*blnkgo.Transaction, error) {
	found, resp, err := l.client.Transaction.GetByReference(reference)
	closeResponse(resp)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if found == nil || found.TransactionID == "" {
		return nil, fmt.Errorf("blnk: the reference %q reads back with no transaction id", reference)
	}
	return found, nil
}

// recordedOn reads back the identity document this package wrote on a
// transaction. A transaction under one of this port's keys that carries no
// document was written behind this package's back, and is reported loudly
// rather than treated as matching nothing: answering "conflict" would
// invite a caller to mint a new key for money that may already have moved.
func recordedOn(metadata map[string]any) (record, error) {
	raw, ok := metadata[metaKeyTransfer]
	if !ok {
		return record{}, errors.New("carries none of this port's annotations, so it was not recorded through this package")
	}
	text, ok := raw.(string)
	if !ok {
		return record{}, fmt.Errorf("carries an annotation of type %T where a JSON document was recorded", raw)
	}
	var stored record
	if err := json.Unmarshal([]byte(text), &stored); err != nil {
		return record{}, fmt.Errorf("carries an annotation that does not decode: %w", err)
	}
	return stored, nil
}

// History streams the account's postings whose PostedAt falls inside window
// ([wallet.Window.Contains]), ordered by ascending PostedAt, each carrying
// the transfer reference and instant the ledger recorded. Swapped bounds
// are refused wrapping [wallet.ErrInvalidWindow], an id EnsureAccount never
// issued wrapping [wallet.ErrUnknownAccount] - both immediately, before the
// iterator exists.
//
// The iterator is lazy: nothing is fetched until iteration begins, so an
// iterator that is never ranged costs nothing. The pages are read in one
// pass and then ordered here rather than trusted in the order they arrive,
// because the contract's ordering is this port's: ascending PostedAt, ties
// broken so that two reads of one window yield the same sequence.
//
// A posting is one movement the caller wrote, not one row the ledger kept.
// The movements are netted per account before a transaction is created, so
// what the ledger stored cannot answer for a transfer that named an account
// twice; the postings come from the document this package records on the
// transaction, and the two are required to agree to the minor unit. That is
// what makes a History read here the same shape as the in-memory and
// Postgres ledgers answer with, which is what the shared conformance suite
// compares.
//
// An account that never received a transfer has no balance row and no
// history: the answer is an empty sequence, not a refusal, because the row
// appears when money first moves and its absence says only that none has.
//
// Two divergences from the in-memory reference are worth stating rather
// than discovering. Ties are broken by the transfer's id, which is stable
// across repeated reads - which is what a watermark resume needs - but is
// not the order the ledger recorded them in, because Blnk's ids are not
// ordered. And only applied transactions are yielded: a queued, inflight or
// rejected transaction has not moved the balance, so it is not a posting.
func (l *Ledger) History(ctx context.Context, id wallet.LedgerAccountID, window wallet.Window) (iter.Seq2[wallet.Posting, error], error) {
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("blnk: %w", err)
	}
	acct, err := l.readAccount(ctx, id)
	if err != nil {
		return nil, err
	}

	return func(yield func(wallet.Posting, error) bool) {
		postings, err := l.postingsOn(ctx, acct, window)
		if err != nil {
			yield(wallet.Posting{}, err)
			return
		}
		for _, p := range postings {
			if err := ctx.Err(); err != nil {
				yield(wallet.Posting{}, fmt.Errorf("blnk: %w", err))
				return
			}
			if !yield(p, nil) {
				return
			}
		}
	}, nil
}

// postingsOn collects every posting the account carries inside the window,
// in the order the port promises.
//
// An account no transfer has named yet has no balance row, so there is no
// id for the filter to match on and nothing to read: the answer is no
// postings, which is what an account holding nothing has.
func (l *Ledger) postingsOn(ctx context.Context, acct account, window wallet.Window) ([]wallet.Posting, error) {
	if acct.balanceID == "" {
		return nil, nil
	}
	rows, err := l.transactionsTouching(ctx, acct)
	if err != nil {
		return nil, err
	}

	// Ordered before the postings are built, so the tie-break is over the
	// ledger's own record rather than over whatever order two filter
	// queries happened to answer in.
	transfers := groupIntoTransfers(rows)
	slices.SortStableFunc(transfers, func(a, b transferGroup) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})

	postings := make([]wallet.Posting, 0, len(transfers))
	for _, transfer := range transfers {
		if !transfer.applied || !window.Contains(transfer.at) {
			continue
		}
		on, err := postingsOfTransfer(transfer, acct)
		if err != nil {
			return nil, err
		}
		postings = append(postings, on...)
	}
	return postings, nil
}

// transferGroup is one transfer as the ledger kept it: every row belonging
// to it, the id they all answer to, and when it happened.
type transferGroup struct {
	// id is what History reports as the posting's TransferRef: the row's
	// own id for a transfer the ledger did not split, and the id the
	// children point at for one it did - which is the id Post returned in
	// both cases, so the domain's join lands on the same value (D7).
	id string
	// rows are the ledger's own records of this transfer, in the order the
	// filter answered with them.
	rows []blnkgo.Transaction
	// at is the earliest instant any of the rows carries. The children of
	// one split are written one at a time, so they can carry instants a
	// microsecond apart; taking the earliest makes the transfer's instant a
	// property of the transfer rather than of which row a page happened to
	// start with, which is what keeps two reads of one window identical.
	at time.Time
	// applied is true only when every row applied. A transfer half of whose
	// rows the ledger did not apply moved part of the money it was asked
	// to, and postingsOfTransfer refuses it by name rather than answering
	// with a fraction of a movement - see there.
	applied bool
}

// groupIntoTransfers reduces the rows a filter answered with to one group
// per transfer.
//
// A transfer the ledger split has no row of its own: it is recorded as one
// child per leg, each naming the transfer it belongs to and carrying a copy
// of its annotation, and the parent is never persisted. So a group is the
// rows sharing a parent, or a single unsplit row, and the account's
// movement in that transfer is the sum over the group - which counts every
// leg once and the transfer's other end once, because each child names that
// end for its own share and no row names it for the whole.
func groupIntoTransfers(rows []blnkgo.Transaction) []transferGroup {
	groups := make(map[string]*transferGroup, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		id := cmp.Or(row.ParentTransactionID, row.TransactionID)
		group, seen := groups[id]
		if !seen {
			group = &transferGroup{id: id, at: row.CreatedAt, applied: true}
			groups[id] = group
			order = append(order, id)
		}
		group.rows = append(group.rows, row)
		if row.CreatedAt.Before(group.at) {
			group.at = row.CreatedAt
		}
		group.applied = group.applied && row.Status == blnkgo.PryTransactionStatusApplied
	}

	out := make([]transferGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *groups[id])
	}
	return out
}

// transactionsTouching reads every transaction naming the account at either
// end, de-duplicated. Two queries are needed because giving and receiving
// are two columns and the filter has no way to ask for either.
//
// Paging stops on an empty page and never on a short one, and the offset
// advances by the rows that arrived rather than by the rows that were asked
// for. A server free to cap a page below the size requested - an ordinary
// default - would otherwise end the read after one page, and a member's
// history would silently lose everything past it while Balance, which is
// the ledger's own figure, went on being right.
//
// The filter asks about the balance id and not about the name. Blnk
// resolves an "@" name to its balance before it records anything, and the
// column it stores is a foreign key into the balances table, so the name
// this package addresses the account by never appears in a recorded row.
func (l *Ledger) transactionsTouching(ctx context.Context, acct account) ([]blnkgo.Transaction, error) {
	seen := make(map[string]bool)
	var found []blnkgo.Transaction
	for _, field := range []string{fieldSource, fieldDestination} {
		offset := 0
		for page := range historyMaxPages {
			batch, err := l.filterTransactions(ctx, field, acct.balanceID, offset)
			if err != nil {
				return nil, err
			}
			if len(batch) == 0 {
				break
			}
			offset += len(batch)
			for _, txn := range batch {
				if txn.TransactionID == "" || seen[txn.TransactionID] {
					continue
				}
				seen[txn.TransactionID] = true
				found = append(found, txn)
			}
			if page == historyMaxPages-1 {
				return nil, fmt.Errorf("blnk: reading the history of account %q: still paging after %d pages", acct.id, historyMaxPages)
			}
		}
	}
	return found, nil
}

// filterTransactions reads one page of the transactions whose field equals
// value.
//
// The request is the SDK's own filter type, so the wire shape is the one
// the SDK defines; the answer is decoded here rather than through the SDK's
// filter result, which models the page as an untyped document and fails
// outright on a page carrying no rows at all. A history with nothing in it
// is an ordinary answer, not an error.
func (l *Ledger) filterTransactions(ctx context.Context, field, value string, offset int) ([]blnkgo.Transaction, error) {
	params := blnkgo.FilterParams{
		Filters:   []blnkgo.Filter{{Field: field, Operator: blnkgo.OpEqual, Value: value}},
		Limit:     historyPageSize,
		Offset:    offset,
		SortBy:    sortByCreatedAt,
		SortOrder: sortAscending,
	}
	var page json.RawMessage
	if err := l.call(ctx, "transactions/filter", http.MethodPost, params, &page); err != nil {
		return nil, fmt.Errorf("blnk: reading the transfers where account %q is the %s: %w", value, field, err)
	}
	transactions, err := decodePage(page)
	if err != nil {
		return nil, fmt.Errorf("blnk: the transfers where account %q is the %s do not read as transactions: %w", value, field, err)
	}
	return transactions, nil
}

// pageRowsKey is the key a filtered page carries its rows under when it
// arrives wrapped in an envelope.
const pageRowsKey = "data"

// decodePage reads one page of transactions, accepting both shapes a
// filtered read comes back in: an envelope carrying the rows under data,
// and the bare array older servers answer with. An absent, null or empty
// page is no transactions rather than a failure.
//
// An envelope that does not carry the rows key at all is an error, not an
// empty page. The shape of this endpoint's answer is one of the things only
// a real server can settle, and a page arriving under some other name -
// or an error document answered with a 200 - would otherwise read as "this
// member has no postings", which is a member's money reported as absent.
func decodePage(page json.RawMessage) ([]blnkgo.Transaction, error) {
	rows := bytes.TrimSpace(page)
	if len(rows) == 0 || string(rows) == "null" {
		return nil, nil
	}
	if rows[0] == '{' {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(rows, &envelope); err != nil {
			return nil, err
		}
		data, carried := envelope[pageRowsKey]
		if !carried {
			return nil, fmt.Errorf("the page carries %v and no %q, so its rows are somewhere this package does not read; an unrecognised page is not an empty history",
				slices.Sorted(maps.Keys(envelope)), pageRowsKey)
		}
		rows = bytes.TrimSpace(data)
		if len(rows) == 0 || string(rows) == "null" {
			return nil, nil
		}
	}
	var transactions []blnkgo.Transaction
	if err := json.Unmarshal(rows, &transactions); err != nil {
		return nil, err
	}
	return transactions, nil
}

// postingsOfTransfer works out what one transfer did to one account.
//
// The answer is the postings the CALLER wrote, read back from the identity
// document this package records on every transaction it creates - one
// wallet.Posting per input posting, in the order they were written, exactly
// as the in-memory and Postgres ledgers answer. What the ledger stored
// cannot answer it: the movements are netted per account before anything is
// sent, so a transfer written as 100 out and 200 out on one account reaches
// Blnk as a single movement of 300, and the port is explicit that those are
// different records.
//
// What the ledger stored is still read, and the two are required to agree
// to the minor unit. The document says what was asked for and the rows say
// what was recorded; a transfer where they differ is a transfer this
// package did not send the way it thinks it did - a split half applied,
// most of all - and that is worth stopping on rather than reporting either
// figure as the truth.
//
// The document is read from ONE row of the group. Every child of a split
// carries a copy of the whole transfer's annotation, so reading it per row
// would report a rounded earning once per leg.
//
// A transfer carrying no such document was written behind this package's
// back - a neighbouring system posting on one of these balances - and there
// is nothing to read but what the ledger recorded, so it yields the one
// netted posting the rows describe.
func postingsOfTransfer(transfer transferGroup, acct account) ([]wallet.Posting, error) {
	written, annotated, err := writtenPostings(transfer, acct)
	if err != nil {
		return nil, err
	}
	net, touched, err := netOf(transfer, acct)
	if err != nil {
		return nil, err
	}
	switch {
	case annotated && len(written) == 0:
		return nil, nil
	case !annotated && (!touched || net.IsZero()):
		return nil, nil
	}
	for _, row := range transfer.rows {
		if row.Currency != string(acct.currency) {
			return nil, fmt.Errorf("blnk: transfer %q moves %s on account %q, which holds %s: %w",
				transfer.id, row.Currency, acct.id, acct.currency, money.ErrCurrencyMismatch)
		}
	}

	posting := func(a money.Amount) wallet.Posting {
		return wallet.Posting{
			Account:     acct.id,
			Amount:      a,
			TransferRef: wallet.TransferRef(transfer.id),
			PostedAt:    transfer.at,
		}
	}
	if !annotated {
		return []wallet.Posting{posting(net)}, nil
	}

	sum, err := money.Zero(acct.currency)
	if err != nil {
		return nil, err
	}
	out := make([]wallet.Posting, 0, len(written))
	for _, amount := range written {
		if sum, err = sum.Add(amount); err != nil {
			return nil, fmt.Errorf("blnk: totalling what transfer %q did to account %q: %w", transfer.id, acct.id, err)
		}
		out = append(out, posting(amount))
	}
	if !sum.Equal(net) {
		return nil, fmt.Errorf(
			"blnk: transfer %q says it moved %s on account %q but the ledger recorded %s across the %d row(s) it kept for it; the transfer this package sent and what the ledger kept are not the same movement",
			transfer.id, sum, acct.id, net, len(transfer.rows))
	}
	return out, nil
}

// writtenPostings reads the account's own postings out of the identity
// document this package records on the transactions it creates, reporting
// annotated false for a transfer that carries none.
//
// One row of the group is read and the rest ignored, because a split's
// children each carry a copy of the same document - see
// [postingsOfTransfer]. The first row is as good as any: they are copies.
func writtenPostings(transfer transferGroup, acct account) (written []money.Amount, annotated bool, err error) {
	if len(transfer.rows) == 0 {
		return nil, false, nil
	}
	row := transfer.rows[0]
	if _, carried := row.MetaData[metaKeyTransfer]; !carried {
		return nil, false, nil
	}
	stored, err := recordedOn(row.MetaData)
	if err != nil {
		return nil, false, fmt.Errorf("blnk: the transfer recorded as transaction %q: %w", transfer.id, err)
	}
	for _, p := range stored.Postings {
		if p.Account != string(acct.id) {
			continue
		}
		if p.Currency != string(acct.currency) {
			return nil, false, fmt.Errorf("blnk: transfer %q records %s on account %q, which holds %s: %w",
				transfer.id, p.Currency, acct.id, acct.currency, money.ErrCurrencyMismatch)
		}
		written = append(written, money.Amount{Minor: p.Minor, Currency: acct.currency})
	}
	return written, true, nil
}

// netOf sums what one transfer's rows record against one account, reporting
// touched false when none of them names the account - which is how a row
// the filter returned but that names the account at neither end is dropped
// rather than trusted.
//
// Every row of the group counts, and that is what makes a split add up: a
// split is recorded as one child per leg, each naming its own leg account
// for its own share and the transfer's other end for the same share, so
// summing the children gives every account exactly what the transfer moved
// on it. There is no parent row to count a second time.
//
// A row is matched against the account by the balance id the ledger
// resolved the name to, and by the name itself. The recorded row carries
// the id; the name is accepted too because a row is persisted before the
// resolution is written back, so a read racing a write can see either.
func netOf(transfer transferGroup, acct account) (money.Amount, bool, error) {
	touched := false
	net, err := money.Zero(acct.currency)
	if err != nil {
		return money.Amount{}, false, err
	}

	names := func(end string) bool {
		return end != "" && (end == acct.balanceID || end == string(acct.id))
	}
	for _, row := range transfer.rows {
		for _, side := range []struct {
			end      string
			negative bool
		}{
			{end: row.Source, negative: true},
			{end: row.Destination, negative: false},
		} {
			if !names(side.end) {
				continue
			}
			if row.PreciseAmount == nil {
				// The other spelling of the amount is a float, and a
				// float is never read as money here (C-6). A transfer
				// this package recorded always carries the integer, so
				// one that does not was written behind its back.
				return money.Amount{}, false, fmt.Errorf(
					"blnk: transaction %q on account %q carries no integer amount, and no float is read as money here", row.TransactionID, acct.id)
			}
			magnitude, err := exactInt64(row.PreciseAmount)
			if err != nil {
				return money.Amount{}, false, fmt.Errorf("blnk: the amount of transaction %q: %w", row.TransactionID, err)
			}
			if side.negative {
				magnitude = -magnitude
			}
			sum, err := net.Add(money.Amount{Minor: magnitude, Currency: acct.currency})
			if err != nil {
				return money.Amount{}, false, fmt.Errorf("blnk: totalling what transfer %q did to account %q: %w", transfer.id, acct.id, err)
			}
			net = sum
			touched = true
		}
	}
	return net, touched, nil
}

// exactInt64 narrows a ledger integer to the port's minor units, refusing
// anything that does not fit rather than truncating it. This is the money
// boundary in one function: every arbitrary-precision integer the SDK hands
// back passes through here, and a figure this repository cannot represent
// is an error, never a wrapped-around number (C-6).
//
// A missing integer is refused too, rather than read as zero. An account
// holding nothing is answered with a zero, not with a silence, so a silence
// means the field was not the one this package thinks it is - and reading
// that as an empty account would report a member's money as gone.
func exactInt64(value *big.Int) (int64, error) {
	if value == nil {
		return 0, errors.New("the ledger answered with no figure at all where an integer was expected")
	}
	if !value.IsInt64() {
		return 0, fmt.Errorf("%w: %s does not fit the minor units this port carries", money.ErrOverflow, value.String())
	}
	return value.Int64(), nil
}

// call sends one request the SDK has no typed method for, through the SDK's
// own client so the endpoint, the credential and the error decoding stay in
// one place.
func (l *Ledger) call(ctx context.Context, endpoint, method string, body, into any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("blnk: %w", err)
	}
	req, err := l.client.NewRequest(endpoint, method, body)
	if err != nil {
		return err
	}
	resp, err := l.client.CallWithRetry(req, into)
	closeResponse(resp)
	return err
}

// closeResponse closes a response body the SDK handed back. The SDK closes
// every body it decodes, so this is ordinarily a second close on an already
// closed reader - which is harmless - and it is done anyway because the
// paths that return a response WITHOUT decoding it (a retry it gave up on,
// a transport error) are exactly the ones a caller would otherwise leak a
// connection on.
func closeResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}

// apiStatus reports the HTTP status behind an SDK error, and whether there
// was one at all. A transport failure has no status and must not be read as
// any particular refusal.
func apiStatus(err error) (int, bool) {
	apiErr, ok := blnkgo.AsApiErrorResponse(err)
	if !ok {
		return 0, false
	}
	return apiErr.Status, true
}

// isNotFound reports whether the ledger said the thing asked for does not
// exist.
func isNotFound(err error) bool {
	status, ok := apiStatus(err)
	return ok && status == http.StatusNotFound
}

// Structured error codes this package branches on. The SDK's guidance is
// to read the machine code and never the message text, and these are the
// codes it carries; the message is consulted only for the older flat shape
// that has no code at all.
//
// They are constants here rather than literals at the branch so that
// confirming them against a running server - which is the only place they
// can be confirmed - is a change in one place.
const (
	// codeUnknown is what the SDK puts on a response carrying only a flat
	// error string, so it means "there was no code" rather than naming a
	// refusal.
	codeUnknown = "UNKNOWN"
	// codeDuplicateReference is the reference - the idempotency key -
	// already being recorded.
	codeDuplicateReference = "TXN_DUPLICATE_REFERENCE"
	// codeConflict is the generic already-exists refusal.
	codeConflict = "GEN_CONFLICT"
	// codeInsufficientFunds is a source the ledger would have to leave
	// short.
	codeInsufficientFunds = "TXN_INSUFFICIENT_FUNDS"
)

// errorCode reads the structured code off a refusal, reporting ok false
// when the response carried none - which is the older flat shape, where the
// reason is only in the message.
func errorCode(apiErr *blnkgo.ApiErrorResponse) (string, bool) {
	if apiErr.ErrorDetail == nil {
		return "", false
	}
	code := strings.ToUpper(strings.TrimSpace(apiErr.ErrorDetail.Code))
	if code == "" || code == codeUnknown {
		return "", false
	}
	return code, true
}

// isConflict reports whether the ledger refused because something already
// exists. For a transaction that is a reference already used - the
// idempotency key - which is judged rather than assumed to be a replay.
//
// The status alone is not enough. Blnk refuses a duplicate reference in
// application code as well as at a unique index, and the two failures reach
// the handler by different routes, so which status either chooses is the
// server's business; a refusal naming the duplicate in its structured code
// is a duplicate whatever status carried it. Reading only the status would
// hand the caller that lost the race a hard error instead of the winner's
// reference.
func isConflict(err error) bool {
	apiErr, ok := blnkgo.AsApiErrorResponse(err)
	if !ok {
		return false
	}
	if apiErr.Status == http.StatusConflict {
		return true
	}
	if apiErr.Status < 400 || apiErr.Status >= 500 {
		return false
	}
	code, hasCode := errorCode(apiErr)
	return hasCode && (code == codeDuplicateReference || code == codeConflict)
}

// insufficientFundsPhrases are the phrases that identify the ledger's
// refusal to leave a source short in a response carrying no structured
// code. They are whole phrases rather than bare words because the text they
// are matched against is written by whatever answered: "insufficient" alone
// matches an authenticating proxy's insufficient_scope and a gateway's
// "507 Insufficient Storage", and reporting an outage to the domain as "the
// member cannot afford this" turns a retryable failure into a member-facing
// denial.
var insufficientFundsPhrases = []string{
	"insufficient fund",
	"insufficient balance",
	"overdraft not allowed",
	"overdraft is not allowed",
}

// isInsufficientFunds reports whether the ledger refused the transfer for
// want of money in a source account.
//
// The structured code decides it where there is one. Where there is not -
// the older flat shape, which the SDK maps to the code UNKNOWN - the
// ledger's own message is read, and nothing else: not the HTTP status line,
// which the SDK puts in Message, and not the raw response body, which is
// whatever answered and on a validation failure commonly echoes the request
// that caused it. This package's own requests carry the field name
// allow_overdraft and the caller's Reference and Metadata verbatim, so a
// reader of the body would report a malformed request as a member's empty
// balance.
//
// The status is checked too: a refusal for want of money is a refusal, and
// a 5xx is an outage whatever words it arrived with.
func isInsufficientFunds(err error) bool {
	apiErr, ok := blnkgo.AsApiErrorResponse(err)
	if !ok {
		return false
	}
	switch apiErr.Status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
	default:
		return false
	}
	if code, hasCode := errorCode(apiErr); hasCode {
		return code == codeInsufficientFunds
	}

	said := strings.ToLower(apiErr.LegacyError)
	if apiErr.ErrorDetail != nil {
		said += " " + strings.ToLower(apiErr.ErrorDetail.Message)
	}
	for _, phrase := range insufficientFundsPhrases {
		if strings.Contains(said, phrase) {
			return true
		}
	}
	return false
}
