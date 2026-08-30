// The shared ledger conformance suite (T045).
//
// [wallet.Ledger] is a contract, not an interface shape: its doc comments
// say what every implementation must do, and three of them - in-memory,
// Postgres and Blnk - are supposed to do it identically. Three separate
// test suites cannot show that. Each was written by someone reading the
// same prose and reaching their own reading of it, and the readings only
// have to differ by a little for the wallet built on top to be right over
// one substrate and wrong over another.
//
// So this file asserts the contract once and runs it against all three. A
// scenario here is written against the port and never against an
// implementation: it may not name a driver, reach for a pool, or know that
// one of the three talks to a server at all. When a scenario cannot pass on
// an implementation, that is a divergence to be named in that package's
// documentation and reflected here deliberately - never a reason to soften
// what the suite asks for.
//
// This is an external test package, which is what lets it import all three
// implementations: they import [wallet], and nothing imports wallet_test.
//
// # What runs, and where
//
// The in-memory ledger always runs. Postgres runs when DATABASE_URL is set,
// Blnk when BLNK_URL is, in the same words their own suites use - so a
// developer without Docker gets the in-memory conformance for free and CI,
// where both are up, gets all three. A skip is reported per implementation
// rather than for the file, so a run that exercised one of the three cannot
// read as a run that exercised them all.
package wallet_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/blnk"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/postgres"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const (
	conformEUR = money.Currency("EUR")
	conformGBP = money.Currency("GBP")
)

// conformAnchor is the instant the conformance ledgers' clocks start from.
// Whole seconds, because timestamptz keeps microseconds and an instant that
// round-trips unchanged is one less difference between the implementations
// than the suite has to reason about.
var conformAnchor = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// implementation is one ledger under the shared suite: a name for the
// subtest and a way to get a fresh one. The factory takes the testing.T so
// it can skip when its backend is absent and fail when its backend is
// present but broken - those are different outcomes and must not collapse
// into one.
type implementation struct {
	name string
	// open answers a ledger no other test is using. Isolation is by naming
	// rather than by teardown: every account this suite touches is derived
	// from a fresh uuid, so two runs against one database never meet.
	open func(t *testing.T) wallet.Ledger
}

// conformPool is the Postgres pool the conformance suite shares, or nil
// when DATABASE_URL is unset. It is opened once for the package.
var conformPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if err := db.Migrate(url); err != nil {
			fmt.Fprintln(os.Stderr, "migrating the conformance database:", err)
			os.Exit(1)
		}
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parsing DATABASE_URL:", err)
			os.Exit(1)
		}
		// The same headroom the Postgres ledger's own suite takes: a
		// concurrent-post scenario holds a connection per racing caller
		// while they wait out the winner's commit, and pgx's default pool
		// is smaller than the race is wide.
		if want := int32(runtime.GOMAXPROCS(0)) + 4; cfg.MaxConns < want {
			cfg.MaxConns = want
		}
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connecting the conformance database:", err)
			os.Exit(1)
		}
		conformPool = pool
	}
	code := m.Run()
	if conformPool != nil {
		conformPool.Close()
	}
	os.Exit(code)
}

// implementations is the table every conformance scenario runs over.
func implementations() []implementation {
	return []implementation{
		{
			name: "memory",
			open: func(*testing.T) wallet.Ledger {
				return memory.New(memory.WithClock(fixedClock()))
			},
		},
		{
			name: "postgres",
			open: func(t *testing.T) wallet.Ledger {
				t.Helper()
				if conformPool == nil {
					t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise the postgres ledger")
				}
				return postgres.New(conformPool, postgres.WithClock(fixedClock()))
			},
		},
		{
			name: "blnk",
			open: func(t *testing.T) wallet.Ledger {
				t.Helper()
				endpoint := os.Getenv("BLNK_URL")
				if endpoint == "" {
					t.Skip("BLNK_URL is unset: no ledger to post to (expected without Docker)")
				}
				// A ledger of its own per test: the Blnk adapter namespaces
				// every account name by its ledger, and a shared name would
				// have two scenarios meeting in one pool of accounts.
				ledger, err := blnk.New(endpoint,
					blnk.WithSecretKey(os.Getenv("BLNK_SECRET_KEY")),
					blnk.WithLedgerName("conformance-"+uuid.NewString()),
				)
				if err != nil {
					t.Fatalf("opening the blnk ledger: %v", err)
				}
				return ledger
			},
		},
	}
}

// fixedClock answers a clock that starts at the anchor and advances a
// millisecond per reading, so postings within one scenario are ordered and
// distinct without any scenario having to sleep.
func fixedClock() func() time.Time {
	at := conformAnchor
	return func() time.Time {
		at = at.Add(time.Millisecond)
		return at
	}
}

// eachLedger runs scenario against every implementation, as a subtest per
// implementation so a failure names which one diverged.
func eachLedger(t *testing.T, scenario func(t *testing.T, ledger wallet.Ledger)) {
	t.Helper()
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			scenario(t, impl.open(t))
		})
	}
}

// conformMember answers an account reference no other test shares.
func conformMember(stage wallet.Stage) wallet.AccountRef {
	return wallet.MemberAccount(uuid.New(), stage)
}

// conformHouse answers a house reference no other test shares.
func conformHouse() wallet.AccountRef {
	return wallet.HouseAccount("conformance-" + uuid.NewString())
}

// ensure resolves ref and fails the scenario if the ledger will not.
func ensure(t *testing.T, ledger wallet.Ledger, ref wallet.AccountRef, currency money.Currency) wallet.LedgerAccountID {
	t.Helper()
	id, err := ledger.EnsureAccount(context.Background(), ref, currency)
	if err != nil {
		t.Fatalf("ensuring %s in %s: %v", ref, currency, err)
	}
	return id
}

// amount is the suite's only way of spelling money, so a scenario cannot
// accidentally compare an amount against a bare integer.
func amount(minor int64, currency money.Currency) money.Amount {
	return money.Amount{Minor: minor, Currency: currency}
}

// balance reads what the ledger says an account holds.
func balance(t *testing.T, ledger wallet.Ledger, id wallet.LedgerAccountID, currency money.Currency) money.Amount {
	t.Helper()
	held, err := ledger.Balance(context.Background(), id, currency)
	if err != nil {
		t.Fatalf("reading the balance of %q: %v", id, err)
	}
	return held
}

// TestConformanceEnsureAccountIsIdempotent is the suite's own smoke test:
// every implementation must resolve one (ref, currency) pair to one id,
// however many times it is asked. A ledger that fails this cannot be
// trusted to fail the rest for the right reason.
func TestConformanceEnsureAccountIsIdempotent(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ref := conformMember(wallet.StageConfirmed)
		first := ensure(t, ledger, ref, conformEUR)
		second := ensure(t, ledger, ref, conformEUR)
		if first != second {
			t.Errorf("one account resolved to two ids, %q then %q", first, second)
		}
		if held := balance(t, ledger, first, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("a freshly ensured account holds %s, want nothing", held)
		}
	})
}

// TestConformanceReplayRecordsOnce is the port's central promise: a retry
// cannot move money twice. Every implementation must answer a replayed key
// with the reference it recorded the first time, record nothing further,
// and leave the balances exactly where the first post left them.
//
// The replay deliberately rebuilds the transfer with its postings in the
// other order. The port defines sameness as a multiset of movements, not as
// the sequence a caller happened to assemble - a retry that rebuilt its
// slice from a map would otherwise be refused as a conflict, which is
// precisely the retry the key exists to make safe.
func TestConformanceReplayRecordsOnce(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)

		transfer := wallet.Transfer{
			IdempotencyKey: "conformance-replay-" + uuid.NewString(),
			Reference:      "an earning",
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformEUR)},
				{Account: member, Amount: amount(2500, conformEUR)},
			},
		}

		first, err := ledger.Post(ctx, transfer)
		if err != nil {
			t.Fatalf("posting: %v", err)
		}

		// The same movements, written the other way round.
		replay := transfer
		replay.Postings = []wallet.Posting{
			{Account: member, Amount: amount(2500, conformEUR)},
			{Account: house, Amount: amount(-2500, conformEUR)},
		}

		second, err := ledger.Post(ctx, replay)
		if err != nil {
			t.Fatalf("replaying the key with the same movements in another order: %v", err)
		}
		if first != second {
			t.Errorf("the replay answered with transfer %q, want the original %q", second, first)
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(2500, conformEUR)) {
			t.Errorf("the member holds %s after one transfer and its replay, want 2500 EUR", held)
		}
		if held := balance(t, ledger, house, conformEUR); !held.Equal(amount(-2500, conformEUR)) {
			t.Errorf("the house holds %s after one transfer and its replay, want -2500 EUR", held)
		}
	})
}

// TestConformanceReplayWithADifferentTransferConflicts is the other half of
// idempotency: one key naming two different movements is the bug the key
// exists to surface, so every implementation refuses it wrapping
// ErrIdempotencyConflict rather than quietly answering with the first.
func TestConformanceReplayWithADifferentTransferConflicts(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)
		key := "conformance-conflict-" + uuid.NewString()

		if _, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: key,
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformEUR)},
				{Account: member, Amount: amount(2500, conformEUR)},
			},
		}); err != nil {
			t.Fatalf("posting: %v", err)
		}

		_, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: key,
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-9900, conformEUR)},
				{Account: member, Amount: amount(9900, conformEUR)},
			},
		})
		if !errors.Is(err, wallet.ErrIdempotencyConflict) {
			t.Fatalf("posting a different transfer under a used key gave %v, want one wrapping ErrIdempotencyConflict", err)
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(2500, conformEUR)) {
			t.Errorf("the member holds %s after a refused conflict, want the original 2500 EUR", held)
		}
	})
}

// TestConformanceRefusesATransferThatDoesNotSumToZero is C-1 at the port
// boundary: money is conserved, so a transfer whose postings do not net to
// nothing is not a transfer. Every implementation refuses it wrapping
// ErrUnbalanced, before any I/O, and nothing moves.
//
// C-1 is also checked inside the ledger, deliberately twice - but a port
// that let an unbalanced transfer through would be relying on a check the
// substrate is free to weaken, and one of the three keeps its postings on a
// server this repository does not own.
func TestConformanceRefusesATransferThatDoesNotSumToZero(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)

		_, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-unbalanced-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformEUR)},
				{Account: member, Amount: amount(2400, conformEUR)},
			},
		})
		if !errors.Is(err, wallet.ErrUnbalanced) {
			t.Fatalf("posting 100 EUR of money from nowhere gave %v, want one wrapping ErrUnbalanced", err)
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the member holds %s after a refused transfer, want nothing", held)
		}
		if held := balance(t, ledger, house, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the house holds %s after a refused transfer, want nothing", held)
		}
	})
}

// TestConformanceRefusesCurrenciesNettingAgainstEachOther is C-6: two
// currencies never cancel. A transfer whose euros and pounds happen to sum
// to zero across the pair balances in neither, and no implementation may
// convert between them, pick a rate, or record it anyway.
//
// This is the shape a rate would have to sneak in through, so the suite
// asks for it explicitly rather than trusting that nobody would.
func TestConformanceRefusesCurrenciesNettingAgainstEachOther(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		euros := ensure(t, ledger, conformHouse(), conformEUR)
		pounds := ensure(t, ledger, conformHouse(), conformGBP)

		_, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-mixed-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: euros, Amount: amount(-2500, conformEUR)},
				{Account: pounds, Amount: amount(2500, conformGBP)},
			},
		})
		if !errors.Is(err, wallet.ErrMixedCurrency) {
			t.Fatalf("posting euros against pounds gave %v, want one wrapping ErrMixedCurrency", err)
		}

		if held := balance(t, ledger, euros, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the euro account holds %s after a refused transfer, want nothing", held)
		}
		if held := balance(t, ledger, pounds, conformGBP); !held.Equal(amount(0, conformGBP)) {
			t.Errorf("the pound account holds %s after a refused transfer, want nothing", held)
		}
	})
}

// TestConformanceRefusesAPostingInTheWrongCurrency is the other way a rate
// could enter: an account holds one currency by definition, so money of
// another kind has no meaning on it. No implementation may convert the
// amount or record it under the account's own currency instead.
func TestConformanceRefusesAPostingInTheWrongCurrency(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)

		_, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-wrong-currency-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformGBP)},
				{Account: member, Amount: amount(2500, conformGBP)},
			},
		})
		if !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("posting pounds onto euro accounts gave %v, want one wrapping money.ErrCurrencyMismatch", err)
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the member holds %s after a refused transfer, want nothing", held)
		}
	})
}

// TestConformanceConcurrentPostsOfOneKeyRecordOnce is the race the port
// exists to survive. Two retries of one domain fact reach the ledger at the
// same moment - two dispatcher deliveries of one event, a caller and its
// own timeout - and exactly one of them may record. Both must learn the
// same reference, and the money must move once.
//
// A ledger that satisfied the sequential replay test and failed this one
// would look correct in every unit test and double a member's earnings
// under load, which is the failure this suite is for.
func TestConformanceConcurrentPostsOfOneKeyRecordOnce(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)

		transfer := wallet.Transfer{
			IdempotencyKey: "conformance-race-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformEUR)},
				{Account: member, Amount: amount(2500, conformEUR)},
			},
		}

		const callers = 8
		refs := make([]wallet.TransferRef, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				refs[i], errs[i] = ledger.Post(ctx, transfer)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: posting one key from %d callers at once: %v", i, callers, err)
			}
		}
		for i, ref := range refs {
			if ref != refs[0] {
				t.Errorf("caller %d learned transfer %q, caller 0 learned %q; one key recorded two transfers",
					i, ref, refs[0])
			}
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(2500, conformEUR)) {
			t.Errorf("the member holds %s after %d concurrent posts of one key, want 2500 EUR", held, callers)
		}
		if held := balance(t, ledger, house, conformEUR); !held.Equal(amount(-2500, conformEUR)) {
			t.Errorf("the house holds %s after %d concurrent posts of one key, want -2500 EUR", held, callers)
		}
	})
}

// TestConformanceConcurrentEnsureResolvesToOneAccount is the same race one
// level down. Two callers ensuring one account must not end up with two:
// an implementation that minted an id per call would split one member's
// money across them, with nothing left to find it by.
func TestConformanceConcurrentEnsureResolvesToOneAccount(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		ref := conformMember(wallet.StageConfirmed)

		const callers = 8
		ids := make([]wallet.LedgerAccountID, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ids[i], errs[i] = ledger.EnsureAccount(ctx, ref, conformEUR)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: ensuring one account from %d callers at once: %v", i, callers, err)
			}
		}
		for i, id := range ids {
			if id != ids[0] {
				t.Errorf("caller %d resolved the account to %q, caller 0 to %q; one account has two ids",
					i, id, ids[0])
			}
		}
	})
}

// TestConformanceBalanceAfterAReversal is what a clawback has to leave
// behind. An earning is reversed by a second transfer that moves the same
// money back, and every implementation must land the member exactly at
// nothing - not near it.
//
// The reversal is a transfer in its own right with a key of its own, never
// a deletion: the port has no way to unrecord a posting, and a ledger that
// let one be removed could not answer what it did last week. Both postings
// stay, and the balance is their sum.
func TestConformanceBalanceAfterAReversal(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)
		suffix := uuid.NewString()

		if _, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-earning-" + suffix,
			Reference:      "an earning",
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-2500, conformEUR)},
				{Account: member, Amount: amount(2500, conformEUR)},
			},
		}); err != nil {
			t.Fatalf("posting the earning: %v", err)
		}

		if _, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-reversal-" + suffix,
			Reference:      "the reversal of an earning",
			Postings: []wallet.Posting{
				{Account: member, Amount: amount(-2500, conformEUR)},
				{Account: house, Amount: amount(2500, conformEUR)},
			},
		}); err != nil {
			t.Fatalf("posting the reversal: %v", err)
		}

		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the member holds %s after an earning and its reversal, want nothing", held)
		}
		if held := balance(t, ledger, house, conformEUR); !held.Equal(amount(0, conformEUR)) {
			t.Errorf("the house holds %s after an earning and its reversal, want nothing", held)
		}

		// Both postings survive: the reversal is a fact, not an erasure.
		seen := 0
		history, err := ledger.History(ctx, member, wallet.Window{})
		if err != nil {
			t.Fatalf("reading the member's history: %v", err)
		}
		for posting, err := range history {
			if err != nil {
				t.Fatalf("reading the member's history: %v", err)
			}
			seen++
			_ = posting
		}
		if seen != 2 {
			t.Errorf("the member's history holds %d posting(s) after an earning and its reversal, want both", seen)
		}
	})
}

// TestConformanceMemberAccountCannotGoNegative is the double-spend defence
// D9 leans on, and it is a requirement of the port rather than a courtesy:
// a member's stage account may never hold less than nothing, judged at the
// moment the transfer is applied.
//
// A house account is exempt in the same scenario, because it is the
// boundary of the closed set of accounts - the place a commission arrives
// from - and a ledger in which nothing may go negative has nothing able to
// fund the first credit.
func TestConformanceMemberAccountCannotGoNegative(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		ctx := context.Background()
		house := ensure(t, ledger, conformHouse(), conformEUR)
		member := ensure(t, ledger, conformMember(wallet.StageReserved), conformEUR)

		// The house funds the closed set from nothing: this must be allowed.
		if _, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-fund-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-1000, conformEUR)},
				{Account: member, Amount: amount(1000, conformEUR)},
			},
		}); err != nil {
			t.Fatalf("the house could not fund a member from nothing: %v", err)
		}
		if held := balance(t, ledger, house, conformEUR); !held.Equal(amount(-1000, conformEUR)) {
			t.Errorf("the house holds %s after funding, want -1000 EUR; house accounts may go negative", held)
		}

		// The member may not be spent past what it holds.
		_, err := ledger.Post(ctx, wallet.Transfer{
			IdempotencyKey: "conformance-overspend-" + uuid.NewString(),
			Postings: []wallet.Posting{
				{Account: member, Amount: amount(-1001, conformEUR)},
				{Account: house, Amount: amount(1001, conformEUR)},
			},
		})
		if !errors.Is(err, wallet.ErrInsufficientFunds) {
			t.Fatalf("spending a member past its balance gave %v, want one wrapping ErrInsufficientFunds", err)
		}
		if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(1000, conformEUR)) {
			t.Errorf("the member holds %s after a refused overspend, want the 1000 EUR it had", held)
		}
	})
}

// TestConformanceAnInterruptedPostLeavesNoHalfTransfer is the crash
// injection. A Post is cut off part-way - the caller's context expires, the
// process is killed, the connection drops - and what the ledger is left
// holding must still be a ledger: every posting of the transfer or none of
// them, never one side of it.
//
// Half a transfer is the worst state this system can reach. It is money
// created or destroyed, it satisfies no error path, and nothing downstream
// can tell it from a correct balance - C-1 would catch it eventually, but
// only after it had been paid out.
//
// The cut is walked across the whole operation rather than made at one
// point: a clean post is timed first, then each attempt is given a slice of
// that duration, from far too little to more than enough. Somewhere in that
// sweep the deadline lands in the middle of the write, which is the moment
// worth testing and the one a fixed timeout would miss.
//
// After each cut the invariants are checked with a clean context, and then
// the same key is posted again as a retry would. The end state must be
// exactly one transfer: an interrupted attempt that recorded must be found
// by its key, and one that did not must be free to record now.
func TestConformanceAnInterruptedPostLeavesNoHalfTransfer(t *testing.T) {
	t.Parallel()

	eachLedger(t, func(t *testing.T, ledger wallet.Ledger) {
		clean := timeACleanPost(t, ledger)

		// Twelve slices from nothing to a little over a whole post: the
		// interesting deadlines are the ones inside the write.
		const attempts = 12
		for attempt := range attempts {
			budget := clean * time.Duration(attempt) / (attempts - 2)

			house := ensure(t, ledger, conformHouse(), conformEUR)
			member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)
			transfer := wallet.Transfer{
				IdempotencyKey: "conformance-interrupt-" + uuid.NewString(),
				Postings: []wallet.Posting{
					{Account: house, Amount: amount(-2500, conformEUR)},
					{Account: member, Amount: amount(2500, conformEUR)},
				},
			}

			cut, cancel := context.WithTimeout(context.Background(), budget)
			_, postErr := ledger.Post(cut, transfer)
			cancel()

			// Whatever happened, the two sides must agree. This is the
			// assertion the whole scenario exists for.
			held := balance(t, ledger, member, conformEUR)
			funded := balance(t, ledger, house, conformEUR)
			total, err := held.Add(funded)
			if err != nil {
				t.Fatalf("attempt %d (cut at %s): totalling the two accounts: %v", attempt, budget, err)
			}
			if !total.IsZero() {
				t.Fatalf("attempt %d (cut at %s): the member holds %s and the house %s, which sum to %s; half a transfer landed",
					attempt, budget, held, funded, total)
			}
			switch {
			case held.Equal(amount(0, conformEUR)), held.Equal(amount(2500, conformEUR)):
			default:
				t.Fatalf("attempt %d (cut at %s): the member holds %s, want either nothing or the whole 2500 EUR",
					attempt, budget, held)
			}
			if postErr == nil && !held.Equal(amount(2500, conformEUR)) {
				t.Fatalf("attempt %d (cut at %s): Post returned no error but the member holds %s",
					attempt, budget, held)
			}

			// A retry of the same key, as the caller would make it. One
			// transfer must exist afterwards, whichever way the cut fell.
			if _, err := ledger.Post(context.Background(), transfer); err != nil {
				t.Fatalf("attempt %d (cut at %s): retrying the key after an interrupted post: %v", attempt, budget, err)
			}
			if held := balance(t, ledger, member, conformEUR); !held.Equal(amount(2500, conformEUR)) {
				t.Fatalf("attempt %d (cut at %s): the member holds %s after the retry, want exactly 2500 EUR",
					attempt, budget, held)
			}
		}
	})
}

// timeACleanPost measures one uninterrupted Post on throwaway accounts, so
// the crash sweep can be scaled to the implementation in front of it rather
// than to a constant that is far too long for the in-memory ledger and far
// too short for a ledger across a network.
func timeACleanPost(t *testing.T, ledger wallet.Ledger) time.Duration {
	t.Helper()
	ctx := context.Background()
	house := ensure(t, ledger, conformHouse(), conformEUR)
	member := ensure(t, ledger, conformMember(wallet.StagePending), conformEUR)

	started := time.Now()
	if _, err := ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: "conformance-timing-" + uuid.NewString(),
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-100, conformEUR)},
			{Account: member, Amount: amount(100, conformEUR)},
		},
	}); err != nil {
		t.Fatalf("timing a clean post: %v", err)
	}
	took := time.Since(started)
	// A floor, because an in-memory post can finish faster than the clock
	// resolves and every slice of nothing is still nothing.
	if took < time.Millisecond {
		took = time.Millisecond
	}
	return took
}
