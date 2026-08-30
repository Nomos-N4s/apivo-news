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
