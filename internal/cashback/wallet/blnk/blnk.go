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
// transaction moves one currency's worth of value from one or more sources
// to one or more destinations. The mapping is therefore:
//
//   - a [wallet.AccountRef] in a currency is one Blnk balance, created in
//     this ledger's own Blnk ledger and found again by a deterministic
//     indicator derived from the pair AND from that ledger (see
//     [Ledger.EnsureAccount]);
//   - a [wallet.Transfer] is one Blnk transaction, whose reference is the
//     transfer's idempotency key - which is what makes a replay a duplicate
//     reference rather than a second movement of money;
//   - a [wallet.Posting] is one movement the caller wrote, read back from
//     the document this package records on the transaction rather than
//     from the legs, which are netted per account for the wire.
//
// # How a split transfer is recorded and read back
//
// A transfer touching more than one account on a side becomes one Blnk
// transaction carrying legs, and Blnk keeps a child transaction beneath it
// for each leg. Both are ordinary rows, so the filter History reads through
// answers with the parent for the account that travelled as the scalar end
// and with a child for each leg account - and a reader that took every row
// it was handed as a posting would count the scalar side's movement once
// for the parent and once for every child.
//
// So the rows are grouped by the transfer they belong to, the parent is the
// only one read, and a parent the filter did not return is fetched by id.
// That costs one extra read per split whose parent was not matched, and it
// is what makes a member's history of a rounded earning - one funding
// account, a member bucket and a rounding remainder (D6) - come back as the
// one posting the caller wrote.
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
// transfer per currency, each with its own key. Within one currency the
// shape is unrestricted: any number of accounts giving and any number
// receiving become one transaction with source and destination legs.
//
// Five smaller divergences from the in-memory reference are real and are
// stated here rather than left to be discovered:
//
//   - PostedAt is the ledger's instant, not a clock this process can set.
//     [WithClock] moves the settle deadline and nothing else, so a test
//     that needs postings at chosen instants draws its windows from what
//     History returns instead of from instants it invented.
//   - History's tie-break is the ledger's transaction id. It is stable
//     across repeated reads, which is what a watermark resume needs, but
//     the ids are not ordered, so it is not the order the ledger recorded
//     them in. Postings sharing an instant are the ordinary case within one
//     transfer, and those all carry one reference anyway.
//   - History finds a transfer by the account being one end of a recorded
//     transaction, and the integration suite posts a many-to-many transfer
//     and reads it back to hold the substrate to it - because a history
//     that silently omitted a leg would understate a member's total while
//     Balance, which is the ledger's own figure, went on being right.
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
//     one netted posting the legs describe. The other two implementations
//     have no such transactions to answer for.
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
// CI job. Four readings of the substrate are encoded here and cannot be
// confirmed anywhere else, and each is written to fail loudly rather than
// guess:
//
//   - that a split is recorded as a parent transaction with a child per
//     leg. A child whose parent this package cannot read makes History
//     fail, naming the transfer it could not read, rather than answering
//     for the leg under the leg's own reference.
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
	// DefaultLedgerName is the name of the Blnk ledger this package keeps
	// its balances in when no id or name is configured. Blnk groups
	// balances into named ledgers, and the resolved group is part of every
	// account name this package derives, which is what keeps its accounts
	// apart from anything else pointed at the same server - the endpoint
	// that finds a balance by name carries no ledger of its own.
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
// it is large because the alternative to one round trip per few postings is
// a member's wallet view costing dozens.
const historyPageSize = 200

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
// mapping table anywhere.
//
// The parts are joined with a full stop and every part is restricted to
// characters that need no escaping, because the indicator travels in a URL
// path when the balance is looked up. A separator that could appear inside
// a part would let two different accounts spell one indicator, which is a
// member's money merged with somebody else's.
//
// The first part is the Blnk ledger the balance belongs to, and it is what
// makes [WithLedgerID] and [WithLedgerName] isolate anything at all: the
// endpoint that finds a balance by name carries no ledger, so two
// deployments pointed at one Blnk server would otherwise resolve one
// another's house accounts - the configured names are the same strings in
// every environment - and post each other's money. The resolved ledger id
// is used rather than the configured name, because a deployment may name
// its ledger or pin its id and both spellings must derive one account.
const (
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
// of the Blnk ledger its balances sit in, and the account ids it has
// already resolved - both immutable facts about a server that never deletes
// either, cached so the common path costs one round trip rather than three.
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

	// mu guards the two caches below and the per-account locks.
	mu sync.Mutex
	// ledgerID is the resolved Blnk ledger, empty until first resolved.
	ledgerID string
	// accounts memoises indicator -> balance id. A Blnk balance is never
	// deleted and its indicator never changes, so an entry can never go
	// stale; it can only save a lookup.
	accounts map[string]wallet.LedgerAccountID
	// ensuring holds one lock per account name, so two goroutines racing
	// to create one account do not both create it. Entries are never
	// removed: there is one per account this process has ever ensured,
	// which is the same bound the memo above already carries.
	ensuring map[string]*sync.Mutex
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

// WithLedgerID pins the Blnk ledger balances are created in, skipping the
// lookup by name. Use it where the ledger is provisioned outside this
// process and its id is configuration. The ledger it names is part of every
// account name this value derives, so two deployments over one Blnk server
// keep their accounts apart by choosing different ledgers and by nothing
// else.
func WithLedgerID(id string) Option {
	return func(l *Ledger) { l.ledgerIDOpt = strings.TrimSpace(id) }
}

// WithLedgerName names the Blnk ledger balances are created in, to be
// resolved on first use and created if it does not exist. An empty name
// keeps [DefaultLedgerName]: a nameless ledger would be found by nothing.
// The resolved ledger is part of every account name this value derives, so
// two deployments sharing one Blnk server keep their accounts apart by
// naming different ledgers.
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
		accounts:       make(map[string]wallet.LedgerAccountID),
		ensuring:       make(map[string]*sync.Mutex),
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

// EnsureAccount resolves ref in currency to the Blnk balance that holds it,
// creating the balance the first time the pair is named.
//
// Identity is an indicator derived deterministically from the Blnk ledger
// this value is configured for and from (ref, currency) -
// <ledger>.member.<uuid>.<stage>.<CUR> or <ledger>.house.<hex of the
// name>.<CUR> - which is what makes the call idempotent with no mapping
// table on this side: the same pair spells the same indicator on every
// call, in every process, and the balance is found by looking that
// indicator up. The returned id is Blnk's own balance id and is opaque to
// every caller, as the port says.
//
// The ledger is part of the name because the endpoint that looks a name up
// carries no ledger: two deployments pointed at one Blnk server would
// otherwise resolve one another's accounts, and house names come from
// configuration, so "commission" is the same string in staging and in
// production. Member accounts are no safer - a database restored from
// production carries the same member ids. Naming the ledger is what makes
// [WithLedgerName] and [WithLedgerID] isolate anything.
//
// The derivation is injective. Member ids are fixed-format uuids and stages
// come from a closed set; house names are hex-encoded, so a configured name
// holding a separator, a slash or a space cannot collide with another name
// - two configured names are two accounts, which is what stops a
// configuration typo quietly sharing a balance with the account it was
// mistyped from. No member identity beyond the opaque account uuid reaches
// the ledger.
//
// An unusable ref is refused wrapping [wallet.ErrInvalidAccountRef], a
// malformed currency wrapping [money.ErrInvalidCurrency], both before any
// I/O. A created balance is read back and its indicator checked: a server
// that did not record the name would otherwise hand out a new balance on
// every call and split one member's money across all of them, and that is a
// failure worth stopping on rather than discovering in a reconciliation.
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
	indicator := indicatorFor(ledgerID, ref, currency)
	if id, ok := l.cachedAccount(indicator); ok {
		return id, nil
	}

	// One lock per account name. Two goroutines ensuring one account must
	// not both create it: Blnk mints a balance id per create, and two
	// balances under one indicator is one member's money in two places.
	// Different accounts do not wait on each other.
	unlock := l.lockAccount(indicator)
	defer unlock()
	if id, ok := l.cachedAccount(indicator); ok {
		return id, nil
	}

	id, err := l.resolveAccount(ctx, ledgerID, indicator, currency)
	if err != nil {
		return "", err
	}
	l.cacheAccount(indicator, id)
	return id, nil
}

// resolveAccount finds the balance carrying indicator, creating it when
// there is none. The lookup runs again after a create, so two processes
// that raced converge on whichever balance the lookup answers with rather
// than each keeping the one it made.
//
// A create that fails is not given up on until the lookup has been asked
// once more. Two processes cold-starting on one account race, and a server
// that refuses the second create is indistinguishable from one that
// accepted it - exactly the argument createLedger already makes - so the
// losing process converges instead of failing an EnsureAccount that a
// retry would have satisfied.
//
// The balance the read-back answers with is compared against the one the
// create returned. They differ only if this process has just leaked a
// second balance under a name that is supposed to be the account's whole
// identity, which would leave one account's money split across two
// balances with nothing to find it later. That is reported rather than
// memoised.
func (l *Ledger) resolveAccount(ctx context.Context, ledgerID, indicator string, currency money.Currency) (wallet.LedgerAccountID, error) {
	found, err := l.balanceByIndicator(ledgerID, indicator, currency)
	switch {
	case err == nil:
		return wallet.LedgerAccountID(found.BalanceID), nil
	case !isNotFound(err):
		return "", fmt.Errorf("blnk: looking up account %q: %w", indicator, err)
	}

	created, createErr := l.createBalance(ctx, ledgerID, indicator, currency)
	if createErr != nil {
		// A create that lost a race to another process has already put the
		// account there, so the lookup - not the refusal - is the answer.
		if raced, err := l.balanceByIndicator(ledgerID, indicator, currency); err == nil {
			return wallet.LedgerAccountID(raced.BalanceID), nil
		}
		return "", createErr
	}

	// Read back through the same lookup every other caller will use. If it
	// answers, its answer is the one that converges; if it does not, the
	// server did not record the indicator and every future EnsureAccount
	// would create another balance.
	found, err = l.balanceByIndicator(ledgerID, indicator, currency)
	if err != nil {
		return "", fmt.Errorf(
			"blnk: account %q was created as balance %q but cannot be found by name; the ledger did not record the name, so every call would create another balance and one account's money would be split across them: %w",
			indicator, created.BalanceID, err)
	}
	if found.BalanceID != created.BalanceID {
		return "", fmt.Errorf(
			"blnk: account %q was created as balance %q but the name now answers with balance %q; two balances carry one account's name, so its money is already split across them and no later reconciliation finds it",
			indicator, created.BalanceID, found.BalanceID)
	}
	return wallet.LedgerAccountID(found.BalanceID), nil
}

// balanceByIndicator looks one balance up by the name this package gave it,
// and checks that what came back is the account that was asked for. The
// check is not ceremony: the lookup is the whole of this package's account
// identity, and a server answering with some other balance would be money
// posted to the wrong account.
//
// The balance must also sit in the Blnk ledger this value is configured
// for. The endpoint carries no ledger, so a server shared by two
// deployments answers both of them from one pool of names; a balance
// belonging to another ledger is reported as absent, which is what it is
// from here.
func (l *Ledger) balanceByIndicator(ledgerID, indicator string, currency money.Currency) (*blnkgo.LedgerBalance, error) {
	balance, resp, err := l.client.LedgerBalance.GetByIndicator(indicator, string(currency))
	closeResponse(resp)
	if err != nil {
		return nil, err
	}
	if balance == nil || balance.BalanceID == "" {
		return nil, fmt.Errorf("blnk: the ledger answered for account %q with no balance id", indicator)
	}
	if balance.Indicator != indicator {
		return nil, fmt.Errorf("blnk: looking up account %q answered with an account named %q", indicator, balance.Indicator)
	}
	if balance.LedgerID != ledgerID {
		return nil, notFoundError{fmt.Errorf("blnk: account %q sits in the ledger %q, not %q", indicator, balance.LedgerID, ledgerID)}
	}
	if balance.Currency != string(currency) {
		return nil, fmt.Errorf("blnk: account %q holds %s, not %s", indicator, balance.Currency, currency)
	}
	return balance, nil
}

// notFoundError marks a refusal this package makes for itself that means
// the same thing as the ledger answering "no such thing": the account is
// not there to be found, so the caller creates it rather than failing.
type notFoundError struct{ err error }

func (e notFoundError) Error() string { return e.err.Error() }
func (e notFoundError) Unwrap() error { return e.err }

// createBalanceRequest is the create-a-balance body this package sends. The
// SDK's own request type carries no indicator field, and the indicator is
// this package's entire account identity, so the body is written here and
// posted through the SDK's client - which keeps the transport, the
// credential and the error decoding shared with every other call.
type createBalanceRequest struct {
	LedgerID  string `json:"ledger_id"`
	Currency  string `json:"currency"`
	Indicator string `json:"indicator"`
}

// createBalance creates one empty balance under the given name.
func (l *Ledger) createBalance(ctx context.Context, ledgerID, indicator string, currency money.Currency) (*blnkgo.LedgerBalance, error) {
	body := createBalanceRequest{LedgerID: ledgerID, Currency: string(currency), Indicator: indicator}
	created := new(blnkgo.LedgerBalance)
	if err := l.call(ctx, "balances", http.MethodPost, body, created); err != nil {
		return nil, fmt.Errorf("blnk: creating account %q: %w", indicator, err)
	}
	if created.BalanceID == "" {
		return nil, fmt.Errorf("blnk: creating account %q returned no balance id", indicator)
	}
	return created, nil
}

// resolveLedgerID answers with the Blnk ledger this package's balances live
// in, resolving it once per process. A configured id is taken as given; a
// name is looked up and created if absent, and the list is re-read after a
// create so two processes that raced converge on one ledger rather than
// each keeping its own.
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

// knownLedgerID answers with the Blnk ledger this package's balances live
// in when there already is one, and with the empty string when the
// configured name names no ledger yet. It creates nothing, which is what
// makes it usable from the read paths: a Balance or a History over an id
// nobody issued must not bring a ledger into existence as a side effect.
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
	scope := hex.EncodeToString([]byte(ledgerID))
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
// anything it wrote for another ledger - which is how an id EnsureAccount
// never issued, here, is told from one it did.
func parseIndicator(ledgerID, indicator string) (kind accountKind, currency money.Currency, ok bool) {
	parts := strings.Split(indicator, indicatorSeparator)
	if len(parts) < 2 || parts[0] != hex.EncodeToString([]byte(ledgerID)) {
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

// account is what Post and Balance need to know about one ledger account:
// the currency it holds, whether the solvency rule governs it, and what it
// holds right now.
type account struct {
	id       wallet.LedgerAccountID
	kind     accountKind
	currency money.Currency
	held     money.Amount
}

// readAccount reads one balance and turns it into this package's view of
// the account. An id Blnk does not know, an id Blnk knows but EnsureAccount
// did not issue, and an id belonging to another deployment's Blnk ledger
// are all refused wrapping [wallet.ErrUnknownAccount]: the port's promise
// is about ids this ledger handed out, and a balance somebody else created
// - in this ledger or in a neighbouring one on the same server - is not one
// of them.
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
	balance, resp, err := l.client.LedgerBalance.Get(string(id))
	closeResponse(resp)
	if isNotFound(err) {
		return account{}, fmt.Errorf("%w: %q", wallet.ErrUnknownAccount, id)
	}
	if err != nil {
		return account{}, fmt.Errorf("blnk: reading account %q: %w", id, err)
	}
	if balance == nil {
		return account{}, fmt.Errorf("blnk: reading account %q returned nothing", id)
	}
	if balance.LedgerID != ledgerID {
		return account{}, fmt.Errorf("%w: %q sits in the ledger %q, and this one keeps its accounts in %q",
			wallet.ErrUnknownAccount, id, balance.LedgerID, ledgerID)
	}

	kind, named, ok := parseIndicator(ledgerID, balance.Indicator)
	if !ok {
		return account{}, fmt.Errorf("%w: %q is a ledger balance this port did not create", wallet.ErrUnknownAccount, id)
	}
	held, err := money.ParseCurrency(balance.Currency)
	if err != nil {
		return account{}, fmt.Errorf("blnk: account %q: %w", id, err)
	}
	if held != named {
		return account{}, fmt.Errorf("blnk: account %q is named for %s but holds %s", id, named, held)
	}

	// The one place a ledger figure becomes an Amount. A balance too large
	// for an int64 is refused rather than truncated: a balance that reads
	// as its own negation is worse than an error (C-6).
	minor, err := exactInt64(balance.Balance)
	if err != nil {
		return account{}, fmt.Errorf("blnk: the balance of account %q: %w", id, err)
	}
	return account{id: id, kind: kind, currency: held, held: money.Amount{Minor: minor, Currency: held}}, nil
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
// currency. A figure too large for an int64 is refused wrapping
// [money.ErrOverflow] rather than wrapped around.
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
//     transfer in which a house account that must go below nothing gives
//     beside a member's stage account is refused the same way, once the
//     accounts have been read: the overdraft this ledger grants covers a
//     whole transaction, so recording that shape would mean giving up the
//     member's protection. See below.
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
// The reference returned is Blnk's transaction id, and it is readable the
// moment Post returns. Blnk queues transactions by default, which would
// make a wallet recomputed straight after a transition show the total from
// before it, so every transaction this package creates asks to skip the
// queue and Post does not return until the ledger reports it applied. If it
// has not applied inside the settle window the call fails rather than
// claiming a visibility it does not have - and because the key is already
// recorded, the caller's retry is a replay, not a second movement.
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
		return wallet.TransferRef(replay.TransactionID), nil
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
		return wallet.TransferRef(winner.TransactionID), nil
	}
	if created == nil || created.TransactionID == "" {
		return "", fmt.Errorf("blnk: the transfer under key %q was accepted but no transaction id came back", transfer.IdempotencyKey)
	}
	if err := l.awaitApplied(ctx, transfer.IdempotencyKey, created); err != nil {
		return "", l.explainRefusal(ctx, transfer, err)
	}
	return wallet.TransferRef(created.TransactionID), nil
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
// side with several as legs, because the SDK refuses a request carrying
// both spellings of one side. A transfer with more than one leg anywhere is
// marked atomic, which is what makes the split legs succeed or fail
// together rather than one at a time.
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
			// makes a replayed dispatch safe (C-5).
			Reference: transfer.IdempotencyKey,
			Currency:  string(shape.currency),
			Precision: l.precision,
			// The integer, and only the integer. Amount keeps its zero
			// value: Blnk refuses a request carrying both, and a float in
			// a money position is a rounding error waiting for an audit.
			PreciseAmount: big.NewInt(shape.total),
			Description:   transfer.Reference,
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
func (l *Ledger) explainCreateFailure(transfer wallet.Transfer, recorded record, err error) (*blnkgo.Transaction, error) {
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
				transfer.IdempotencyKey, winner.TransactionID, err)
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
// The transaction the create returned is passed in when there is one, so
// the ordinary case - skip_queue accepted, applied inline - costs no extra
// round trip. Anything else is re-read until it applies or the settle
// window closes. A transaction the ledger declined to apply is reported as
// a [refusedTransfer] for Post to answer for, and a status this package
// does not know is reported immediately rather than polled for: the money
// question behind an unrecognised answer does not get clearer by asking the
// same question again for fifteen seconds.
func (l *Ledger) awaitApplied(ctx context.Context, key string, created *blnkgo.Transaction) error {
	status := blnkgo.PryTransactionStatus("")
	if created != nil {
		status = created.Status
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
		found, resp, err := l.client.Transaction.GetByReference(key)
		closeResponse(resp)
		switch {
		case err != nil && !isNotFound(err):
			return fmt.Errorf("blnk: waiting for transfer %q to be readable: %w", key, err)
		case err == nil && found != nil:
			status = found.Status
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
// the ledger: the recorded transaction for a true replay, a conflict for
// the same key carrying a different transfer, and keyUnclaimed when the key
// is not recorded at all. The transaction is handed back rather than only
// its id, so the caller can see whether it has applied without asking
// again.
//
// A transaction the ledger refused is judged before its contents are, and
// answers keyRefused whatever it was carrying. Nothing was recorded under
// the key, so there is no transfer for a second post to collide with -
// reporting a conflict there would strand a caller that corrected a refused
// transfer, and reporting the refusal again would strand one that simply
// retried it. Keys are derived from the domain fact that caused the
// transfer (D8), so neither caller can escape by minting a new one.
func (l *Ledger) judgeRecordedKey(transfer wallet.Transfer, incoming record) (*blnkgo.Transaction, keyState, error) {
	found, resp, err := l.client.Transaction.GetByReference(transfer.IdempotencyKey)
	closeResponse(resp)
	if isNotFound(err) {
		return nil, keyUnclaimed, nil
	}
	if err != nil {
		return nil, keyUnclaimed, fmt.Errorf("blnk: reading idempotency key %q: %w", transfer.IdempotencyKey, err)
	}
	if found == nil || found.TransactionID == "" {
		return nil, keyUnclaimed, fmt.Errorf("blnk: idempotency key %q reads back with no transaction id", transfer.IdempotencyKey)
	}
	if refused(found.Status) {
		return found, keyRefused, nil
	}

	stored, err := recordedOn(found)
	if err != nil {
		return nil, keyClaimed, fmt.Errorf("blnk: the transfer recorded under key %q: %w", transfer.IdempotencyKey, err)
	}
	if !stored.matches(incoming) {
		return nil, keyClaimed, fmt.Errorf("%w: key %q", wallet.ErrIdempotencyConflict, transfer.IdempotencyKey)
	}
	return found, keyClaimed, nil
}

// recordedOn reads back the identity document this package wrote on a
// transaction. A transaction under one of this port's keys that carries no
// document was written behind this package's back, and is reported loudly
// rather than treated as matching nothing: answering "conflict" would
// invite a caller to mint a new key for money that may already have moved.
func recordedOn(txn *blnkgo.Transaction) (record, error) {
	raw, ok := txn.MetaData[metaKeyTransfer]
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
// A posting is one movement the caller wrote, not one leg on the wire. The
// legs are netted per account before a transaction is created, so they
// cannot answer for a transfer that named an account twice; the postings
// come from the document this package records on the transaction, and the
// two are required to agree to the minor unit. That is what makes a History
// read here the same shape as the in-memory and Postgres ledgers answer
// with, which is what the shared conformance suite compares.
//
// Two divergences from the in-memory reference are worth stating rather
// than discovering. Ties are broken by the ledger's transaction id, which
// is stable across repeated reads - which is what a watermark resume needs
// - but is not the order the ledger recorded them in, because Blnk's ids
// are not ordered. And only applied transactions are yielded: a queued,
// inflight or rejected transaction has not moved the balance, so it is not
// a posting.
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
func (l *Ledger) postingsOn(ctx context.Context, acct account, window wallet.Window) ([]wallet.Posting, error) {
	rows, err := l.transactionsTouching(ctx, acct.id)
	if err != nil {
		return nil, err
	}
	transfers, err := l.transfersAmong(ctx, rows)
	if err != nil {
		return nil, err
	}

	// Ordered before the postings are built, so the tie-break is over the
	// ledger's own record rather than over whatever order two filter
	// queries happened to answer in.
	slices.SortStableFunc(transfers, func(a, b blnkgo.Transaction) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.TransactionID, b.TransactionID)
	})

	postings := make([]wallet.Posting, 0, len(transfers))
	for _, txn := range transfers {
		if txn.Status != blnkgo.PryTransactionStatusApplied {
			continue
		}
		if !window.Contains(txn.CreatedAt) {
			continue
		}
		on, err := postingsOfTransfer(txn, acct)
		if err != nil {
			return nil, err
		}
		postings = append(postings, on...)
	}
	return postings, nil
}

// transfersAmong reduces the rows a filter answered with to one transaction
// per transfer: the one this package created.
//
// Blnk records a transfer whose legs are split across several balances as a
// parent transaction carrying the legs, with one child transaction beneath
// it per leg (that is what the SDK's parent_transaction field is for). Both
// are ordinary rows, so a filter on an account that travelled as the scalar
// end of a split answers with the parent AND with every child - and reading
// each of them as a posting would count that account's movement twice over.
// Grouping by parent is what makes that impossible: the transfer is one
// thing however many rows the ledger keeps for it.
//
// The parent is the row that matters. It carries the reference, the
// annotation that says which postings the caller wrote, and both ends of
// the movement, so a child is only ever a way of finding it - which is
// exactly what a filter on a leg account answers with. Where the filter did
// not return the parent it is fetched by id, once per transfer, which costs
// a round trip on the split shapes and none on the rest.
func (l *Ledger) transfersAmong(ctx context.Context, rows []blnkgo.Transaction) ([]blnkgo.Transaction, error) {
	parents := make(map[string]blnkgo.Transaction, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		id := cmp.Or(row.ParentTransactionID, row.TransactionID)
		if _, seen := parents[id]; !seen {
			order = append(order, id)
			parents[id] = blnkgo.Transaction{}
		}
		if row.TransactionID == id {
			parents[id] = row
		}
	}

	out := make([]blnkgo.Transaction, 0, len(order))
	for _, id := range order {
		txn := parents[id]
		if txn.TransactionID == "" {
			fetched, err := l.transactionByID(ctx, id)
			if err != nil {
				return nil, err
			}
			txn = *fetched
		}
		out = append(out, txn)
	}
	return out, nil
}

// transactionByID reads the transfer a split leg belongs to. A leg that
// names a parent the ledger will not hand back is reported rather than
// worked around: the parent is where the transfer's identity lives, and
// answering from the leg alone would report a movement under a reference
// the domain cannot join on (D7).
func (l *Ledger) transactionByID(ctx context.Context, id string) (*blnkgo.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("blnk: %w", err)
	}
	txn, resp, err := l.client.Transaction.Get(id)
	closeResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("blnk: reading the transfer %q that a split leg belongs to: %w", id, err)
	}
	if txn == nil || txn.TransactionID == "" {
		return nil, fmt.Errorf("blnk: the transfer %q that a split leg belongs to reads back with no transaction id", id)
	}
	return txn, nil
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
func (l *Ledger) transactionsTouching(ctx context.Context, id wallet.LedgerAccountID) ([]blnkgo.Transaction, error) {
	seen := make(map[string]bool)
	var found []blnkgo.Transaction
	for _, field := range []string{fieldSource, fieldDestination} {
		offset := 0
		for page := range historyMaxPages {
			batch, err := l.filterTransactions(ctx, field, string(id), offset)
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
				return nil, fmt.Errorf("blnk: reading the history of account %q: still paging after %d pages", id, historyMaxPages)
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
// as the in-memory and Postgres ledgers answer. The legs on the wire cannot
// answer it: they are netted per account, so a transfer written as 100 out
// and 200 out on one account reaches Blnk as a single leg of 300, and the
// port is explicit that those are different records.
//
// What the ledger stored is still read, and the two are required to agree
// to the minor unit. The document says what was asked for and the legs say
// what was recorded; a transfer where they differ is a transfer this
// package did not send the way it thinks it did, and that is worth stopping
// on rather than reporting either figure as the truth.
//
// A transaction carrying no such document was written behind this package's
// back - a neighbouring system posting on one of these balances - and there
// is nothing to read but what the ledger recorded, so it yields the one
// netted posting the legs describe.
func postingsOfTransfer(txn blnkgo.Transaction, acct account) ([]wallet.Posting, error) {
	written, annotated, err := writtenPostings(txn, acct)
	if err != nil {
		return nil, err
	}
	net, touched, err := netOf(txn, acct)
	if err != nil {
		return nil, err
	}
	switch {
	case annotated && len(written) == 0:
		return nil, nil
	case !annotated && (!touched || net.IsZero()):
		return nil, nil
	}
	if txn.Currency != string(acct.currency) {
		return nil, fmt.Errorf("blnk: transfer %q moves %s on account %q, which holds %s: %w",
			txn.TransactionID, txn.Currency, acct.id, acct.currency, money.ErrCurrencyMismatch)
	}

	posting := func(a money.Amount) wallet.Posting {
		return wallet.Posting{
			Account:     acct.id,
			Amount:      a,
			TransferRef: wallet.TransferRef(txn.TransactionID),
			PostedAt:    txn.CreatedAt,
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
			return nil, fmt.Errorf("blnk: totalling what transfer %q did to account %q: %w", txn.TransactionID, acct.id, err)
		}
		out = append(out, posting(amount))
	}
	if !sum.Equal(net) {
		return nil, fmt.Errorf(
			"blnk: transfer %q says it moved %s on account %q but the ledger recorded %s; the transfer this package sent and the transaction the ledger kept are not the same movement",
			txn.TransactionID, sum, acct.id, net)
	}
	return out, nil
}

// writtenPostings reads the account's own postings out of the identity
// document this package records on the transactions it creates, reporting
// annotated false for a transaction that carries none.
func writtenPostings(txn blnkgo.Transaction, acct account) (written []money.Amount, annotated bool, err error) {
	if _, carried := txn.MetaData[metaKeyTransfer]; !carried {
		return nil, false, nil
	}
	stored, err := recordedOn(&txn)
	if err != nil {
		return nil, false, fmt.Errorf("blnk: the transfer recorded as transaction %q: %w", txn.TransactionID, err)
	}
	for _, p := range stored.Postings {
		if p.Account != string(acct.id) {
			continue
		}
		if p.Currency != string(acct.currency) {
			return nil, false, fmt.Errorf("blnk: transfer %q records %s on account %q, which holds %s: %w",
				txn.TransactionID, p.Currency, acct.id, acct.currency, money.ErrCurrencyMismatch)
		}
		written = append(written, money.Amount{Minor: p.Minor, Currency: acct.currency})
	}
	return written, true, nil
}

// netOf sums what one transaction records against one account, reporting
// touched false when it names the account nowhere - which is how a
// transaction the filter returned but that names the account at neither end
// is dropped rather than trusted.
//
// Both spellings of a side are read: the scalar source and destination, and
// the legs a split transaction carries. A leg's amount is read from
// precise_distribution and never from the SDK's distribution, which is a
// percentage or a decimal number; a leg carrying only that is refused
// rather than parsed through a float (C-6).
func netOf(txn blnkgo.Transaction, acct account) (money.Amount, bool, error) {
	id := string(acct.id)
	touched := false
	net, err := money.Zero(acct.currency)
	if err != nil {
		return money.Amount{}, false, err
	}

	apply := func(magnitude int64, negative bool) error {
		if negative {
			magnitude = -magnitude
		}
		sum, addErr := net.Add(money.Amount{Minor: magnitude, Currency: acct.currency})
		if addErr != nil {
			return fmt.Errorf("blnk: totalling what transfer %q did to account %q: %w", txn.TransactionID, id, addErr)
		}
		net = sum
		touched = true
		return nil
	}

	for _, side := range []struct {
		scalar   string
		legs     []blnkgo.Source
		negative bool
	}{
		{scalar: txn.Source, legs: txn.Sources, negative: true},
		{scalar: txn.Destination, legs: txn.Destinations, negative: false},
	} {
		if side.scalar == id {
			if txn.PreciseAmount == nil {
				// The other spelling of the amount is a float, and a
				// float is never read as money here (C-6). A transfer
				// this package recorded always carries the integer, so
				// one that does not was written behind its back.
				return money.Amount{}, false, fmt.Errorf(
					"blnk: transfer %q on account %q carries no integer amount, and no float is read as money here", txn.TransactionID, id)
			}
			magnitude, err := exactInt64(txn.PreciseAmount)
			if err != nil {
				return money.Amount{}, false, fmt.Errorf("blnk: the amount of transfer %q: %w", txn.TransactionID, err)
			}
			if err := apply(magnitude, side.negative); err != nil {
				return money.Amount{}, false, err
			}
		}
		for _, leg := range side.legs {
			if leg.Identifier != id {
				continue
			}
			magnitude, err := legAmount(leg)
			if err != nil {
				return money.Amount{}, false, fmt.Errorf("blnk: a leg of transfer %q on account %q: %w", txn.TransactionID, id, err)
			}
			if err := apply(magnitude, side.negative); err != nil {
				return money.Amount{}, false, err
			}
		}
	}
	return net, touched, nil
}

// legAmount reads one leg's integer amount. A leg with no
// precise_distribution is refused: the other spelling is a percentage or a
// decimal number, and this package will not turn a float into money.
func legAmount(leg blnkgo.Source) (int64, error) {
	if leg.PreciseDistribution == "" {
		return 0, fmt.Errorf("carries the distribution %q rather than an integer amount, and no float is read as money here", string(leg.Distribution))
	}
	magnitude, ok := new(big.Int).SetString(leg.PreciseDistribution, 10)
	if !ok {
		return 0, fmt.Errorf("carries the amount %q, which is not an integer", leg.PreciseDistribution)
	}
	return exactInt64(magnitude)
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

// isNotFound reports whether the thing asked for does not exist - because
// the ledger said so, or because what it answered with belongs to another
// ledger and so is not there to be found from here.
func isNotFound(err error) bool {
	var scoped notFoundError
	if errors.As(err, &scoped) {
		return true
	}
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
// The status alone is not enough. Blnk has no unique index on the reference
// and refuses a duplicate in application code (spike S2), so which status
// that code chooses is the server's business; a refusal naming the
// duplicate in its structured code is a duplicate whatever status carried
// it. Reading only the status would hand the caller that lost the race a
// hard error instead of the winner's reference.
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

// cachedAccount answers with an account id this process has already
// resolved.
func (l *Ledger) cachedAccount(indicator string) (wallet.LedgerAccountID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.accounts[indicator]
	return id, ok
}

// cacheAccount remembers a resolved account id. The mapping is immutable -
// Blnk deletes no balance and renames no indicator - so an entry can never
// go stale.
func (l *Ledger) cacheAccount(indicator string, id wallet.LedgerAccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accounts[indicator] = id
}

// lockAccount takes the lock guarding one account name and returns the
// release. One lock per name, so two goroutines ensuring one account cannot
// both create it while two ensuring different accounts never wait on each
// other.
func (l *Ledger) lockAccount(indicator string) func() {
	l.mu.Lock()
	lock, ok := l.ensuring[indicator]
	if !ok {
		lock = new(sync.Mutex)
		l.ensuring[indicator] = lock
	}
	l.mu.Unlock()

	lock.Lock()
	return lock.Unlock
}
