// Package postgres implements the [wallet.Ledger] port over the exit-route
// schema that migration 0022_pg_ledger creates: three tables (account,
// transfer, posting), a deferred zero-sum trigger and a unique idempotency
// key, in the `ledger` schema of the application's own database. It exists
// so the Blnk adoption stays honest (ADR-0002): the decision to carry money
// on an external ledger is reversible in days precisely because this
// implementation is real, tested and behind the same port.
//
// The port's promises land in the database, not in advisory code:
//
//   - C-1 is checked twice by design. [wallet.Transfer.Validate] refuses an
//     unbalanced transfer before any I/O, and the schema's deferred
//     constraint trigger refuses one again at COMMIT - including one
//     written in SQL behind this package's back.
//   - Idempotency is the UNIQUE constraint on the key. Of N concurrent
//     posts of one key, the index lets exactly one insert commit; the
//     losers read the winner back and answer with its reference (a true
//     replay) or with [wallet.ErrIdempotencyConflict] (a different
//     transfer), never with a raw unique-violation error.
//   - C-6 is the column types: bigint minor units beside a format-checked
//     char(3) currency, and a composite foreign key that makes a posting
//     in a currency its account does not hold unrepresentable.
//   - The rule that a member's stage account never goes negative - the
//     withdrawal reservation's double-spend defence (D9) - is a row lock
//     over the member accounts a transfer touches, taken before any
//     balance is read, plus a second deferred trigger that states the same
//     rule for writers that never come through this package. The account's
//     kind column is what tells a member's bucket from a house account,
//     which is exempt.
//
// Balances are never stored. Balance sums the account's postings in SQL at
// the moment of the call, and the schema's balances view derives the same
// figures for the continuous C-1 check (D7).
//
// One honest divergence from the in-memory reference: instants pass
// through a timestamptz column, so the PostedAt a History read returns is
// the posted instant at Postgres's microsecond resolution. Callers that
// draw windows from instants they invented should keep them
// microsecond-aligned; windows drawn from instants read back out of
// History are exact by construction.
package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The port is satisfied by *Ledger or not at all; this line is where the
// compiler says so.
var _ wallet.Ledger = (*Ledger)(nil)

// DB is the slice of database access this ledger needs, named here per the
// boundary rules (the consumer names its dependency) so the package
// depends on the shape it uses rather than on pgxpool. *pgxpool.Pool
// satisfies it; so does pgx.Tx, in which case every Post nests inside the
// caller's transaction as a savepoint and commits only when the caller
// does.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// querier is the read-only slice of DB the lookup helpers need. Both DB
// and pgx.Tx satisfy it, so a helper reads identically inside and outside
// a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The SQL, in one place. Every statement names the ledger schema
// explicitly rather than trusting a search path some session might have
// rearranged.
const (
	// ON CONFLICT names no target on purpose. The account table carries
	// two unique indexes - the primary key on id, and the (id, currency)
	// pair the posting foreign key aims at - and an arbiter naming only
	// one of them leaves the other free to raise: when callers ensure one
	// account at the same instant, a loser's speculative insert reaches
	// the unnamed index and comes back a unique violation instead of the
	// row that already exists. Arbitrating every constraint is what makes
	// EnsureAccount idempotent under the concurrency the port promises it
	// survives.
	sqlEnsureAccount = `
		insert into ledger.account (id, currency, kind)
		values ($1, $2, $3)
		on conflict do nothing`

	sqlAccountCurrency = `
		select currency from ledger.account where id = $1`

	// Post needs the kind beside the currency: the solvency rule governs
	// member accounts and exempts house ones, and the column is what says
	// which is which. The id's member/ prefix says the same thing, but the
	// port declares ids opaque - a query that read one would be SQL
	// coupled to a spelling this package is free to change.
	sqlAccountCurrencyAndKind = `
		select currency, kind from ledger.account where id = $1`

	sqlAccountExists = `
		select exists (select 1 from ledger.account where id = $1)`

	// The row lock that makes the solvency check race-free. A plain sum
	// would not be: under READ COMMITTED two concurrent posts each see
	// their own snapshot, both read the same balance, and both pass. This
	// statement makes the second wait out the first, and the balance query
	// after it is a new statement with a new snapshot - so the loser sums
	// a balance that already includes what the winner took.
	//
	// The account rows are the rendezvous because postings are only ever
	// inserted: there is no posting row two transfers could contend on.
	// ORDER BY id gives every transfer one lock order, so two transfers
	// over the same pair of accounts cannot each hold what the other
	// wants.
	sqlLockAccounts = `
		select 1 from ledger.account where id = any($1) order by id for update`

	// The cast is the same overflow guard sqlSumAccountPostings carries.
	sqlAccountBalances = `
		select a.id, coalesce(sum(p.amount_minor), 0)::bigint
		  from ledger.account a
		  left join ledger.posting p on p.account_id = a.id
		 where a.id = any($1)
		 group by a.id`

	// ON CONFLICT DO NOTHING is the whole concurrency story: when two
	// posts of one key race, the unique index makes the second insert wait
	// for the first transaction's outcome and return no row if it
	// committed. The loser then reads the winner back - no advisory lock,
	// no retry loop, and no raw unique-violation error escaping to a
	// caller.
	sqlClaimIdempotencyKey = `
		insert into ledger.transfer (idempotency_key, reference, metadata, posted_at)
		values ($1, $2, $3, $4)
		on conflict (idempotency_key) do nothing
		returning ref`

	sqlTransferByKey = `
		select ref, reference, metadata from ledger.transfer where idempotency_key = $1`

	sqlPostingsByTransfer = `
		select account_id, amount_minor, currency from ledger.posting where transfer_ref = $1`

	sqlInsertPosting = `
		insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
		values ($1, $2, $3, $4)`

	// The cast is the overflow guard: sum(bigint) widens to numeric, and
	// forcing the result back into bigint makes a balance that left the
	// int64 range a loud error instead of a wrapped-around number.
	sqlSumAccountPostings = `
		select coalesce(sum(p.amount_minor), 0)::bigint
		  from ledger.posting p
		 where p.account_id = $1`

	// This WHERE clause is wallet.Window.Contains spelled in SQL:
	// half-open, From inclusive, To exclusive, a null bound imposing
	// nothing. Ordered by the transfer's instant with the posting id - the
	// recording order - breaking ties, so a frozen clock cannot shuffle
	// the record.
	sqlHistory = `
		select p.amount_minor, p.currency, p.transfer_ref, t.posted_at
		  from ledger.posting p
		  join ledger.transfer t on t.ref = p.transfer_ref
		 where p.account_id = $1
		   and ($2::timestamptz is null or t.posted_at >= $2)
		   and ($3::timestamptz is null or t.posted_at < $3)
		 order by t.posted_at, p.id`
)

// numericValueOutOfRange is the SQLSTATE Postgres raises when a value will
// not fit the type it is being forced into - here, a postings sum forced
// back into bigint. It is translated to [money.ErrOverflow] so every
// implementation refuses an unrepresentable balance with the same error.
const numericValueOutOfRange = "22003"

// The two values ledger.account.kind takes, mirroring the two shapes
// [wallet.AccountRef] has. They are written by EnsureAccount and read by
// the solvency check; the schema's own check constraint allows no others.
const (
	kindMember = "member"
	kindHouse  = "house"
)

// Ledger is the Postgres implementation of [wallet.Ledger]. Build one with
// [New]; the zero value carries no database and is not usable.
//
// It holds no state of its own - every account, transfer and posting lives
// in the ledger schema - so any number of Ledger values over one database
// are one ledger, across processes and restarts alike.
type Ledger struct {
	db DB
	// now supplies each transfer's PostedAt. It is a field rather than a
	// call to time.Now so tests can hand in a clock of their own
	// ([WithClock]) and draw windows around postings exactly.
	now func() time.Time
}

// Option configures the ledger [New] returns.
type Option func(*Ledger)

// WithClock replaces the source PostedAt is read from, so a test can hand
// out instants of its own choosing and get reproducible windows. The
// ledger calls the clock once per posted transfer, concurrently when posts
// race, so a stateful clock must serialise itself. A nil clock keeps the
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

// New returns a ledger over db, which must not be nil: time is read from
// [time.Now] unless [WithClock] replaces it. The schema it speaks to is
// created by migration 0022_pg_ledger; New performs no I/O and does not
// verify the schema exists, so a miswired database surfaces on first use.
func New(db DB, opts ...Option) *Ledger {
	l := &Ledger{db: db, now: time.Now}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// EnsureAccount resolves ref in currency to this ledger's identity for the
// account, creating the row the first time the pair is named. The id is
// derived deterministically from (ref, currency) and the insert is
// ON CONFLICT DO NOTHING, so idempotency needs no bookkeeping and no lock:
// the same pair spells the same id on every call, however many callers or
// processes race, and the row only ever goes from absent to present. An
// unusable ref is refused wrapping [wallet.ErrInvalidAccountRef], a
// malformed currency wrapping [money.ErrInvalidCurrency], both before any
// I/O.
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
		return "", fmt.Errorf("postgres: %w", err)
	}

	// Which shape the ref carries is settled here, where the ref itself is
	// in hand, and stored on the row: it is what Post's solvency rule
	// governs by, and the ref is the only place the fact is stated without
	// inference.
	kind := kindHouse
	if _, _, member := ref.Member(); member {
		kind = kindMember
	}

	id := accountID(ref, currency)
	if _, err := l.db.Exec(ctx, sqlEnsureAccount, string(id), string(currency), kind); err != nil {
		return "", fmt.Errorf("postgres: ensuring account %q: %w", id, err)
	}
	return id, nil
}

// accountID derives this ledger's identity for (ref, currency). The
// derivation is injective - member ids are fixed-format UUIDs, house names
// are quoted so no name can smuggle a separator, and the two shapes carry
// distinct prefixes - so distinct accounts can never share a row. Every
// caller treats the result as opaque (the port says so); the readable
// spelling is for a human reading the ledger tables directly, not for
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

// Post records transfer atomically: every posting or none, in one database
// transaction. [wallet.Transfer.Validate] runs first, before any I/O, so a
// transfer that would create or destroy money is refused before it can
// leave the process - and the schema's deferred trigger would refuse it
// again at COMMIT if it somehow got past (C-1, checked twice by design).
//
// Idempotency is decided by the transfer's key, compared byte for byte. A
// key already recorded answers before anything else is judged: the same
// key carrying the same transfer returns the original reference and
// records nothing, and the same key carrying a different transfer is
// refused wrapping [wallet.ErrIdempotencyConflict]. "The same transfer" is
// semantic, not positional - the postings must be the same multiset of
// (account, amount) movements, and Reference and Metadata must match by
// content, with nil and empty metadata both annotating nothing.
//
// A key not yet recorded is claimed by inserting the transfer row under
// the schema's unique constraint, ON CONFLICT DO NOTHING. That constraint
// is the entire concurrent-replay story: of N posts racing one key,
// exactly one insert returns a reference and commits; each loser's insert
// waits out the winner's transaction, returns nothing, and the loser then
// judges its own transfer against what the winner committed - the same
// replay-or-conflict decision, one committed winner, never a raw
// unique-violation error.
//
// Every posting must name an account EnsureAccount issued, in that
// account's own currency: an unissued id is refused wrapping
// [wallet.ErrUnknownAccount], a posting denominated in a currency the
// account does not hold wrapping [money.ErrCurrencyMismatch]. Both checks
// run over the whole transfer inside the transaction, so a refused Post
// rolls back having changed no balance and consumed no key.
//
// A transfer that would leave a member's stage account holding less than
// nothing is refused wrapping [wallet.ErrInsufficientFunds], judged on the
// transfer's net movement per account rather than posting by posting.
// House accounts are exempt: they are the boundary of the closed set of
// accounts, and a ledger in which nothing may go negative has nothing able
// to fund its first credit.
//
// That refusal is the double-spend defence [wallet.StageReserved] leans
// on, so it has to survive two withdrawal requests arriving at once, on
// separate connections, in separate transactions. Summing the account
// would not survive it: under READ COMMITTED each transaction sums its own
// snapshot, neither sees the other, and both pass. So this implementation
// takes a row lock on every member account the transfer touches - in id
// order, one statement, so two transfers over one pair of accounts cannot
// deadlock - and only then reads the balances. The second transfer waits
// out the first, and because the balance query is a statement of its own
// it sees a snapshot taken after the winner committed: it sums money that
// is already gone and is refused. The schema carries the same rule as a
// deferred constraint trigger, which is the half that judges SQL written
// behind this package's back.
//
// The solvency check runs after the idempotency key is claimed rather than
// beside the account checks, and that order is load-bearing. Two
// concurrent posts of one key must both learn the winner's reference; a
// loser that took the account lock first would instead wait out the
// winner's commit, find the money spent, and answer about funds when the
// question it asked was about a key.
func (l *Ledger) Post(ctx context.Context, transfer wallet.Transfer) (wallet.TransferRef, error) {
	if err := transfer.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("postgres: %w", err)
	}
	metadata, err := encodeMetadata(transfer.Metadata)
	if err != nil {
		return "", err
	}

	tx, err := l.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("postgres: beginning transfer transaction: %w", err)
	}
	// A no-op after Commit; everywhere else it is what makes "refused
	// means nothing happened" true.
	defer func() { _ = tx.Rollback(ctx) }()

	// The key may already be recorded: replay or conflict, decided before
	// the account checks so a recorded key always answers about what it
	// recorded, exactly as the in-memory reference does.
	if ref, seen, err := l.judgeRecordedKey(ctx, tx, transfer); err != nil || seen {
		return ref, err
	}

	// All-or-nothing: the whole transfer is judged before any posting is
	// written, so posting 3 failing cannot leave postings 1 and 2 behind.
	drawn, err := checkAccounts(ctx, tx, transfer.Postings)
	if err != nil {
		return "", err
	}

	// One instant per transfer: every posting of a transfer is recorded
	// at the same moment, read once, here.
	var minted string
	err = tx.QueryRow(ctx, sqlClaimIdempotencyKey,
		transfer.IdempotencyKey, transfer.Reference, metadata, l.now()).Scan(&minted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Lost the race: another post claimed the key and committed while
		// this one was in flight (the unique index made the insert wait
		// out that transaction). Read committed gives the next statement
		// a snapshot that sees the winner, so the replay-or-conflict
		// judgement now runs against it.
		ref, seen, err := l.judgeRecordedKey(ctx, tx, transfer)
		if err != nil {
			return "", err
		}
		if !seen {
			// Transfers are immutable and never deleted, so a key that
			// just refused an insert must be readable. Reported rather
			// than swallowed in case that ever stops being true.
			return "", fmt.Errorf("postgres: idempotency key %q was claimed concurrently but cannot be read back", transfer.IdempotencyKey)
		}
		return ref, nil
	case err != nil:
		return "", fmt.Errorf("postgres: claiming idempotency key %q: %w", transfer.IdempotencyKey, err)
	}

	// The key is this transfer's alone from here, so the question left is
	// whether the money is there. Locking now rather than before the claim
	// is what keeps a losing same-key post answering about its key.
	if err := checkFunds(ctx, tx, drawn); err != nil {
		return "", err
	}

	for i, p := range transfer.Postings {
		if _, err := tx.Exec(ctx, sqlInsertPosting,
			minted, string(p.Account), p.Amount.Minor, string(p.Amount.Currency)); err != nil {
			return "", fmt.Errorf("postgres: recording posting %d of %d on %q: %w", i+1, len(transfer.Postings), p.Account, err)
		}
	}
	// COMMIT is where the deferred zero-sum trigger passes judgement on
	// what was just written: Validate makes its refusal unreachable from
	// here, and the trigger exists for the SQL that does not come through
	// here.
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("postgres: committing transfer %q: %w", minted, err)
	}
	return wallet.TransferRef(minted), nil
}

// judgeRecordedKey answers for an idempotency key that is already in the
// ledger: the recorded reference for a true replay, a conflict for the
// same key carrying a different transfer, and seen false when the key is
// not recorded at all.
func (l *Ledger) judgeRecordedKey(ctx context.Context, q querier, transfer wallet.Transfer) (wallet.TransferRef, bool, error) {
	var ref, reference string
	var rawMetadata []byte
	err := q.QueryRow(ctx, sqlTransferByKey, transfer.IdempotencyKey).Scan(&ref, &reference, &rawMetadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("postgres: reading idempotency key %q: %w", transfer.IdempotencyKey, err)
	}

	recorded, err := readPostings(ctx, q, ref)
	if err != nil {
		return "", false, err
	}
	metadata, err := decodeMetadata(rawMetadata)
	if err != nil {
		return "", false, fmt.Errorf("postgres: metadata recorded under key %q: %w", transfer.IdempotencyKey, err)
	}
	if !matches(transfer, reference, metadata, recorded) {
		return "", true, fmt.Errorf("%w: key %q", wallet.ErrIdempotencyConflict, transfer.IdempotencyKey)
	}
	return wallet.TransferRef(ref), true, nil
}

// readPostings loads a recorded transfer's postings as (account, amount)
// movements, which is all replay identity compares.
func readPostings(ctx context.Context, q querier, ref string) ([]wallet.Posting, error) {
	rows, err := q.Query(ctx, sqlPostingsByTransfer, ref)
	if err != nil {
		return nil, fmt.Errorf("postgres: reading postings of %q: %w", ref, err)
	}
	defer rows.Close()

	var postings []wallet.Posting
	for rows.Next() {
		var account, currency string
		var minor int64
		if err := rows.Scan(&account, &minor, &currency); err != nil {
			return nil, fmt.Errorf("postgres: reading postings of %q: %w", ref, err)
		}
		amount, err := money.New(minor, money.Currency(currency))
		if err != nil {
			// Unreachable while the schema format-checks every currency -
			// and returned rather than swallowed in case that ever stops
			// being true.
			return nil, fmt.Errorf("postgres: posting of %q: %w", ref, err)
		}
		postings = append(postings, wallet.Posting{Account: wallet.LedgerAccountID(account), Amount: amount})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: reading postings of %q: %w", ref, err)
	}
	return postings, nil
}

// matches implements replay identity as documented on [Ledger.Post]:
// postings as a multiset of (account, amount) movements, reference and
// metadata by content.
func matches(transfer wallet.Transfer, reference string, metadata map[string]string, recorded []wallet.Posting) bool {
	if transfer.Reference != reference {
		return false
	}
	// maps.Equal compares content, so nil and empty metadata - both of
	// which annotate nothing - count as the same annotations.
	if !maps.Equal(transfer.Metadata, metadata) {
		return false
	}
	incoming := canonical(transfer.Postings)
	stored := canonical(recorded)
	if len(incoming) != len(stored) {
		return false
	}
	for i := range incoming {
		if incoming[i].Account != stored[i].Account || !incoming[i].Amount.Equal(stored[i].Amount) {
			return false
		}
	}
	return true
}

// canonical strips postings to what they mean - the account and the signed
// amount - and sorts them into one fixed order in Go rather than in SQL,
// so the comparison cannot depend on whatever collation the database
// happens to order text by. Two spellings of one movement of money compare
// equal and two different movements compare different, deterministically.
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

// drawing is one member account a transfer touches, beside the net
// movement the transfer makes on it. The net is what the solvency rule is
// judged on: a transfer that takes 5000 out of a bucket and puts 4500 back
// in the same atomic act leaves the account 500 lighter and never leaves
// it short of anything.
type drawing struct {
	account wallet.LedgerAccountID
	net     money.Amount
}

// checkAccounts refuses the transfer whole when any posting names an
// account the ledger never issued (wrapping [wallet.ErrUnknownAccount]) or
// moves a currency its account does not hold (wrapping
// [money.ErrCurrencyMismatch]). The composite foreign key enforces the
// currency rule again at the insert; checking here first is what turns the
// refusal into the port's error with the offending posting named.
//
// It also nets the transfer over the member accounts it touches and hands
// that back for [checkFunds] to judge, in the order the postings
// introduced them - so a transfer overdrawing two accounts always names
// the same one. House accounts are left out here rather than skipped
// later: they are exempt from the rule, so they are not drawings.
func checkAccounts(ctx context.Context, q querier, postings []wallet.Posting) ([]drawing, error) {
	var drawn []drawing
	at := make(map[wallet.LedgerAccountID]int, len(postings))
	for i, p := range postings {
		var held, kind string
		err := q.QueryRow(ctx, sqlAccountCurrencyAndKind, string(p.Account)).Scan(&held, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: posting %d of %d names %q", wallet.ErrUnknownAccount, i+1, len(postings), p.Account)
		}
		if err != nil {
			return nil, fmt.Errorf("postgres: reading account %q: %w", p.Account, err)
		}
		if p.Amount.Currency != money.Currency(held) {
			return nil, fmt.Errorf("postgres: posting %d of %d: %w: account %q holds %s, the posting moves %s",
				i+1, len(postings), money.ErrCurrencyMismatch, p.Account, held, p.Amount.Currency)
		}
		if kind != kindMember {
			continue
		}
		j, seen := at[p.Account]
		if !seen {
			at[p.Account] = len(drawn)
			drawn = append(drawn, drawing{account: p.Account, net: p.Amount})
			continue
		}
		// Add can only fail on overflow: the currency check above passed,
		// so every posting on one account shares that account's currency.
		sum, err := drawn[j].net.Add(p.Amount)
		if err != nil {
			return nil, fmt.Errorf("postgres: netting the postings on account %q: %w", p.Account, err)
		}
		drawn[j].net = sum
	}
	return drawn, nil
}

// checkFunds refuses the transfer whole, wrapping
// [wallet.ErrInsufficientFunds], when applying it would leave any of the
// member accounts it draws on holding less than nothing. A transfer that
// touches none - the ordinary commission split, whose only giver is a
// house account - costs nothing here: no lock is taken and no query runs.
//
// The lock comes first and the balances second, in that order and as two
// statements, which is the whole of why the check is race-free. See
// [Ledger.Post].
func checkFunds(ctx context.Context, q querier, drawn []drawing) error {
	if len(drawn) == 0 {
		return nil
	}
	ids := make([]string, len(drawn))
	for i, d := range drawn {
		ids[i] = string(d.account)
	}
	if err := lockAccounts(ctx, q, ids); err != nil {
		return err
	}
	held, err := balancesOf(ctx, q, ids)
	if err != nil {
		return err
	}

	for _, d := range drawn {
		// The account's currency is the posting's: checkAccounts refused
		// the transfer otherwise, which is what lets a bigint from SQL
		// become an amount here without asking the row again.
		before := money.Amount{Minor: held[d.account], Currency: d.net.Currency}
		after, err := before.Add(d.net)
		if err != nil {
			return fmt.Errorf("postgres: applying %s to account %q: %w", d.net, d.account, err)
		}
		if after.IsNegative() {
			return fmt.Errorf("%w: account %q holds %s and this transfer would leave it at %s",
				wallet.ErrInsufficientFunds, d.account, before, after)
		}
	}
	return nil
}

// lockAccounts takes a row lock on each of the named accounts, in id
// order, and holds it for the rest of the transaction. Every row is read,
// because a lock is only taken as its row is returned, and the count is
// checked against what was asked for: the ids were all read out of
// ledger.account moments ago and the rows can never be deleted, so a
// missing one means an assumption this package rests on has stopped being
// true.
func lockAccounts(ctx context.Context, q querier, ids []string) error {
	rows, err := q.Query(ctx, sqlLockAccounts, ids)
	if err != nil {
		return fmt.Errorf("postgres: locking accounts %v: %w", ids, err)
	}
	defer rows.Close()

	locked := 0
	for rows.Next() {
		locked++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: locking accounts %v: %w", ids, err)
	}
	if locked != len(ids) {
		return fmt.Errorf("postgres: locked %d of the %d accounts %v", locked, len(ids), ids)
	}
	return nil
}

// balancesOf sums the postings of each named account in one statement,
// which under READ COMMITTED is one snapshot: the balances answer as of a
// single instant, after every transfer that had committed by then and
// after none that had not.
func balancesOf(ctx context.Context, q querier, ids []string) (map[wallet.LedgerAccountID]int64, error) {
	rows, err := q.Query(ctx, sqlAccountBalances, ids)
	if err != nil {
		return nil, summingErr(ids, err)
	}
	defer rows.Close()

	balances := make(map[wallet.LedgerAccountID]int64, len(ids))
	for rows.Next() {
		var id string
		var minor int64
		if err := rows.Scan(&id, &minor); err != nil {
			return nil, summingErr(ids, err)
		}
		balances[wallet.LedgerAccountID(id)] = minor
	}
	if err := rows.Err(); err != nil {
		return nil, summingErr(ids, err)
	}
	return balances, nil
}

// summingErr reports a failure to sum postings, translating Postgres's
// out-of-range SQLSTATE into [money.ErrOverflow] so a balance too large
// for an int64 is refused with the port's own error rather than a driver's.
func summingErr(ids []string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == numericValueOutOfRange {
		return fmt.Errorf("postgres: summing accounts %v: %w", ids, money.ErrOverflow)
	}
	return fmt.Errorf("postgres: summing accounts %v: %w", ids, err)
}

// encodeMetadata renders the transfer's annotations for the jsonb column.
// Nil and empty maps both become the empty object, so the two spellings of
// "no annotations" are one recorded value.
func encodeMetadata(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		// Unreachable for a map of strings - and returned rather than
		// swallowed in case the type ever changes.
		return nil, fmt.Errorf("postgres: encoding transfer metadata: %w", err)
	}
	return raw, nil
}

// decodeMetadata reads the annotations back for replay comparison. A
// document that will not decode into strings was written behind this
// package's back and is reported loudly rather than treated as matching
// nothing.
func decodeMetadata(raw []byte) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("does not decode as string annotations: %w", err)
	}
	return m, nil
}

// Balance sums every posting on the account in SQL at the moment of the
// call - there is no stored figure that could drift from the postings
// (D7), and a transfer's postings commit atomically, so the single-
// statement sum can never see half of one. The currency argument is the
// caller's assertion of what the account holds (C-6): an account of a
// different currency is refused wrapping [money.ErrCurrencyMismatch], an
// id this ledger never issued wrapping [wallet.ErrUnknownAccount]. An
// account with no postings is zero in its currency. A sum that leaves the
// int64 range is refused wrapping [money.ErrOverflow] rather than wrapped
// around: a balance that reads as its own negation is worse than an error.
func (l *Ledger) Balance(ctx context.Context, account wallet.LedgerAccountID, currency money.Currency) (money.Amount, error) {
	if err := ctx.Err(); err != nil {
		return money.Amount{}, fmt.Errorf("postgres: %w", err)
	}

	// The account row is immutable, so reading it in a separate statement
	// from the sum below races nothing.
	var held string
	err := l.db.QueryRow(ctx, sqlAccountCurrency, string(account)).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Amount{}, fmt.Errorf("%w: %q", wallet.ErrUnknownAccount, account)
	}
	if err != nil {
		return money.Amount{}, fmt.Errorf("postgres: reading account %q: %w", account, err)
	}
	if currency != money.Currency(held) {
		return money.Amount{}, fmt.Errorf("postgres: account %q: %w: holds %s, asked for %s",
			account, money.ErrCurrencyMismatch, held, currency)
	}

	var minor int64
	if err := l.db.QueryRow(ctx, sqlSumAccountPostings, string(account)).Scan(&minor); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == numericValueOutOfRange {
			return money.Amount{}, fmt.Errorf("postgres: summing account %q: %w", account, money.ErrOverflow)
		}
		return money.Amount{}, fmt.Errorf("postgres: summing account %q: %w", account, err)
	}
	return money.Amount{Minor: minor, Currency: currency}, nil
}

// History streams the account's postings whose PostedAt falls inside
// window ([wallet.Window.Contains], spelled in SQL), ordered by ascending
// PostedAt with ties broken by recording order, each carrying the
// reference and instant the ledger recorded. Swapped bounds are refused
// wrapping [wallet.ErrInvalidWindow], an id this ledger never issued
// wrapping [wallet.ErrUnknownAccount] - both immediately, before the
// iterator exists.
//
// The iterator is lazy: the query runs when iteration begins, holds one
// connection while it runs, and releases it when the consumer finishes or
// stops early - an iterator that is never ranged costs nothing and leaks
// nothing. The stream is one statement's snapshot, so a Post racing the
// iteration - from inside the consumer's own loop included - can neither
// tear it nor leak into it; it lands in the next History call. A context
// cancelled mid-stream is yielded as the pair's error, after which
// iteration ends.
func (l *Ledger) History(ctx context.Context, account wallet.LedgerAccountID, window wallet.Window) (iter.Seq2[wallet.Posting, error], error) {
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	var exists bool
	if err := l.db.QueryRow(ctx, sqlAccountExists, string(account)).Scan(&exists); err != nil {
		return nil, fmt.Errorf("postgres: reading account %q: %w", account, err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %q", wallet.ErrUnknownAccount, account)
	}

	// A zero bound imposes nothing; null is how the query says so.
	var from, to *time.Time
	if !window.From.IsZero() {
		from = &window.From
	}
	if !window.To.IsZero() {
		to = &window.To
	}

	return func(yield func(wallet.Posting, error) bool) {
		rows, err := l.db.Query(ctx, sqlHistory, string(account), from, to)
		if err != nil {
			yield(wallet.Posting{}, fmt.Errorf("postgres: reading history of %q: %w", account, err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			// Checked here as well as left to the driver, so a consumer
			// that cancels mid-stream sees the cancellation at the next
			// posting deterministically, however much of the result the
			// connection had already buffered.
			if err := ctx.Err(); err != nil {
				yield(wallet.Posting{}, fmt.Errorf("postgres: %w", err))
				return
			}
			var (
				minor         int64
				currency, ref string
				postedAt      time.Time
			)
			if err := rows.Scan(&minor, &currency, &ref, &postedAt); err != nil {
				yield(wallet.Posting{}, fmt.Errorf("postgres: reading history of %q: %w", account, err))
				return
			}
			amount, err := money.New(minor, money.Currency(currency))
			if err != nil {
				// Unreachable while the schema format-checks every
				// currency - and yielded rather than swallowed in case
				// that ever stops being true.
				yield(wallet.Posting{}, fmt.Errorf("postgres: history of %q: %w", account, err))
				return
			}
			p := wallet.Posting{
				Account:     account,
				Amount:      amount,
				TransferRef: wallet.TransferRef(ref),
				PostedAt:    postedAt,
			}
			if !yield(p, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(wallet.Posting{}, fmt.Errorf("postgres: reading history of %q: %w", account, err))
		}
	}, nil
}
