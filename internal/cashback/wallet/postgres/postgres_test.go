package postgres_test

// Behaviour tests for the Postgres ledger, mirroring the in-memory
// reference suite case for case: the two implementations sit behind one
// port and must refuse the same mistake with the same error and answer the
// same question with the same shape.
//
// They run against a real Postgres, keyed on DATABASE_URL, exactly like
// the schema invariant suites in internal/platform/db. The database
// outlives a test run and the ledger's rows are immutable by design, so
// nothing here cleans up after itself: every test works with accounts and
// idempotency keys carrying a random suffix, which keeps its money
// separate from every other test's and from every earlier run's.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/postgres"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every per-currency rule; which
// currencies a deployment trades in is configuration, not a property of
// the ledger.
const (
	eur = money.Currency("EUR")
	gbp = money.Currency("GBP")
)

// anchor is the fixed instant injected clocks start from: postings land at
// knowable offsets and windows can be drawn around them exactly. Whole
// seconds on purpose - timestamptz stores microseconds, so a
// microsecond-aligned instant round-trips unchanged.
var anchor = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("DATABASE_URL")
	if url != "" {
		if err := db.Migrate(url); err != nil {
			fmt.Fprintln(os.Stderr, "migrating test database:", err)
			os.Exit(1)
		}
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "parsing test database URL:", err)
			os.Exit(1)
		}
		// The concurrency tests hold many connections at once - a losing
		// Post keeps its connection while it waits out the winner's commit
		// - so the pool gets headroom over pgx's small default, sized from
		// the same knob the platform/db suites use.
		if want := int32(runtime.GOMAXPROCS(0)) + 4; cfg.MaxConns < want {
			cfg.MaxConns = want
		}
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connecting test database:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

// requirePool skips when no database is available, in the same words the
// sibling suites use, and hands back the shared pool otherwise.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise the postgres ledger")
	}
	return testPool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// tickingClock hands out instants one second apart starting at start. It
// locks itself rather than relying on the ledger to serialise calls: how
// the ledger schedules its clock is its own business, not something a test
// should encode.
func tickingClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	next := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at := next
		next = next.Add(time.Second)
		return at
	}
}

// amt builds an amount as a struct literal, the way the port's own tests
// do, so no constructor stands between a test and the value it means.
func amt(minor int64, currency money.Currency) money.Amount {
	return money.Amount{Minor: minor, Currency: currency}
}

// mustEnsure resolves ref in currency, failing the test on any error, so
// setup reads as a list of accounts rather than error plumbing.
func mustEnsure(t *testing.T, l *postgres.Ledger, ref wallet.AccountRef, currency money.Currency) wallet.LedgerAccountID {
	t.Helper()
	id, err := l.EnsureAccount(t.Context(), ref, currency)
	if err != nil {
		t.Fatalf("EnsureAccount(%s, %s) = %v, want nil", ref, currency, err)
	}
	return id
}

// accounts is the cast of the canonical commission split - the source that
// gives, the member bucket and the house remainder that receive (D6) -
// plus the suffix that keeps this test's keys out of every other test's
// way in the shared database.
type accounts struct {
	suffix                string
	source, member, house wallet.LedgerAccountID
}

// key namespaces an idempotency key to this test. Derived, not random per
// call, so a replay in a test really is the same key.
func (a accounts) key(name string) string { return name + "-" + a.suffix }

// newLedger builds a ledger on a ticking clock from anchor with the three
// split accounts ensured - fresh ones for this test alone: a fresh member
// id and suffixed house names mean every balance below starts at zero
// however much money earlier runs left behind.
func newLedger(t *testing.T) (*postgres.Ledger, accounts) {
	t.Helper()
	pool := requirePool(t)
	l := postgres.New(pool, postgres.WithClock(tickingClock(anchor)))
	suffix := randomSuffix(t)
	return l, accounts{
		suffix: suffix,
		source: mustEnsure(t, l, wallet.HouseAccount("network-receivable-"+suffix), eur),
		member: mustEnsure(t, l, wallet.MemberAccount(uuid.New(), wallet.StagePending), eur),
		house:  mustEnsure(t, l, wallet.HouseAccount("rounding-remainder-"+suffix), eur),
	}
}

// split is the canonical commission split under the test's namespaced key:
// the source gives 1000, the member takes 900, the house keeps the
// remainder, and the whole sums to zero (D6).
func split(name string, a accounts) wallet.Transfer {
	return wallet.Transfer{
		IdempotencyKey: a.key(name),
		Postings: []wallet.Posting{
			{Account: a.source, Amount: amt(-1000, eur)},
			{Account: a.member, Amount: amt(900, eur)},
			{Account: a.house, Amount: amt(100, eur)},
		},
		Reference: "entry " + name,
	}
}

// assertBalance asks the ledger for the account's balance in want's
// currency and compares to the minor unit.
func assertBalance(t *testing.T, l *postgres.Ledger, account wallet.LedgerAccountID, want money.Amount) {
	t.Helper()
	got, err := l.Balance(t.Context(), account, want.Currency)
	if err != nil {
		t.Fatalf("Balance(%s) = %v, want nil", account, err)
	}
	if !got.Equal(want) {
		t.Fatalf("Balance(%s) = %s, want %s", account, got, want)
	}
}

func TestEnsureAccount(t *testing.T) {
	t.Parallel()

	t.Run("the same pair resolves to the same id on every call", func(t *testing.T) {
		t.Parallel()

		l := postgres.New(requirePool(t))
		ref := wallet.MemberAccount(uuid.New(), wallet.StageConfirmed)
		first := mustEnsure(t, l, ref, eur)
		second := mustEnsure(t, l, ref, eur)
		if first == "" {
			t.Fatal("EnsureAccount issued a blank id")
		}
		if first != second {
			t.Fatalf("EnsureAccount issued two ids for one account: %q and %q", first, second)
		}
	})

	t.Run("one ref is one account per currency", func(t *testing.T) {
		t.Parallel()

		// A member holding two currencies holds two accounts (C-6), and
		// no member account is ever a house account or another stage's.
		l := postgres.New(requirePool(t))
		memberID := uuid.New()
		ids := []wallet.LedgerAccountID{
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), eur),
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), gbp),
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur),
			mustEnsure(t, l, wallet.HouseAccount("rounding-remainder-"+randomSuffix(t)), eur),
		}
		seen := make(map[wallet.LedgerAccountID]bool, len(ids))
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("two distinct accounts share the id %q", id)
			}
			seen[id] = true
		}
	})

	t.Run("an unusable ref is refused", func(t *testing.T) {
		t.Parallel()

		l := postgres.New(requirePool(t))
		if _, err := l.EnsureAccount(t.Context(), wallet.AccountRef{}, eur); !errors.Is(err, wallet.ErrInvalidAccountRef) {
			t.Fatalf("EnsureAccount(zero ref) = %v, want an error wrapping %v", err, wallet.ErrInvalidAccountRef)
		}
	})

	t.Run("a malformed currency is refused", func(t *testing.T) {
		t.Parallel()

		l := postgres.New(requirePool(t))
		ref := wallet.MemberAccount(uuid.New(), wallet.StagePending)
		if _, err := l.EnsureAccount(t.Context(), ref, "eur"); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Fatalf("EnsureAccount(lowercase currency) = %v, want an error wrapping %v", err, money.ErrInvalidCurrency)
		}
	})
}

// TestTwoAdaptersOverOneDatabaseAreOneLedger is the property the in-memory
// implementation cannot have and this one exists for: the state lives in
// the schema, so a second adapter - a second process, after a restart -
// sees the same accounts and the same money.
func TestTwoAdaptersOverOneDatabaseAreOneLedger(t *testing.T) {
	t.Parallel()

	first, a := newLedger(t)
	if _, err := first.Post(t.Context(), split("entry-1", a)); err != nil {
		t.Fatalf("Post() = %v, want nil", err)
	}

	second := postgres.New(requirePool(t))
	assertBalance(t, second, a.member, amt(900, eur))
	if _, err := second.Post(t.Context(), split("entry-1", a)); err != nil {
		t.Fatalf("Post(replay through a second adapter) = %v, want nil", err)
	}
	// The replay was a replay, not a second recording.
	assertBalance(t, first, a.member, amt(900, eur))
}

func TestPostMovesEveryBalanceOrNone(t *testing.T) {
	t.Parallel()

	t.Run("a recorded split moves all three balances", func(t *testing.T) {
		t.Parallel()

		l, a := newLedger(t)
		ref, err := l.Post(t.Context(), split("entry-1", a))
		if err != nil {
			t.Fatalf("Post() = %v, want nil", err)
		}
		if ref == "" {
			t.Fatal("Post() returned a blank reference; the domain stores it, so it must never be blank")
		}
		assertBalance(t, l, a.source, amt(-1000, eur))
		assertBalance(t, l, a.member, amt(900, eur))
		assertBalance(t, l, a.house, amt(100, eur))
	})

	t.Run("an unknown account refuses the whole transfer", func(t *testing.T) {
		t.Parallel()

		// The bad posting is deliberately last: all-or-nothing means the
		// two good postings before it must not survive.
		l, a := newLedger(t)
		transfer := split("entry-1", a)
		transfer.Postings[2].Account = "never-issued"
		if _, err := l.Post(t.Context(), transfer); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Fatalf("Post() = %v, want an error wrapping %v", err, wallet.ErrUnknownAccount)
		}
		assertBalance(t, l, a.source, amt(0, eur))
		assertBalance(t, l, a.member, amt(0, eur))
	})

	t.Run("a posting in the wrong currency refuses the whole transfer", func(t *testing.T) {
		t.Parallel()

		// Balanced in GBP, but every account here holds EUR: an account
		// is one currency by definition, so this transfer has nowhere
		// legal to land.
		l, a := newLedger(t)
		transfer := wallet.Transfer{
			IdempotencyKey: a.key("entry-1"),
			Postings: []wallet.Posting{
				{Account: a.source, Amount: amt(-100, gbp)},
				{Account: a.member, Amount: amt(100, gbp)},
			},
		}
		if _, err := l.Post(t.Context(), transfer); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Post() = %v, want an error wrapping %v", err, money.ErrCurrencyMismatch)
		}
		assertBalance(t, l, a.source, amt(0, eur))
		assertBalance(t, l, a.member, amt(0, eur))
	})

	t.Run("a refused transfer does not consume its key", func(t *testing.T) {
		t.Parallel()

		// The retry after a refusal is the same domain fact under the
		// same derived key (D8); a refusal that burned the key would turn
		// every corrected retry into a conflict.
		l, a := newLedger(t)
		broken := split("entry-1", a)
		broken.Postings[2].Account = "never-issued"
		if _, err := l.Post(t.Context(), broken); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Fatalf("Post(broken) = %v, want an error wrapping %v", err, wallet.ErrUnknownAccount)
		}
		if _, err := l.Post(t.Context(), split("entry-1", a)); err != nil {
			t.Fatalf("Post(corrected retry) = %v, want nil", err)
		}
		assertBalance(t, l, a.member, amt(900, eur))
	})
}

func TestPostValidatesBeforeAnythingElse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// breaks takes the canonical split and breaks exactly one rule.
		breaks  func(wallet.Transfer) wallet.Transfer
		wantErr error
	}{
		{
			name: "a missing idempotency key",
			breaks: func(tr wallet.Transfer) wallet.Transfer {
				tr.IdempotencyKey = ""
				return tr
			},
			wantErr: wallet.ErrMissingIdempotencyKey,
		},
		{
			name: "postings that do not sum to zero",
			breaks: func(tr wallet.Transfer) wallet.Transfer {
				tr.Postings = tr.Postings[:2]
				return tr
			},
			wantErr: wallet.ErrUnbalanced,
		},
		{
			name: "one currency netted against another",
			breaks: func(tr wallet.Transfer) wallet.Transfer {
				tr.Postings[1].Amount = amt(900, gbp)
				tr.Postings[2].Amount = amt(100, eur)
				tr.Postings[0].Amount = amt(-1000, eur)
				return tr
			},
			wantErr: wallet.ErrMixedCurrency,
		},
		{
			name: "a posting recycled from history",
			breaks: func(tr wallet.Transfer) wallet.Transfer {
				tr.Postings[1].TransferRef = "transfer-1"
				return tr
			},
			wantErr: wallet.ErrRecycledPosting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, a := newLedger(t)
			if _, err := l.Post(t.Context(), tc.breaks(split("entry-1", a))); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Post() = %v, want an error wrapping %v", err, tc.wantErr)
			}
			// Nothing was applied: the refusal happened before any state.
			assertBalance(t, l, a.source, amt(0, eur))
			assertBalance(t, l, a.member, amt(0, eur))
			assertBalance(t, l, a.house, amt(0, eur))
		})
	}

	t.Run("validation outranks the idempotency lookup", func(t *testing.T) {
		t.Parallel()

		// A broken transfer under a recorded key is refused for being
		// broken, not answered with the recorded reference and not
		// reported as a conflict: Validate runs first, always.
		l, a := newLedger(t)
		if _, err := l.Post(t.Context(), split("entry-1", a)); err != nil {
			t.Fatalf("Post(original) = %v, want nil", err)
		}
		broken := split("entry-1", a)
		broken.Postings = broken.Postings[:2]
		if _, err := l.Post(t.Context(), broken); !errors.Is(err, wallet.ErrUnbalanced) {
			t.Fatalf("Post(broken replay) = %v, want an error wrapping %v", err, wallet.ErrUnbalanced)
		}
	})
}

func TestPostIdempotency(t *testing.T) {
	t.Parallel()

	// Every case posts the canonical split first, then a second transfer
	// under the same key. A replay must return the original reference
	// with balances moved exactly once; a conflict must move nothing.
	tests := []struct {
		name string
		// second derives the second transfer from the accounts; the base
		// key is reused unless the case says otherwise.
		second func(accounts) wallet.Transfer
		// wantConflict true means the second Post must be refused
		// wrapping ErrIdempotencyConflict; false means a true replay.
		wantConflict bool
	}{
		{
			name:   "an identical replay",
			second: func(a accounts) wallet.Transfer { return split("entry-1", a) },
		},
		{
			name: "a replay with its postings reordered",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Postings[0], tr.Postings[2] = tr.Postings[2], tr.Postings[0]
				return tr
			},
		},
		{
			name: "a replay whose nil metadata became an empty map",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Metadata = map[string]string{}
				return tr
			},
		},
		{
			name: "the same key over different amounts",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Postings[1].Amount = amt(800, eur)
				tr.Postings[2].Amount = amt(200, eur)
				return tr
			},
			wantConflict: true,
		},
		{
			name: "the same key over different accounts",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Postings[1].Account, tr.Postings[2].Account = tr.Postings[2].Account, tr.Postings[1].Account
				return tr
			},
			wantConflict: true,
		},
		{
			name: "the same key under a different reference",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Reference = "entry somewhere-else"
				return tr
			},
			wantConflict: true,
		},
		{
			name: "the same key under different metadata",
			second: func(a accounts) wallet.Transfer {
				tr := split("entry-1", a)
				tr.Metadata = map[string]string{"origin": "backfill"}
				return tr
			},
			wantConflict: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, a := newLedger(t)
			original, err := l.Post(t.Context(), split("entry-1", a))
			if err != nil {
				t.Fatalf("Post(original) = %v, want nil", err)
			}

			ref, err := l.Post(t.Context(), tc.second(a))
			if tc.wantConflict {
				if !errors.Is(err, wallet.ErrIdempotencyConflict) {
					t.Fatalf("Post(second) = %v, want an error wrapping %v", err, wallet.ErrIdempotencyConflict)
				}
			} else {
				if err != nil {
					t.Fatalf("Post(replay) = %v, want nil", err)
				}
				if ref != original {
					t.Fatalf("Post(replay) = %q, want the original reference %q", ref, original)
				}
			}
			// Replay or conflict, the money moved exactly once.
			assertBalance(t, l, a.source, amt(-1000, eur))
			assertBalance(t, l, a.member, amt(900, eur))
			assertBalance(t, l, a.house, amt(100, eur))
		})
	}

	t.Run("keys are compared byte for byte", func(t *testing.T) {
		t.Parallel()

		// "entry-1" and "entry-1 " are two keys, so the same postings
		// under them are two transfers, each recorded.
		l, a := newLedger(t)
		first, err := l.Post(t.Context(), split("entry-1", a))
		if err != nil {
			t.Fatalf("Post(first) = %v, want nil", err)
		}
		trailing := split("entry-1", a)
		trailing.IdempotencyKey += " "
		second, err := l.Post(t.Context(), trailing)
		if err != nil {
			t.Fatalf("Post(second) = %v, want nil", err)
		}
		if first == second {
			t.Fatalf("two distinct keys share the reference %q", first)
		}
		assertBalance(t, l, a.member, amt(1800, eur))
	})

	t.Run("a conflict leaves the recorded transfer standing", func(t *testing.T) {
		t.Parallel()

		l, a := newLedger(t)
		original, err := l.Post(t.Context(), split("entry-1", a))
		if err != nil {
			t.Fatalf("Post(original) = %v, want nil", err)
		}
		imposter := split("entry-1", a)
		imposter.Reference = "entry imposter"
		if _, err := l.Post(t.Context(), imposter); !errors.Is(err, wallet.ErrIdempotencyConflict) {
			t.Fatalf("Post(imposter) = %v, want an error wrapping %v", err, wallet.ErrIdempotencyConflict)
		}
		// The true replay still answers with the original reference: a
		// collision must not corrupt the identity it collided with.
		ref, err := l.Post(t.Context(), split("entry-1", a))
		if err != nil || ref != original {
			t.Fatalf("Post(replay after conflict) = (%q, %v), want (%q, nil)", ref, err, original)
		}
	})

	t.Run("replay detection survives the caller mutating its transfer", func(t *testing.T) {
		t.Parallel()

		// The ledger recorded its own copy of the annotations: a caller
		// reusing its metadata map for the next transfer is ordinary Go,
		// and it must not rewrite what the key remembers.
		l, a := newLedger(t)
		metadata := map[string]string{"origin": "network"}
		first := split("entry-1", a)
		first.Metadata = metadata
		original, err := l.Post(t.Context(), first)
		if err != nil {
			t.Fatalf("Post(original) = %v, want nil", err)
		}
		metadata["origin"] = "backfill"

		replay := split("entry-1", a)
		replay.Metadata = map[string]string{"origin": "network"}
		ref, err := l.Post(t.Context(), replay)
		if err != nil || ref != original {
			t.Fatalf("Post(replay) = (%q, %v), want (%q, nil)", ref, err, original)
		}
	})
}

func TestPostConcurrentSameKey(t *testing.T) {
	t.Parallel()

	// The port's concurrency promise, verbatim: of many concurrent posts
	// of one key exactly one records, all learning the same reference.
	// Here the race is real - separate connections, resolved by the
	// unique index, the losers waiting out the winner's commit - and
	// under -race it is also the data-race check on the adapter itself.
	const workers = 32

	l, a := newLedger(t)
	refs := make([]wallet.TransferRef, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			refs[i], errs[i] = l.Post(t.Context(), split("contested", a))
		}()
	}
	close(start)
	wg.Wait()

	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("Post() in goroutine %d = %v, want nil", i, errs[i])
		}
		if refs[i] != refs[0] {
			t.Fatalf("goroutine %d learned reference %q, goroutine 0 learned %q; every caller must learn the same one", i, refs[i], refs[0])
		}
	}
	// One winner: the balances moved once, not thirty-two times.
	assertBalance(t, l, a.source, amt(-1000, eur))
	assertBalance(t, l, a.member, amt(900, eur))
	assertBalance(t, l, a.house, amt(100, eur))
}

func TestConcurrentDistinctWork(t *testing.T) {
	t.Parallel()

	t.Run("distinct keys all record", func(t *testing.T) {
		t.Parallel()

		const workers = 16
		l, a := newLedger(t)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = l.Post(t.Context(), split("entry-"+strconv.Itoa(i), a))
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("Post() in goroutine %d = %v, want nil", i, err)
			}
		}
		assertBalance(t, l, a.member, amt(900*workers, eur))
	})

	t.Run("racing EnsureAccount calls agree on one id", func(t *testing.T) {
		t.Parallel()

		const workers = 16
		l := postgres.New(requirePool(t))
		ref := wallet.MemberAccount(uuid.New(), wallet.StageConfirmed)
		ids := make([]wallet.LedgerAccountID, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ids[i], errs[i] = l.EnsureAccount(t.Context(), ref, eur)
			}()
		}
		wg.Wait()

		for i := range workers {
			if errs[i] != nil {
				t.Fatalf("EnsureAccount() in goroutine %d = %v, want nil", i, errs[i])
			}
			if ids[i] != ids[0] {
				t.Fatalf("goroutine %d resolved %q, goroutine 0 resolved %q; one account has one id", i, ids[i], ids[0])
			}
		}
	})

	t.Run("readers and writers share the ledger without racing", func(t *testing.T) {
		t.Parallel()

		// No assertion beyond "no error": this subtest exists for the
		// race detector and for the connection pool - readers streaming
		// history while writers commit must starve nobody.
		const writers, readers, posts = 2, 2, 4
		l, a := newLedger(t)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for p := range posts {
					key := "entry-" + strconv.Itoa(w) + "-" + strconv.Itoa(p)
					if _, err := l.Post(t.Context(), split(key, a)); err != nil {
						t.Errorf("Post(%s) = %v, want nil", key, err)
					}
				}
			}()
		}
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range posts {
					seq, err := l.History(t.Context(), a.member, wallet.Window{})
					if err != nil {
						t.Errorf("History() = %v, want nil", err)
						return
					}
					for _, err := range seq {
						if err != nil {
							t.Errorf("History yielded %v, want nil", err)
							return
						}
					}
					if _, err := l.Balance(t.Context(), a.member, eur); err != nil {
						t.Errorf("Balance() = %v, want nil", err)
					}
				}
			}()
		}
		wg.Wait()
	})
}

func TestBalance(t *testing.T) {
	t.Parallel()

	t.Run("an account with no postings is zero in its currency", func(t *testing.T) {
		t.Parallel()

		l, a := newLedger(t)
		assertBalance(t, l, a.member, amt(0, eur))
	})

	t.Run("the wrong currency is refused, not converted", func(t *testing.T) {
		t.Parallel()

		l, a := newLedger(t)
		if _, err := l.Balance(t.Context(), a.member, gbp); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Balance(GBP on a EUR account) = %v, want an error wrapping %v", err, money.ErrCurrencyMismatch)
		}
	})

	t.Run("an id the ledger never issued is refused", func(t *testing.T) {
		t.Parallel()

		l, _ := newLedger(t)
		if _, err := l.Balance(t.Context(), "never-issued", eur); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Fatalf("Balance(unknown) = %v, want an error wrapping %v", err, wallet.ErrUnknownAccount)
		}
	})

	t.Run("a balance is the sum of every posting", func(t *testing.T) {
		t.Parallel()

		l, a := newLedger(t)
		for _, key := range []string{"entry-1", "entry-2", "entry-3"} {
			if _, err := l.Post(t.Context(), split(key, a)); err != nil {
				t.Fatalf("Post(%s) = %v, want nil", key, err)
			}
		}
		assertBalance(t, l, a.member, amt(2700, eur))
		assertBalance(t, l, a.source, amt(-3000, eur))
	})
}

func TestBalanceAfterReversal(t *testing.T) {
	t.Parallel()

	l, a := newLedger(t)
	original := split("entry-1", a)
	if _, err := l.Post(t.Context(), original); err != nil {
		t.Fatalf("Post(original) = %v, want nil", err)
	}

	// The reversal is rebuilt from the original with every amount flipped
	// through Neg, under its own key - never copied out of History, whose
	// postings carry provenance the port refuses.
	reversal := wallet.Transfer{
		IdempotencyKey: a.key("entry-1-reversal"),
		Postings:       make([]wallet.Posting, len(original.Postings)),
		Reference:      "reversal of entry entry-1",
	}
	for i, p := range original.Postings {
		flipped, err := p.Amount.Neg()
		if err != nil {
			t.Fatalf("Neg(%s) = %v, want nil", p.Amount, err)
		}
		reversal.Postings[i] = wallet.Posting{Account: p.Account, Amount: flipped}
	}
	if _, err := l.Post(t.Context(), reversal); err != nil {
		t.Fatalf("Post(reversal) = %v, want nil", err)
	}

	// Every balance is back where it started, and the history still shows
	// both movements: reversal is a new fact, not an erasure.
	assertBalance(t, l, a.source, amt(0, eur))
	assertBalance(t, l, a.member, amt(0, eur))
	assertBalance(t, l, a.house, amt(0, eur))
	if got := collectHistory(t, l, a.member, wallet.Window{}); len(got) != 2 {
		t.Fatalf("History after reversal holds %d postings, want 2: the reversal must not erase what it reverses", len(got))
	}

	// The copy-paste reversal the port exists to refuse: a posting taken
	// from History still carries its provenance.
	fromHistory := collectHistory(t, l, a.member, wallet.Window{})[0]
	recycled := wallet.Transfer{
		IdempotencyKey: a.key("entry-1-recycled"),
		Postings:       []wallet.Posting{fromHistory, {Account: a.source, Amount: amt(-900, eur)}},
	}
	if _, err := l.Post(t.Context(), recycled); !errors.Is(err, wallet.ErrRecycledPosting) {
		t.Fatalf("Post(recycled) = %v, want an error wrapping %v", err, wallet.ErrRecycledPosting)
	}
}

// collectHistory drains a History stream into a slice, failing the test on
// any error, immediate or yielded.
func collectHistory(t *testing.T, l *postgres.Ledger, account wallet.LedgerAccountID, window wallet.Window) []wallet.Posting {
	t.Helper()
	seq, err := l.History(t.Context(), account, window)
	if err != nil {
		t.Fatalf("History() = %v, want nil", err)
	}
	var got []wallet.Posting
	for p, err := range seq {
		if err != nil {
			t.Fatalf("History yielded %v, want nil", err)
		}
		got = append(got, p)
	}
	return got
}

func TestHistory(t *testing.T) {
	t.Parallel()

	// seed posts three splits at anchor, anchor+1s and anchor+2s - the
	// ticking clock hands each Post exactly one instant - and returns the
	// references in posting order.
	seed := func(t *testing.T) (*postgres.Ledger, accounts, []wallet.TransferRef) {
		t.Helper()
		l, a := newLedger(t)
		refs := make([]wallet.TransferRef, 3)
		for i, key := range []string{"entry-1", "entry-2", "entry-3"} {
			ref, err := l.Post(t.Context(), split(key, a))
			if err != nil {
				t.Fatalf("Post(%s) = %v, want nil", key, err)
			}
			refs[i] = ref
		}
		return l, a, refs
	}

	t.Run("postings carry the provenance the ledger recorded", func(t *testing.T) {
		t.Parallel()

		l, a, refs := seed(t)
		got := collectHistory(t, l, a.member, wallet.Window{})
		if len(got) != 3 {
			t.Fatalf("History yielded %d postings, want 3", len(got))
		}
		for i, p := range got {
			want := wallet.Posting{
				Account:     a.member,
				Amount:      amt(900, eur),
				TransferRef: refs[i],
				PostedAt:    anchor.Add(time.Duration(i) * time.Second),
			}
			if p.Account != want.Account || !p.Amount.Equal(want.Amount) || p.TransferRef != want.TransferRef || !p.PostedAt.Equal(want.PostedAt) {
				t.Errorf("posting %d = %+v, want %+v", i+1, p, want)
			}
		}
	})

	t.Run("windows select by Contains", func(t *testing.T) {
		t.Parallel()

		second := anchor.Add(time.Second)
		tests := []struct {
			name   string
			window wallet.Window
			// want holds the PostedAt offsets, in seconds from anchor, of
			// the postings the window must yield, in order.
			want []int
		}{
			{name: "the zero window is the whole of history", window: wallet.Window{}, want: []int{0, 1, 2}},
			{name: "From is inclusive, To exclusive", window: wallet.Window{From: second, To: anchor.Add(2 * time.Second)}, want: []int{1}},
			{name: "a zero From starts at the first posting", window: wallet.Window{To: second}, want: []int{0}},
			{name: "a zero To never ends", window: wallet.Window{From: second}, want: []int{1, 2}},
			{name: "an empty but ordered window yields nothing", window: wallet.Window{From: second, To: second}, want: nil},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				l, a, _ := seed(t)
				got := collectHistory(t, l, a.member, tc.window)
				if len(got) != len(tc.want) {
					t.Fatalf("History yielded %d postings, want %d", len(got), len(tc.want))
				}
				for i, p := range got {
					if want := anchor.Add(time.Duration(tc.want[i]) * time.Second); !p.PostedAt.Equal(want) {
						t.Errorf("posting %d recorded at %s, want %s", i+1, p.PostedAt.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
					}
				}
			})
		}
	})

	t.Run("swapped bounds are refused up front", func(t *testing.T) {
		t.Parallel()

		l, a, _ := seed(t)
		window := wallet.Window{From: anchor.Add(time.Hour), To: anchor}
		if _, err := l.History(t.Context(), a.member, window); !errors.Is(err, wallet.ErrInvalidWindow) {
			t.Fatalf("History(swapped bounds) = %v, want an error wrapping %v", err, wallet.ErrInvalidWindow)
		}
	})

	t.Run("an id the ledger never issued is refused up front", func(t *testing.T) {
		t.Parallel()

		l, _, _ := seed(t)
		if _, err := l.History(t.Context(), "never-issued", wallet.Window{}); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Fatalf("History(unknown) = %v, want an error wrapping %v", err, wallet.ErrUnknownAccount)
		}
	})

	t.Run("a frozen clock still yields recording order", func(t *testing.T) {
		t.Parallel()

		// Every posting lands at one instant, so ascending PostedAt says
		// nothing - the tie-break must be the order the ledger recorded,
		// which in this schema is the posting id.
		l := postgres.New(requirePool(t), postgres.WithClock(func() time.Time { return anchor }))
		suffix := randomSuffix(t)
		source := mustEnsure(t, l, wallet.HouseAccount("network-receivable-"+suffix), eur)
		member := mustEnsure(t, l, wallet.MemberAccount(uuid.New(), wallet.StagePending), eur)
		refs := make([]wallet.TransferRef, 3)
		for i, key := range []string{"entry-1", "entry-2", "entry-3"} {
			ref, err := l.Post(t.Context(), wallet.Transfer{
				IdempotencyKey: key + "-" + suffix,
				Postings: []wallet.Posting{
					{Account: source, Amount: amt(-100, eur)},
					{Account: member, Amount: amt(100, eur)},
				},
			})
			if err != nil {
				t.Fatalf("Post(%s) = %v, want nil", key, err)
			}
			refs[i] = ref
		}
		got := collectHistory(t, l, member, wallet.Window{})
		if len(got) != 3 {
			t.Fatalf("History yielded %d postings, want 3", len(got))
		}
		for i, p := range got {
			if p.TransferRef != refs[i] {
				t.Errorf("posting %d belongs to %q, want %q", i+1, p.TransferRef, refs[i])
			}
		}
	})

	t.Run("a consumer may post from inside its own loop", func(t *testing.T) {
		t.Parallel()

		// Both halves of the streaming promise: no deadlock when the loop
		// body posts (the iteration holds one pooled connection, the Post
		// takes another), and the in-flight iteration keeps answering
		// from its own statement snapshot while a fresh one sees the new
		// posting.
		l, a, _ := seed(t)
		seq, err := l.History(t.Context(), a.member, wallet.Window{})
		if err != nil {
			t.Fatalf("History() = %v, want nil", err)
		}
		seen := 0
		for _, err := range seq {
			if err != nil {
				t.Fatalf("History yielded %v, want nil", err)
			}
			seen++
			if seen == 1 {
				if _, err := l.Post(t.Context(), split("mid-iteration", a)); err != nil {
					t.Fatalf("Post(mid-iteration) = %v, want nil", err)
				}
			}
		}
		if seen != 3 {
			t.Fatalf("the in-flight iteration yielded %d postings, want the 3 that existed when it started", seen)
		}
		if got := collectHistory(t, l, a.member, wallet.Window{}); len(got) != 4 {
			t.Fatalf("a fresh History yielded %d postings, want 4", len(got))
		}
	})

	t.Run("a consumer may stop early, repeatedly, without draining the pool", func(t *testing.T) {
		t.Parallel()

		// Stopping early must release the connection the stream holds.
		// One leak would be invisible, so the loop breaks out of more
		// streams than the pool has connections: if any were leaked, the
		// later iterations - or the Balance after them - would hang on an
		// exhausted pool rather than answer.
		l, a, _ := seed(t)
		rounds := int(requirePool(t).Config().MaxConns) + 2
		for range rounds {
			seq, err := l.History(t.Context(), a.member, wallet.Window{})
			if err != nil {
				t.Fatalf("History() = %v, want nil", err)
			}
			for _, err := range seq {
				if err != nil {
					t.Fatalf("History yielded %v, want nil", err)
				}
				break
			}
		}
		assertBalance(t, l, a.member, amt(2700, eur))
	})

	t.Run("cancellation mid-stream is yielded, then the stream ends", func(t *testing.T) {
		t.Parallel()

		l, a, _ := seed(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		seq, err := l.History(ctx, a.member, wallet.Window{})
		if err != nil {
			t.Fatalf("History() = %v, want nil", err)
		}
		var yielded []error
		seen := 0
		for _, err := range seq {
			if err != nil {
				yielded = append(yielded, err)
				continue
			}
			seen++
			cancel()
		}
		if seen != 1 {
			t.Fatalf("iteration yielded %d postings after cancellation, want 1", seen)
		}
		if len(yielded) != 1 || !errors.Is(yielded[0], context.Canceled) {
			t.Fatalf("iteration yielded errors %v, want exactly one wrapping %v", yielded, context.Canceled)
		}
	})
}

func TestCancelledContextIsRefused(t *testing.T) {
	t.Parallel()

	// Post in particular must consume nothing on a cancelled context: the
	// retry with a live context is the same key.
	l, a := newLedger(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := l.EnsureAccount(ctx, wallet.MemberAccount(uuid.New(), wallet.StageConfirmed), eur); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureAccount(cancelled) = %v, want an error wrapping %v", err, context.Canceled)
	}
	if _, err := l.Post(ctx, split("entry-1", a)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Post(cancelled) = %v, want an error wrapping %v", err, context.Canceled)
	}
	if _, err := l.Balance(ctx, a.member, eur); !errors.Is(err, context.Canceled) {
		t.Fatalf("Balance(cancelled) = %v, want an error wrapping %v", err, context.Canceled)
	}
	if _, err := l.History(ctx, a.member, wallet.Window{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("History(cancelled) = %v, want an error wrapping %v", err, context.Canceled)
	}

	// The refused Post consumed neither money nor the key.
	assertBalance(t, l, a.member, amt(0, eur))
	ref, err := l.Post(t.Context(), split("entry-1", a))
	if err != nil {
		t.Fatalf("Post(retry with a live context) = %v, want nil", err)
	}
	if ref == "" {
		t.Fatal("Post(retry with a live context) returned a blank reference")
	}
	assertBalance(t, l, a.member, amt(900, eur))
}
