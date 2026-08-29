// Package wallet owns the cashback money path: which ledger accounts exist,
// how money moves between them, and what a balance means. Its centrepiece is
// the [Ledger] port - the only way any code in this repository touches money
// (ADR-0002).
//
// The port is consumer-defined. This package declares what it needs from a
// double-entry ledger; adapter packages beneath it implement the declaration
// against the production ledger, an in-memory ledger for unit tests and
// ledger-less local development, and a Postgres ledger kept as the documented
// exit route. The composition root in cmd wires one in. No vendor type
// crosses this boundary in either direction - the architecture test fails
// the build if one does - which is what keeps the substrate swappable inside
// the constitution's five-day replaceability budget.
//
// Two constitution invariants are load-bearing here and are enforced in this
// file, before any adapter sees a transfer:
//
//   - C-1: every transfer's postings sum to zero per currency.
//     [Transfer.Validate] is that check, and every implementation calls it
//     before any I/O, so C-1 is checked twice by design - once in this port
//     and once again by the ledger itself. A transfer that would create or
//     destroy money is refused before it can leave the process.
//   - C-6: every amount is an int64 of minor units carrying an explicit
//     ISO-4217 currency. The port speaks [money.Amount] and nothing else;
//     there is no float, no decimal string and no implicit currency in any
//     signature below.
//
// Balances are never stored by this package. A member-facing total is the
// sum of ledger postings at the moment it is asked for (D7), which is why
// the port has a History and a Balance and no SetBalance.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Sentinel errors. Every failure this package reports wraps exactly one of
// them, and the ledger conformance suite asserts on them with errors.Is, so
// all three implementations refuse the same mistake with the same error.
// Failures of the money type itself - a malformed currency, an overflowing
// sum - wrap that package's own sentinels ([money.ErrInvalidCurrency],
// [money.ErrOverflow], [money.ErrCurrencyMismatch]) rather than duplicates
// of them: one money representation means one set of money errors.
var (
	// ErrMissingIdempotencyKey reports a transfer with no idempotency key.
	// A Post without one is a programming error, not a request: the key is
	// what makes a retry safe, so a transfer that lacks one has no defined
	// behaviour under exactly the failure it exists to survive.
	ErrMissingIdempotencyKey = errors.New("wallet: transfer carries no idempotency key")
	// ErrTooFewPostings reports a transfer with fewer than two postings.
	// Double entry means every movement has a source and a destination; one
	// posting could only sum to zero by moving nothing, which is rejected
	// too.
	ErrTooFewPostings = errors.New("wallet: a transfer needs at least two postings")
	// ErrMissingAccount reports a posting that names no ledger account. It
	// is distinct from [ErrUnknownAccount]: a blank id was never issued by
	// EnsureAccount, so the transfer is malformed rather than mistargeted.
	ErrMissingAccount = errors.New("wallet: posting names no ledger account")
	// ErrZeroPosting reports a posting whose amount is zero. A posting that
	// moves nothing records nothing, and the likeliest way one appears is a
	// caller that forgot to set an amount - which is worth seeing, not
	// storing.
	ErrZeroPosting = errors.New("wallet: posting moves no money")
	// ErrRecycledPosting reports an input posting that already carries
	// ledger provenance (a transfer reference or a posted-at time). Those
	// fields are the ledger's to write, so their presence means the posting
	// was copied out of History rather than built for this transfer - the
	// classic shape of a reversal assembled by copy-paste, which must flip
	// its amounts and shed its provenance instead.
	ErrRecycledPosting = errors.New("wallet: posting already carries ledger provenance")
	// ErrUnbalanced reports a transfer with exactly one currency whose
	// postings do not sum to zero (C-1). Posting it would create or destroy
	// money, so it is refused before any I/O.
	ErrUnbalanced = errors.New("wallet: postings do not sum to zero")
	// ErrMixedCurrency reports a transfer in which two or more currencies
	// each fail to sum to zero. Only netting one currency against another
	// could have made such a transfer look balanced to its author, and
	// currencies never net: each balances independently, with no exchange
	// rate anywhere in this port. It is distinct from [ErrUnbalanced] so
	// the error names the mistake actually made - thinking 100 EUR can
	// offset 100 GBP - rather than a generic imbalance.
	ErrMixedCurrency = errors.New("wallet: currencies cannot net against each other")
	// ErrInvalidAccountRef reports an [AccountRef] that does not name an
	// account: the zero value, a member reference without a member or a
	// stage, or a house reference without a name.
	ErrInvalidAccountRef = errors.New("wallet: account reference names no account")
	// ErrInvalidWindow reports a [Window] that ends before it starts. No
	// posting can fall inside it, and a caller asking for one has its
	// bounds swapped - worth seeing, not an empty answer.
	ErrInvalidWindow = errors.New("wallet: window ends before it starts")

	// ErrUnknownAccount reports an operation against a ledger account id
	// that EnsureAccount never issued. It is returned by implementations,
	// not by validation here - only the ledger knows which ids exist - and
	// is defined in the port so every implementation refuses an unknown
	// account identically and the conformance suite can assert on it.
	ErrUnknownAccount = errors.New("wallet: ledger account does not exist")
	// ErrIdempotencyConflict reports a Post replaying an idempotency key
	// with a different transfer than the one that first used it. A true
	// replay returns the original reference and creates nothing; the same
	// key over different postings is two transfers fighting for one
	// identity, and honouring either silently would hide the bug that
	// minted the collision. Returned by implementations, defined here for
	// the same reason as [ErrUnknownAccount].
	ErrIdempotencyConflict = errors.New("wallet: idempotency key was already used by a different transfer")
)

// LedgerAccountID identifies one account inside whichever ledger is wired
// in. It is issued by EnsureAccount and opaque to every caller: nothing in
// this package or above it parses one, so the ledger behind the port is free
// to use whatever identity scheme it has, and swapping ledgers cannot strand
// a caller that had learned to read the old one's ids.
type LedgerAccountID string

// TransferRef identifies one posted transfer inside the ledger. It is what
// the domain rows store - entry_transition.ledger_transfer_ref and
// ledger_link.ledger_transfer_ref are this value - so it is text, opaque,
// and never blank: the seam between the domain schema and the ledger is a
// reference, not a foreign key, and an empty reference would be a seam that
// joins to nothing.
type TransferRef string

// Stage names which claim bucket of a member's money an account holds. It
// exists because the wallet shows a member distinct totals - pending,
// confirmed and so on - and D7 says every one of them derives from ledger
// postings, never from a stored number. Balance answers per account, so
// each bucket must be its own ledger account: a member's money is a set of
// (member, stage, currency) accounts, and an entry-state transition is a
// transfer between two of them. Collapsing the buckets into one account
// would make those transfers net to zero on a single account and record
// nothing, leaving the buckets to be tracked outside the ledger - exactly
// the second source of truth D7 forbids.
//
// The stages mirror the non-terminal entry states (FR-042). The terminal
// states have no bucket because they are not claims: money paid out has
// left the member entirely, and money reversed has gone back where it came
// from. Both remain visible as History, which is where a "paid out" total
// comes from.
//
// The zero value is deliberately not a stage, so a caller that forgets to
// choose one gets an error rather than a default bucket nobody decided on.
type Stage uint8

const (
	// stageUnspecified is the zero value of Stage and is deliberately not
	// a stage. Its existence is what makes a forgotten argument an error.
	stageUnspecified Stage = iota
	// StageHeld holds money credited under a hold rule (US7): real, but
	// not yet counted toward anything member-facing.
	StageHeld
	// StagePending holds money the network has reported but not yet
	// confirmed. Visible to the member, never spendable.
	StagePending
	// StageConfirmed holds money the network has confirmed: the only
	// bucket that counts toward the withdrawal threshold (FR-050).
	StageConfirmed
	// StageReserved holds money a withdrawal request has claimed (D9).
	// Reserving at request time, in the ledger, is what closes the
	// double-spend window between request and approval.
	StageReserved
)

// Valid reports whether s is one of the named stages. The zero value is not.
func (s Stage) Valid() bool {
	switch s {
	case StageHeld, StagePending, StageConfirmed, StageReserved:
		return true
	default:
		return false
	}
}

// String names the stage, so an error about one says which it was rather
// than printing a number.
func (s Stage) String() string {
	switch s {
	case StageHeld:
		return "held"
	case StagePending:
		return "pending"
	case StageConfirmed:
		return "confirmed"
	case StageReserved:
		return "reserved"
	case stageUnspecified:
		return "unspecified"
	default:
		return "unknown stage " + strconv.FormatUint(uint64(s), 10)
	}
}

// accountKind separates the two shapes an AccountRef can take. It is
// unexported because the set of kinds is this package's decision: a new
// kind is a new constructor here, never a magic value invented by a caller.
type accountKind uint8

const (
	// kindUnspecified is the zero value of accountKind, carried by the
	// zero AccountRef, which names no account and validates as such.
	kindUnspecified accountKind = iota
	// kindMember is a member's claim bucket: (member, stage, currency).
	kindMember
	// kindHouse is a named operational account owned by the business.
	kindHouse
)

// AccountRef names an account the caller wants to exist, in the wallet's
// own vocabulary; EnsureAccount turns one into the ledger's identity for
// it. A ref is currency-free on purpose - EnsureAccount takes the currency
// beside it, so one ref names a family of accounts, one per currency the
// deployment trades in, and no currency is ever implicit (C-6).
//
// There are exactly two shapes, and the type makes them unmistakable
// rather than encoding them in a string a typo could blur:
//
//   - [MemberAccount]: one claim bucket of one member's money.
//   - [HouseAccount]: a named account the business owns - the rounding
//     remainder that keeps every split summing to zero (D6), the clawback
//     loss absorbed after a payout (Q3). House account names come from
//     configuration, never from literals in domain code, so the
//     constructor takes the name rather than defining any.
//
// The fields are unexported and the zero value names no account, so the
// only way to hold a usable ref is through a constructor - a ref cannot be
// half-built, and member and house identities cannot be confused however
// alike their spellings are.
type AccountRef struct {
	kind   accountKind
	member uuid.UUID
	stage  Stage
	house  string
}

// MemberAccount names one claim bucket of one member's money: the stage
// account of the member with this account id. The member is identified by
// id rather than by anything human-readable because the ledger must never
// hold member identity beyond what routing money requires.
func MemberAccount(memberID uuid.UUID, stage Stage) AccountRef {
	return AccountRef{kind: kindMember, member: memberID, stage: stage}
}

// HouseAccount names an operational account the business owns, by the name
// configuration gave it. The name is taken as-is and validated, never
// normalised: two names differing in case or padding are two accounts, and
// a configuration typo should fail loudly at EnsureAccount rather than
// quietly open a second rounding-remainder account beside the real one.
func HouseAccount(name string) AccountRef {
	return AccountRef{kind: kindHouse, house: name}
}

// Member returns the member id and stage a member reference names,
// reporting ok false for anything that is not a member reference.
// Implementations use it to derive their own storage identity for the
// account; how they spell that identity is theirs to decide.
func (r AccountRef) Member() (memberID uuid.UUID, stage Stage, ok bool) {
	if r.kind != kindMember {
		return uuid.UUID{}, stageUnspecified, false
	}
	return r.member, r.stage, true
}

// House returns the configured name a house reference carries, reporting
// ok false for anything that is not a house reference.
func (r AccountRef) House() (name string, ok bool) {
	if r.kind != kindHouse {
		return "", false
	}
	return r.house, true
}

// Validate reports whether r names an account, returning an error wrapping
// [ErrInvalidAccountRef] when it does not. Implementations call it at the
// top of EnsureAccount, so a zero ref, a nil member id, an unspecified
// stage or a blank house name is refused before any I/O rather than
// becoming a real account named after a mistake.
func (r AccountRef) Validate() error {
	switch r.kind {
	case kindMember:
		if r.member == uuid.Nil {
			return fmt.Errorf("%w: member reference carries the nil member id", ErrInvalidAccountRef)
		}
		if !r.stage.Valid() {
			return fmt.Errorf("%w: member reference carries stage %q", ErrInvalidAccountRef, r.stage)
		}
		return nil
	case kindHouse:
		// Rejected rather than trimmed, for the same reason money rejects
		// lowercase currency codes: padding means some caller or some
		// configuration assembled the name by hand, and an account that
		// exists under " rounding" beside "rounding" is a balance split
		// invisibly in two.
		if r.house == "" || r.house != strings.TrimSpace(r.house) {
			return fmt.Errorf("%w: house reference carries name %q", ErrInvalidAccountRef, r.house)
		}
		return nil
	case kindUnspecified:
		return fmt.Errorf("%w: zero reference", ErrInvalidAccountRef)
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrInvalidAccountRef, uint8(r.kind))
	}
}

// String renders the reference for logs, errors and test failures. It is
// diagnostic output only, never a storage key: implementations derive their
// identities from [AccountRef.Member] and [AccountRef.House], so a change
// to this rendering can never strand an account.
func (r AccountRef) String() string {
	switch r.kind {
	case kindMember:
		return "member " + r.member.String() + " (" + r.stage.String() + ")"
	case kindHouse:
		return "house " + strconv.Quote(r.house)
	default:
		return "unspecified account"
	}
}

// Posting is one movement of money on one account: the double-entry atom.
// A transfer is a set of them summing to zero per currency, and a balance
// is the sum of every posting on an account.
//
// The representation is a signed amount, not a direction flag beside a
// magnitude: a positive amount raises the account's balance and a negative
// amount lowers it, so within a transfer, value flows from the negative
// postings to the positive ones. Signed is chosen deliberately, twice
// over. First, it makes the C-1 check arithmetic on the one money type -
// the postings of each currency [money.Sum] to zero, or the transfer is
// refused - with no translation step in which a check could go wrong.
// Second, a direction flag can disagree with the sign of its own amount (a
// "credit" of -500), and a representation in which the illegal state
// cannot be written beats one in which it must be checked for. The words
// debit and credit are avoided entirely: which of them raises a balance
// depends on which side of the books an account sits, and a sign carries
// no such ambiguity.
type Posting struct {
	// Account is the ledger account the amount moves on, as issued by
	// EnsureAccount.
	Account LedgerAccountID
	// Amount is the signed movement in minor units of an explicit
	// currency (C-6). Positive raises the account's balance, negative
	// lowers it, zero is refused: a posting that moves nothing records
	// nothing.
	Amount money.Amount
	// TransferRef is the transfer this posting belongs to. It is the
	// ledger's to write: set on every posting History yields, and
	// required blank on input to Post - the reference does not exist
	// until the ledger mints it, so an input posting carrying one was
	// copied from history, and [Transfer.Validate] refuses it.
	TransferRef TransferRef
	// PostedAt is when the ledger recorded the posting, under the same
	// rule as TransferRef: set by History, required zero on input to
	// Post. It is the instant Window bounds select on.
	PostedAt time.Time
}

// Transfer is one atomic movement of money: at least two postings summing
// to zero per currency, under an idempotency key that makes retrying it
// safe. It is the only argument Post takes, so everything the ledger needs
// to record the movement - and everything an auditor needs to read it back
// - travels in one value.
type Transfer struct {
	// IdempotencyKey makes Post safe to retry: replaying a key returns
	// the original transfer's reference and creates nothing. Required,
	// and compared byte for byte - the port imposes no format, but two
	// keys that differ at all are two transfers. Callers derive it
	// deterministically from the domain fact that caused the transfer
	// (D8): a key that cannot be re-derived is a key a retry cannot
	// reuse, which is how duplicates actually happen.
	IdempotencyKey string
	// Postings are the movements, at least two, summing to zero within
	// every currency they touch (C-1). A transfer may span currencies -
	// each balances independently - but no posting nets against one in
	// another currency, ever.
	Postings []Posting
	// Reference names the domain record that caused this transfer - an
	// entry transition, a withdrawal request - for whoever reads the
	// ledger directly. It is a courtesy pointer, not the seam: the
	// authoritative join runs the other way, from the domain rows that
	// store this transfer's TransferRef (D7), which is why validation
	// does not require it.
	Reference string
	// Metadata carries free-form annotations stored with the transfer.
	// Like Reference it is for reading the ledger directly; the port
	// offers no read-back, because the domain's own rows are the
	// authority on why money moved.
	Metadata map[string]string
}

// Validate is the port's half of C-1, together with every structural rule
// a transfer must satisfy, checked before any I/O. Implementations call it
// first in Post, so the check runs twice by design - here, and again
// inside the ledger - and a transfer that would create or destroy money is
// refused before it can leave the process.
//
// The rules, in the order they are checked:
//
//   - the idempotency key is present ([ErrMissingIdempotencyKey]);
//   - there are at least two postings ([ErrTooFewPostings]);
//   - every posting names an account ([ErrMissingAccount]), carries no
//     ledger provenance ([ErrRecycledPosting]), has a well-formed currency
//     (wrapping [money.ErrInvalidCurrency]) and moves a non-zero amount
//     ([ErrZeroPosting]);
//   - within every currency the postings sum to exactly zero. One
//     currency off is [ErrUnbalanced]; two or more off is
//     [ErrMixedCurrency], because only netting currencies against each
//     other could have made such a transfer look balanced.
//
// The first hole found is the one reported, with the offending posting's
// position in the message, so a refused transfer says which of its parts
// to look at.
func (t Transfer) Validate() error {
	if strings.TrimSpace(t.IdempotencyKey) == "" {
		return ErrMissingIdempotencyKey
	}
	if len(t.Postings) < 2 {
		return fmt.Errorf("%w: got %d", ErrTooFewPostings, len(t.Postings))
	}

	// One pass does both the per-posting checks and the per-currency
	// netting, in posting order, so the currencies report in the order
	// the transfer introduced them and the same broken transfer is
	// always refused with the same message.
	nets := make(map[money.Currency]money.Amount, 1)
	order := make([]money.Currency, 0, 1)
	for i, p := range t.Postings {
		position := fmt.Sprintf("posting %d of %d", i+1, len(t.Postings))
		if strings.TrimSpace(string(p.Account)) == "" {
			return fmt.Errorf("%w: %s", ErrMissingAccount, position)
		}
		if p.TransferRef != "" || !p.PostedAt.IsZero() {
			return fmt.Errorf("%w: %s was read from history, not built for this transfer", ErrRecycledPosting, position)
		}
		if err := p.Amount.Validate(); err != nil {
			return fmt.Errorf("wallet: %s: %w", position, err)
		}
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: %s", ErrZeroPosting, position)
		}

		total, seen := nets[p.Amount.Currency]
		if !seen {
			order = append(order, p.Amount.Currency)
			nets[p.Amount.Currency] = p.Amount
			continue
		}
		// Add can only fail on overflow here - the currencies match by
		// construction of the map key - and an overflowing net is
		// refused outright: a transfer this port cannot even sum is not
		// one any ledger should be asked to record.
		sum, err := total.Add(p.Amount)
		if err != nil {
			return fmt.Errorf("wallet: netting %s postings: %w", p.Amount.Currency, err)
		}
		nets[p.Amount.Currency] = sum
	}

	var off []string
	for _, currency := range order {
		if !nets[currency].IsZero() {
			off = append(off, fmt.Sprintf("%s nets to %s", currency, nets[currency]))
		}
	}
	switch len(off) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%w: %s", ErrUnbalanced, off[0])
	default:
		return fmt.Errorf("%w: %s; each currency must sum to zero on its own", ErrMixedCurrency, strings.Join(off, ", "))
	}
}

// Window bounds a History read by when the ledger recorded each posting:
// half-open, taking every posting with From <= PostedAt < To. Half-open is
// what lets adjacent windows partition a history with no posting counted
// twice and none falling in a seam - the property a resumable reader
// advances a watermark on.
//
// The zero value is the whole of history, and each bound widens away
// independently: a zero From starts at the first posting, a zero To never
// ends. That makes "everything", "everything since the watermark" and
// "one reconciliation period" the same type with no flags.
type Window struct {
	// From is the inclusive lower bound; zero means from the first
	// posting.
	From time.Time
	// To is the exclusive upper bound; zero means no upper bound.
	To time.Time
}

// Validate reports whether the window can contain anything, returning an
// error wrapping [ErrInvalidWindow] when To lies before From - swapped
// bounds, which History refuses rather than answering with an emptiness
// the caller would mistake for an empty history. An empty-but-ordered
// window (From equal to To) is legal: it is a genuine question with a
// genuinely empty answer.
func (w Window) Validate() error {
	if !w.From.IsZero() && !w.To.IsZero() && w.To.Before(w.From) {
		return fmt.Errorf("%w: from %s, to %s", ErrInvalidWindow, w.From.Format(time.RFC3339Nano), w.To.Format(time.RFC3339Nano))
	}
	return nil
}

// Contains reports whether an instant falls inside the window: at or after
// From, strictly before To, with a zero bound imposing nothing. It is the
// single definition of membership, shared so that every implementation
// filters history identically and the conformance suite can hold them to
// it.
func (w Window) Contains(at time.Time) bool {
	if !w.From.IsZero() && at.Before(w.From) {
		return false
	}
	if !w.To.IsZero() && !at.Before(w.To) {
		return false
	}
	return true
}

// Ledger is the port every implementation satisfies and the only way any
// code touches money (ADR-0002). It is small on purpose: accounts exist,
// money moves between them atomically, and what has happened can be read
// back - everything else the cashback domain does with money is built from
// those three abilities, in this package, identically over every
// implementation.
//
// Contract, held by all implementations and asserted by the shared
// conformance suite:
//
//  1. Post is idempotent on Transfer.IdempotencyKey: replaying a key
//     returns the original TransferRef and creates nothing, and of two
//     concurrent posts of one key exactly one records, both learning the
//     same reference.
//  2. Post rejects a transfer whose postings do not sum to zero per
//     currency, before any I/O, by calling [Transfer.Validate] first
//     (C-1, checked twice by design).
//  3. Currencies never net against each other: each balances
//     independently within a transfer.
//  4. Every amount is an int64 of minor units with an explicit ISO-4217
//     currency - [money.Amount], nothing else, anywhere (C-6).
//  5. Balance is the ledger's own sum over the account's postings at the
//     moment of the call, never a stored figure that could drift from
//     them; member-facing totals are computed by the wallet from
//     postings, and the two are cross-checked to the minor unit.
type Ledger interface {
	// EnsureAccount resolves ref in currency to the ledger's identity
	// for that account, creating it empty if this is the first time it
	// is named. It is idempotent and safe under concurrency: the same
	// (ref, currency) pair resolves to the same id on every call,
	// however many callers race, so callers never ask "does it exist"
	// - they ensure and use.
	//
	// One ref names one account per currency (C-6): a member holding
	// two currencies holds two confirmed accounts, and no operation
	// ever spans them implicitly. An unusable ref is refused wrapping
	// [ErrInvalidAccountRef]; a malformed currency wrapping
	// [money.ErrInvalidCurrency].
	EnsureAccount(ctx context.Context, ref AccountRef, currency money.Currency) (LedgerAccountID, error)

	// Post records transfer atomically: every posting or none. It calls
	// [Transfer.Validate] before any I/O and refuses whatever it
	// refuses; a posting naming an account EnsureAccount never issued
	// is refused wrapping [ErrUnknownAccount].
	//
	// Post is idempotent on the transfer's key. Replaying a key with
	// the same transfer returns the original reference and records
	// nothing - a retry cannot move money twice - and replaying it with
	// a different transfer is refused wrapping
	// [ErrIdempotencyConflict], because two transfers under one
	// identity is the bug idempotency exists to surface, not to hide.
	// The returned reference is never blank: it is what the domain
	// stores as ledger_transfer_ref, the seam an auditor follows from a
	// state change to the money that made it real (D7).
	Post(ctx context.Context, transfer Transfer) (TransferRef, error)

	// Balance returns the sum of every posting on the account: the
	// ledger's own answer, computed from postings at the moment of the
	// call, never served from a cache that could drift from them. The
	// currency states what the caller believes the account holds, so a
	// balance can never come back denominated in a currency nobody
	// named (C-6): an account of a different currency is refused
	// wrapping [money.ErrCurrencyMismatch]. An account with no postings
	// is zero in its currency; an id EnsureAccount never issued is
	// refused wrapping [ErrUnknownAccount].
	Balance(ctx context.Context, account LedgerAccountID, currency money.Currency) (money.Amount, error)

	// History returns the account's postings inside window, each
	// carrying the TransferRef and PostedAt the ledger recorded,
	// ordered by ascending PostedAt. Membership in the window is
	// [Window.Contains], identically in every implementation.
	//
	// The result is the standard library's iterator shape rather than a
	// slice or a page-and-cursor pair, because a history is unbounded
	// and its natural readers - a totals computation, a reconciliation
	// - are single passes that should not require materialising it. The
	// immediate error covers what is checkable before any posting
	// flows: an unknown account (wrapping [ErrUnknownAccount]) or
	// swapped bounds (wrapping [ErrInvalidWindow]). A failure while
	// streaming is yielded as the pair's error, after which iteration
	// ends: a caller that stops ranging early reads nothing further and
	// leaks nothing.
	History(ctx context.Context, account LedgerAccountID, window Window) (iter.Seq2[Posting, error], error)
}
