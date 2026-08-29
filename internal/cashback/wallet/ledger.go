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
// crosses this boundary in either direction: every signature below speaks
// types declared here or in platform/money, and the architecture test in
// internal/arch fails the build when a package outside this package's own
// adapter directories imports a ledger vendor's SDK. That is what keeps the
// substrate swappable inside the constitution's five-day replaceability
// budget.
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
// A third rule is this port's own rather than the constitution's: a
// transfer must move at least one account's balance ([ErrNoMovement]).
// Summing to zero and moving money are different properties, and postings
// that cancel on the account they name satisfy the first while failing the
// second - so without this rule a caller whose source and destination
// resolved to one account would be handed a TransferRef, which the domain
// stores as proof that money moved.
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
	// ErrNoMovement reports a transfer that balances by moving nothing:
	// every account it touches nets to zero, so posting it would mint a
	// real TransferRef - stored in entry_transition and ledger_link as
	// proof that money moved (D7) - for money that never moved. The
	// likeliest way one appears is a caller whose source and destination
	// resolved to the same account, which is a bug worth seeing rather
	// than a transfer worth recording. A transfer in which one account
	// nets to zero while another moves is legal: money did move, and the
	// cancelling pair is that caller's business.
	ErrNoMovement = errors.New("wallet: transfer leaves every account's balance unchanged")
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
	// ErrInsufficientFunds reports a Post that would leave a member's
	// stage account holding less than nothing. It is the mechanism behind
	// the reservation D9 leans on: two withdrawal requests racing for one
	// confirmed balance both try to take it, and the ledger - which
	// applies transfers one at a time against a real balance - is what
	// makes exactly one of them win. Without this refusal the port would
	// advertise a double-spend defence it did not have. See
	// [StageReserved] and [Ledger.Post] for which accounts it governs and
	// why house accounts are exempt.
	ErrInsufficientFunds = errors.New("wallet: account holds too little for this transfer")
	// ErrUnsupportedTransfer reports a transfer this port admits but the
	// ledger behind it cannot record in one atomic act - a transfer
	// spanning currencies where the substrate denominates a transaction in
	// one, most of all. Refusing is the only honest answer: Post promises
	// every posting or none, and splitting such a transfer into several of
	// the substrate's own would break that promise silently. Callers that
	// meet it split the movement themselves, into one transfer per
	// currency with a key each, and accept that the parts are separately
	// idempotent because they are separately atomic.
	ErrUnsupportedTransfer = errors.New("wallet: the ledger cannot record this transfer atomically")
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
	// double-spend window between request and approval - and it closes it
	// only because a member's stage account may not go negative: two
	// requests racing for one confirmed balance both post a transfer out
	// of it, and the second is refused wrapping [ErrInsufficientFunds]
	// rather than overdrawing the member into a payout the money does not
	// cover. Nothing else in the design refuses it; there is no
	// one-open-request constraint on withdrawal_request, so this is the
	// defence, not a belt beside a brace.
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
	// Post. It is the instant Window bounds select on, carried at
	// whatever resolution the ledger stores, and it names a moment
	// rather than a posting - every posting of one transfer shares it.
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
	// every currency they touch (C-1), and moving at least one account's
	// balance. A transfer may span currencies - each balances
	// independently - but no posting nets against one in another
	// currency, ever, and an implementation whose substrate denominates a
	// transaction in a single currency refuses the cross-currency shape
	// wrapping [ErrUnsupportedTransfer] rather than posting it in
	// instalments. Within one currency the shape is unrestricted: any
	// number of accounts giving and any number receiving.
	//
	// Slice order is not part of the transfer's identity. It is preserved
	// in error messages so a refusal can point at "posting 2 of 3", and
	// ignored everywhere else - see [Ledger.Post] on what makes two
	// transfers the same.
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
//     other could have made such a transfer look balanced;
//   - at least one account ends the transfer holding a different amount
//     than it started with ([ErrNoMovement]). Summing to zero is not the
//     same as moving money: postings that cancel on the account they
//     name balance perfectly and move nothing, and a transfer of nothing
//     that is nonetheless handed a TransferRef puts a proof of payment in
//     entry_transition for a payment that never happened.
//
// The first hole found is the one reported, with the offending posting's
// position in the message, so a refused transfer says which of its parts
// to look at. The movement check is last because it is the only rule a
// transfer can break while breaking no other: it fires on transfers that
// are otherwise entirely well formed.
//
// What Validate cannot check is anything only the ledger knows - whether
// an account exists, what currency it holds, whether it can afford to
// give. Those are [Ledger.Post]'s refusals, and they are stated there.
func (t Transfer) Validate() error {
	if strings.TrimSpace(t.IdempotencyKey) == "" {
		return ErrMissingIdempotencyKey
	}
	if len(t.Postings) < 2 {
		return fmt.Errorf("%w: got %d", ErrTooFewPostings, len(t.Postings))
	}

	// One pass does the per-posting checks and both nettings - per
	// currency for C-1, per account for the movement check - in posting
	// order, so the totals report in the order the transfer introduced
	// them and the same broken transfer is always refused with the same
	// message.
	nets := make(map[money.Currency]money.Amount, 1)
	currencies := make([]money.Currency, 0, 1)
	moved := make(map[holding]money.Amount, len(t.Postings))
	touched := make([]holding, 0, len(t.Postings))
	var err error
	for i, p := range t.Postings {
		position := fmt.Sprintf("posting %d of %d", i+1, len(t.Postings))
		if strings.TrimSpace(string(p.Account)) == "" {
			return fmt.Errorf("%w: %s", ErrMissingAccount, position)
		}
		if p.TransferRef != "" || !p.PostedAt.IsZero() {
			return fmt.Errorf("%w: %s was read from history, not built for this transfer", ErrRecycledPosting, position)
		}
		if err = p.Amount.Validate(); err != nil {
			return fmt.Errorf("wallet: %s: %w", position, err)
		}
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: %s", ErrZeroPosting, position)
		}

		if currencies, err = accumulate(nets, currencies, p.Amount.Currency, p.Amount); err != nil {
			return fmt.Errorf("wallet: netting %s postings: %w", p.Amount.Currency, err)
		}
		where := holding{account: p.Account, currency: p.Amount.Currency}
		if touched, err = accumulate(moved, touched, where, p.Amount); err != nil {
			return fmt.Errorf("wallet: netting the postings on account %q: %w", p.Account, err)
		}
	}

	var off []string
	for _, currency := range currencies {
		if !nets[currency].IsZero() {
			off = append(off, fmt.Sprintf("%s nets to %s", currency, nets[currency]))
		}
	}
	switch len(off) {
	case 0:
		// Balanced. Whether it is also a movement is the next question.
	case 1:
		return fmt.Errorf("%w: %s", ErrUnbalanced, off[0])
	default:
		return fmt.Errorf("%w: %s; each currency must sum to zero on its own", ErrMixedCurrency, strings.Join(off, ", "))
	}

	for _, where := range touched {
		if !moved[where].IsZero() {
			return nil
		}
	}
	return fmt.Errorf("%w: every posting nets to zero on the account it names (%s)", ErrNoMovement, strings.Join(accountsOf(touched), ", "))
}

// holding keys a running net by the account it moves on and the currency
// it moves in. An account holds exactly one currency (C-6) and Post
// refuses a posting in any other, but Validate runs before any
// implementation can say which currency that is, so the pair is the key
// rather than the account alone - which also keeps [money.Amount.Add]
// from ever being handed two currencies.
type holding struct {
	account  LedgerAccountID
	currency money.Currency
}

// accumulate adds amount to the running total under key, appending key to
// order the first time it is seen so the totals report in the order the
// transfer introduced them. Add can only fail on overflow - every amount
// under one key shares its currency by construction of the key - and an
// overflowing running total is refused outright: a transfer this port
// cannot even sum is not one any ledger should be asked to record.
func accumulate[K comparable](totals map[K]money.Amount, order []K, key K, amount money.Amount) ([]K, error) {
	total, seen := totals[key]
	if !seen {
		totals[key] = amount
		return append(order, key), nil
	}
	sum, err := total.Add(amount)
	if err != nil {
		return order, err
	}
	totals[key] = sum
	return order, nil
}

// accountsOf names the distinct accounts a set of holdings touches, in the
// order the transfer introduced them, quoted for the error message. It
// de-duplicates because one account can appear under two currencies, and
// naming it twice would read as two accounts.
func accountsOf(holdings []holding) []string {
	seen := make(map[LedgerAccountID]bool, len(holdings))
	names := make([]string, 0, len(holdings))
	for _, h := range holdings {
		if seen[h.account] {
			continue
		}
		seen[h.account] = true
		names = append(names, strconv.Quote(string(h.account)))
	}
	return names
}

// Window bounds a History read by when the ledger recorded each posting:
// half-open, taking every posting with From <= PostedAt < To. Half-open is
// what lets adjacent windows partition a history with no posting counted
// twice and none falling in a seam.
//
// The zero value is the whole of history, and each bound widens away
// independently: a zero From starts at the first posting, a zero To never
// ends. That makes "everything", "everything since a watermark" and "one
// reconciliation period" the same type with no flags.
//
// Resuming from a watermark is at-least-once, not exactly-once, and how
// the bound is set is what keeps it safe. PostedAt carries whatever
// resolution the ledger stores - the Postgres implementation truncates to
// microseconds, and every posting of one transfer shares that transfer's
// instant by design - so postings routinely share a PostedAt. A reader
// therefore sets From to the PostedAt of the last posting it consumed,
// never to an instant just after it: an inclusive bound re-yields that
// instant's postings, which an idempotent reader absorbs, while an
// exclusive one silently drops whichever of them the reader had not
// reached. History's ordering is stable and its ties are broken (see
// [Ledger.History]), so the re-read is the same sequence every time; what
// the port does not promise is that a watermark alone can distinguish two
// postings inside one instant.
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
//     same reference. What counts as a replay rather than a collision is
//     defined on Post, in one place, for all of them.
//  2. Post rejects a transfer whose postings do not sum to zero per
//     currency, before any I/O, by calling [Transfer.Validate] first
//     (C-1, checked twice by design).
//  3. Currencies never net against each other: each balances
//     independently within a transfer, and no implementation converts
//     between them. A posting whose currency is not its account's is
//     refused wrapping [money.ErrCurrencyMismatch].
//  4. Every amount is an int64 of minor units with an explicit ISO-4217
//     currency - [money.Amount], nothing else, anywhere (C-6).
//  5. Balance is the ledger's own authoritative figure for the account,
//     consistent to the minor unit with the postings it recorded and
//     never a number this repository stored beside them; member-facing
//     totals are computed by the wallet from postings, and the two are
//     cross-checked to the minor unit (D7).
//  6. A Post never leaves a member's stage account negative: it is
//     refused wrapping [ErrInsufficientFunds]. House accounts may go
//     negative, because that is where money enters and leaves the closed
//     set of accounts.
//  7. A transfer this port admits but an implementation cannot record in
//     one atomic act is refused wrapping [ErrUnsupportedTransfer], never
//     recorded in instalments.
//  8. What Post recorded is readable the moment Post returns: a Balance
//     or History call that begins after it sees the transfer's postings.
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
	// is refused wrapping [ErrUnknownAccount], and a posting denominated
	// in a currency its account does not hold wrapping
	// [money.ErrCurrencyMismatch]. An account holds one currency by
	// definition (C-6), so money of another kind has no meaning on it:
	// no implementation may convert the amount, pick a rate, or record
	// the posting under the account's currency instead. There is no
	// exchange rate anywhere in this port, and this is the refusal that
	// keeps it that way.
	//
	// Post is idempotent on the transfer's key. Replaying a key with
	// the same transfer returns the original reference and records
	// nothing - a retry cannot move money twice - and replaying it with
	// a different transfer is refused wrapping
	// [ErrIdempotencyConflict], because two transfers under one
	// identity is the bug idempotency exists to surface, not to hide.
	//
	// "The same transfer" is decided by content, never by spelling, and
	// exactly these things are compared:
	//
	//   - the postings, as a multiset of (account, amount) movements.
	//     Order is not identity: the sequence a caller assembled its
	//     slice in is representation, and refusing a retry that rebuilt
	//     the same movements in another order would break precisely the
	//     retry the key exists to make safe. Multiplicity is identity:
	//     the same movement listed twice is not the same transfer as the
	//     same movement listed once, because the two record different
	//     postings.
	//   - Reference, byte for byte.
	//   - Metadata, key for key and value for value, with a nil map and
	//     an empty map both meaning "no annotations" and comparing equal.
	//     A caller annotating one key two ways believes it is recording
	//     two different facts, which is the bug worth surfacing.
	//
	// Nothing else is compared. The provenance fields are required blank
	// on input by [Transfer.Validate], and the key itself is what is
	// being replayed.
	//
	// The key is judged before anything the ledger knows about accounts,
	// so a Post under a recorded key answers about what that key
	// recorded - even where posting the same transfer fresh would now be
	// refused. A replay is a question about a movement that already
	// happened, not a second attempt to make it happen.
	//
	// A Post may not leave a member's stage account holding less than
	// nothing: one that would is refused wrapping [ErrInsufficientFunds],
	// judged against the balance at the moment the transfer is applied
	// rather than a figure read earlier. That refusal is the double-spend
	// defence D9 leans on, so it is a requirement of the port and not a
	// courtesy - see [StageReserved]. House accounts are exempt: they are
	// the boundary of the closed set of accounts, the place a commission
	// arrives from and an absorbed loss goes to, and a ledger in which no
	// account may go negative has no account able to fund the first
	// credit. An implementation knows which of its ids name member
	// accounts, because EnsureAccount issued every one of them from an
	// [AccountRef].
	//
	// A transfer whose shape the ledger behind the port cannot record in
	// one atomic act - a transfer spanning currencies, where the
	// substrate denominates a transaction in one - is refused wrapping
	// [ErrUnsupportedTransfer]. Refusing is the contract: Post promises
	// every posting or none, and an implementation that quietly split
	// such a transfer into several of its substrate's own would be
	// promising atomicity it was not delivering.
	//
	// A refused Post changes no balance and leaves the key exactly as it
	// found it: unclaimed if nothing had claimed it, still bound to the
	// transfer that claimed it otherwise. So a caller whose transfer was
	// refused for any reason above may correct it and post it again under
	// the same key.
	//
	// The returned reference is never blank: it is what the domain
	// stores as ledger_transfer_ref, the seam an auditor follows from a
	// state change to the money that made it real (D7). It is also
	// readable immediately - a Balance or History call that begins after
	// Post returns sees this transfer's postings. An implementation whose
	// substrate records asynchronously does not return from Post until it
	// has stopped being asynchronous, because a wallet that recomputes a
	// member's total straight after a transition would otherwise show the
	// total from before it.
	Post(ctx context.Context, transfer Transfer) (TransferRef, error)

	// Balance returns what the ledger says the account holds as of the
	// call: its own authoritative figure, equal to the sum of the
	// postings it has recorded on that account and agreeing with a
	// History read of the same account to the minor unit. Every posting
	// Post has returned from is included (see Post on visibility).
	//
	// How the ledger arrives at the figure is the ledger's business.
	// Both implementations in this repository sum the postings on demand,
	// which is the simplest thing that is certainly right; a ledger built
	// for the job maintains the figure as it posts, and that is a ledger
	// keeping its own books rather than a cache. What the port forbids is
	// the second truth: a balance computed here, stored beside the
	// ledger and served afterwards, which is what D7 rules out and why
	// this port has no SetBalance.
	//
	// The currency states what the caller believes the account holds, so
	// a balance can never come back denominated in a currency nobody
	// named (C-6): an account of a different currency is refused
	// wrapping [money.ErrCurrencyMismatch]. An account with no postings
	// is zero in its currency; an id EnsureAccount never issued is
	// refused wrapping [ErrUnknownAccount].
	Balance(ctx context.Context, account LedgerAccountID, currency money.Currency) (money.Amount, error)

	// History returns the account's postings inside window, each
	// carrying the TransferRef and PostedAt the ledger recorded,
	// ordered by ascending PostedAt with ties broken by the order the
	// ledger recorded them. The tie-break is part of the contract rather
	// than left to the substrate, because ties are ordinary here: every
	// posting of one transfer carries that transfer's instant, and a
	// substrate may store instants more coarsely than Go measures them.
	// Two History calls over one window therefore yield the same
	// postings in the same order, which is what makes a re-read after a
	// watermark resume comparable to what it re-reads (see [Window]).
	// Every posting Post has returned from is eligible (see Post on
	// visibility). Membership in the window is [Window.Contains],
	// identically in every implementation.
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
