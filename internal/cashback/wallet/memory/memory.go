// Package memory implements the [wallet.Ledger] port entirely in process:
// accounts, transfers and postings held in Go maps behind one mutex. It
// exists for the two callers the port contract names for it - unit tests,
// which need a working double-entry ledger with no network attached, and
// ledger-less local development, where the memory driver completes a
// configuration on a machine that cannot run the production ledger.
//
// It is also the reference the shared conformance suite measures the other
// implementations against, which sets its priorities: exactness over
// cleverness, everywhere. One lock serialises every operation, nothing is
// cached, and Balance literally sums the account's postings at the moment
// of the call (D7) - the slowest correct answer, chosen because a
// reference that cuts corners moves the target for every implementation
// behind it.
//
// Nothing survives the process, deliberately: a unit test wants a ledger
// born empty, and local development wants one that resets with a restart.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The port is satisfied by *Ledger or not at all; this line is where the
// compiler says so.
var _ wallet.Ledger = (*Ledger)(nil)

// Ledger is the in-memory implementation of [wallet.Ledger]. Build one
// with [New]; the zero value carries no maps and is not usable.
//
// One mutex serialises every operation. That is deliberate simplicity,
// not an oversight: this ledger's job is to be exactly right under the
// conformance suite's concurrent hammering, and a single lock makes the
// port's racing promises - one winner per contested idempotency key, one
// id per account however many callers ensure it - true by construction,
// with nothing finer-grained to get wrong. Throughput belongs to the
// production ledger.
type Ledger struct {
	// now supplies PostedAt. It is a field rather than a call to
	// time.Now so tests can hand in a clock of their own ([WithClock])
	// and draw windows around postings exactly.
	now func() time.Time

	// mu guards everything below it.
	mu sync.Mutex
	// accounts holds every account EnsureAccount has issued, keyed by
	// the id it issued.
	accounts map[wallet.LedgerAccountID]*account
	// transfers holds one record per idempotency key ever accepted,
	// keyed byte for byte: the port imposes no format on keys and this
	// ledger reads nothing into them.
	transfers map[string]recorded
	// sequence numbers the minted transfer references.
	sequence uint64
}

// account is one ledger account: the currency it is denominated in and
// every posting recorded on it, in the order they were recorded.
type account struct {
	currency money.Currency
	postings []wallet.Posting
}

// recorded is what one idempotency key remembers: the reference the first
// Post under it minted, and the transfer it recorded - postings held in
// canonical form so a replay can be told apart from a collision without
// caring what order a retry happened to assemble its slice in.
type recorded struct {
	ref       wallet.TransferRef
	postings  []wallet.Posting
	reference string
	metadata  map[string]string
}

// matches reports whether transfer is a true replay of what the key
// recorded. It implements the definition documented on [Ledger.Post]:
// postings as a multiset of (account, amount) movements, reference and
// metadata by content.
func (r recorded) matches(transfer wallet.Transfer) bool {
	if r.reference != transfer.Reference {
		return false
	}
	// maps.Equal compares content, so nil and empty metadata - both of
	// which annotate nothing - count as the same annotations.
	if !maps.Equal(r.metadata, transfer.Metadata) {
		return false
	}
	incoming := canonical(transfer.Postings)
	if len(incoming) != len(r.postings) {
		return false
	}
	for i := range incoming {
		if incoming[i].Account != r.postings[i].Account || !incoming[i].Amount.Equal(r.postings[i].Amount) {
			return false
		}
	}
	return true
}

// canonical strips postings to what they mean - the account and the
// signed amount; provenance is required blank on input by the port's
// Validate - and sorts them into one fixed order, so two spellings of one
// movement of money compare equal and two different movements compare
// different, deterministically.
func canonical(postings []wallet.Posting) []wallet.Posting {
	out := make([]wallet.Posting, len(postings))
	for i, p := range postings {
		out[i] = wallet.Posting{Account: p.Account, Amount: p.Amount}
	}
	slices.SortFunc(out, func(a, b wallet.Posting) int {
		if c := cmp.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Amount.Currency, b.Amount.Currency); c != 0 {
			return c
		}
		return cmp.Compare(a.Amount.Minor, b.Amount.Minor)
	})
	return out
}

// Option configures the ledger [New] returns.
type Option func(*Ledger)

// WithClock replaces the source PostedAt is read from, so a test can hand
// out instants of its own choosing and get reproducible windows. The
// ledger calls the clock under its own lock, once per posted transfer, so
// a stateful clock needs no locking of its own. A nil clock keeps the
// default, [time.Now]: silently freezing time on a nil would make every
// posting simultaneous, which is exactly the kind of accident an option
// should not be able to encode.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// New returns an empty ledger: no accounts, no transfers, time read from
// [time.Now] unless [WithClock] replaces it.
func New(opts ...Option) *Ledger {
	l := &Ledger{
		now:       time.Now,
		accounts:  make(map[wallet.LedgerAccountID]*account),
		transfers: make(map[string]recorded),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// EnsureAccount resolves ref in currency to this ledger's identity for
// the account, creating it empty the first time the pair is named. The id
// is derived deterministically from (ref, currency), so idempotency needs
// no bookkeeping: the same pair spells the same id on every call, however
// many callers race, and the map entry only ever goes from absent to
// present under the lock. An unusable ref is refused wrapping
// [wallet.ErrInvalidAccountRef], a malformed currency wrapping
// [money.ErrInvalidCurrency], both before any state is touched.
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
		return "", fmt.Errorf("memory: %w", err)
	}

	id := accountID(ref, currency)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.accounts[id]; !ok {
		l.accounts[id] = &account{currency: currency}
	}
	return id, nil
}

// accountID derives this ledger's identity for (ref, currency). The
// derivation is injective - member ids are fixed-format UUIDs, house
// names are quoted so no name can smuggle a separator, and the two shapes
// carry distinct prefixes - so distinct accounts can never share an id.
// Every caller treats the result as opaque (the port says so); the
// readable spelling is for a human staring at a test failure, not for
// parsing.
func accountID(ref wallet.AccountRef, currency money.Currency) wallet.LedgerAccountID {
	if memberID, stage, ok := ref.Member(); ok {
		return wallet.LedgerAccountID("member/" + memberID.String() + "/" + stage.String() + "/" + string(currency))
	}
	// Validate passed in EnsureAccount, so a ref that is not a member
	// reference is a house reference.
	name, _ := ref.House()
	return wallet.LedgerAccountID("house/" + strconv.Quote(name) + "/" + string(currency))
}

// Post records transfer atomically: every posting or none.
// [wallet.Transfer.Validate] runs first, before the lock is taken, so a
// transfer that would create or destroy money is refused before this
// ledger does anything at all (C-1, checked twice by design).
//
// Idempotency is decided by the transfer's key, compared byte for byte.
// The first Post under a key records the transfer and mints its
// reference; a replay - the same key carrying the same transfer - returns
// that original reference and records nothing, and the same key carrying
// a different transfer is refused wrapping
// [wallet.ErrIdempotencyConflict].
//
// "The same transfer" is semantic, not positional. The postings must be
// the same multiset of (account, amount) movements: the order a caller
// happened to assemble its slice in is representation, not meaning, and
// refusing a reordered rebuild of the same movement would break exactly
// the retry the key exists to make safe. Reference and Metadata must
// match by content, because a caller annotating one key two ways believes
// it is recording two different facts - the bug the conflict error exists
// to surface. Nil and empty metadata both annotate nothing and compare
// equal.
//
// Every posting must name an account EnsureAccount issued, in that
// account's own currency: an unissued id is refused wrapping
// [wallet.ErrUnknownAccount], and a posting denominated in a currency the
// account does not hold is refused wrapping [money.ErrCurrencyMismatch] -
// an account is one currency by definition (C-6), so money of another
// kind has no meaning on it. Both checks run over the whole transfer
// before anything is applied, so a refused transfer changes no balance
// and consumes no key.
func (l *Ledger) Post(ctx context.Context, transfer wallet.Transfer) (wallet.TransferRef, error) {
	if err := transfer.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("memory: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if prior, seen := l.transfers[transfer.IdempotencyKey]; seen {
		if prior.matches(transfer) {
			return prior.ref, nil
		}
		return "", fmt.Errorf("%w: key %q", wallet.ErrIdempotencyConflict, transfer.IdempotencyKey)
	}

	// All-or-nothing: the whole transfer is judged before any posting is
	// applied, so posting 3 failing cannot leave postings 1 and 2 behind.
	for i, p := range transfer.Postings {
		acct, ok := l.accounts[p.Account]
		if !ok {
			return "", fmt.Errorf("%w: posting %d of %d names %q", wallet.ErrUnknownAccount, i+1, len(transfer.Postings), p.Account)
		}
		if p.Amount.Currency != acct.currency {
			return "", fmt.Errorf("memory: posting %d of %d: %w: account %q holds %s, the posting moves %s",
				i+1, len(transfer.Postings), money.ErrCurrencyMismatch, p.Account, acct.currency, p.Amount.Currency)
		}
	}

	// The reference is minted from a counter rather than randomness so a
	// failing test prints references a human can line up with the posts
	// that made them. It is opaque to every caller, like the account ids.
	l.sequence++
	ref := wallet.TransferRef("transfer-" + strconv.FormatUint(l.sequence, 10))

	// One instant per transfer, read under the lock: every posting of a
	// transfer is recorded at the same moment, and moments advance in the
	// order transfers actually recorded - which is what makes windows
	// drawn over an injected test clock exact.
	at := l.now()
	for _, p := range transfer.Postings {
		acct := l.accounts[p.Account]
		acct.postings = append(acct.postings, wallet.Posting{
			Account:     p.Account,
			Amount:      p.Amount,
			TransferRef: ref,
			PostedAt:    at,
		})
	}
	l.transfers[transfer.IdempotencyKey] = recorded{
		ref:      ref,
		postings: canonical(transfer.Postings),
		// The caller keeps its transfer and may mutate the map after
		// Post returns; replay detection must compare against what was
		// recorded, not against whatever the caller's map has become.
		reference: transfer.Reference,
		metadata:  maps.Clone(transfer.Metadata),
	}
	return ref, nil
}

// Balance sums every posting on the account at the moment of the call,
// under the lock - there is no stored figure that could drift from the
// postings (D7). The currency argument is the caller's assertion of what
// the account holds (C-6): an account of a different currency is refused
// wrapping [money.ErrCurrencyMismatch], an id this ledger never issued
// wrapping [wallet.ErrUnknownAccount]. An account with no postings is
// zero in its currency. A sum that leaves the int64 range is refused
// wrapping [money.ErrOverflow] rather than wrapped around: a balance that
// reads as its own negation is worse than an error.
func (l *Ledger) Balance(ctx context.Context, account wallet.LedgerAccountID, currency money.Currency) (money.Amount, error) {
	if err := ctx.Err(); err != nil {
		return money.Amount{}, fmt.Errorf("memory: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	acct, ok := l.accounts[account]
	if !ok {
		return money.Amount{}, fmt.Errorf("%w: %q", wallet.ErrUnknownAccount, account)
	}
	if currency != acct.currency {
		return money.Amount{}, fmt.Errorf("memory: account %q: %w: holds %s, asked for %s",
			account, money.ErrCurrencyMismatch, acct.currency, currency)
	}

	total, err := money.Zero(acct.currency)
	if err != nil {
		// Unreachable while EnsureAccount validates every currency it
		// stores - and returned rather than swallowed in case that ever
		// stops being true.
		return money.Amount{}, err
	}
	for _, p := range acct.postings {
		// Add can only fail on overflow here: Post refuses a posting
		// whose currency differs from the account's, so every operand
		// matches by construction.
		total, err = total.Add(p.Amount)
		if err != nil {
			return money.Amount{}, err
		}
	}
	return total, nil
}

// History streams the account's postings whose PostedAt falls inside
// window ([wallet.Window.Contains]), ordered by ascending PostedAt, each
// carrying the reference and instant the ledger recorded. Swapped bounds
// are refused wrapping [wallet.ErrInvalidWindow], an id this ledger never
// issued wrapping [wallet.ErrUnknownAccount] - both immediately, before
// the iterator exists.
//
// The matching postings are copied out under the lock before the iterator
// is returned, and iteration walks that snapshot with no lock held. A
// consumer is therefore free to Post - to this same account included -
// from inside its own loop without deadlocking, and a Post racing the
// iteration can neither tear it nor leak into it: the stream is the
// history as it stood when History was called. A context cancelled
// mid-stream ends the iteration with the cancellation as the yielded
// error, the way an implementation that really streams would fail.
func (l *Ledger) History(ctx context.Context, account wallet.LedgerAccountID, window wallet.Window) (iter.Seq2[wallet.Posting, error], error) {
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}

	matched, err := l.snapshot(account, window)
	if err != nil {
		return nil, err
	}

	// Postings are appended in recording order and the clock normally
	// moves forward with them, but the contract is ascending PostedAt,
	// not "whatever the clock did": a wall clock stepped backwards, or an
	// injected test clock replaying one instant, must not leak an
	// unordered history. The sort is stable so postings sharing an
	// instant keep the order they were recorded in.
	slices.SortStableFunc(matched, func(a, b wallet.Posting) int {
		return a.PostedAt.Compare(b.PostedAt)
	})

	return func(yield func(wallet.Posting, error) bool) {
		for _, p := range matched {
			if err := ctx.Err(); err != nil {
				yield(wallet.Posting{}, fmt.Errorf("memory: %w", err))
				return
			}
			if !yield(p, nil) {
				return
			}
		}
	}, nil
}

// snapshot copies the account's postings inside window out from under the
// lock. The copy is what buys History its concurrency promise: the lock
// is held for the copy and released before the first yield, so iteration
// can never deadlock against a Post and a Post can never tear an
// iteration.
func (l *Ledger) snapshot(account wallet.LedgerAccountID, window wallet.Window) ([]wallet.Posting, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	acct, ok := l.accounts[account]
	if !ok {
		return nil, fmt.Errorf("%w: %q", wallet.ErrUnknownAccount, account)
	}
	matched := make([]wallet.Posting, 0, len(acct.postings))
	for _, p := range acct.postings {
		if window.Contains(p.PostedAt) {
			matched = append(matched, p)
		}
	}
	return matched, nil
}
