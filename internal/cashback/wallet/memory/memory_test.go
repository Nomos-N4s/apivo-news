package memory_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every per-currency rule; which
// currencies a deployment trades in is configuration, not a property of
// the ledger.
const (
	eur = money.Currency("EUR")
	gbp = money.Currency("GBP")
)

// memberID is a fixed member identity, so a failing test prints the same
// account every run.
var memberID = uuid.MustParse("2b7e1516-28ae-d2a6-abf7-158809cf4f3c")

// anchor is the fixed instant injected clocks start from: postings land
// at knowable offsets and windows can be drawn around them exactly.
var anchor = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// tickingClock hands out instants one second apart starting at start. It
// locks itself rather than relying on the ledger to serialise calls: how
// the ledger schedules its clock is its own business, not something a
// test should encode.
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
func mustEnsure(t *testing.T, l *memory.Ledger, ref wallet.AccountRef, currency money.Currency) wallet.LedgerAccountID {
	t.Helper()
	id, err := l.EnsureAccount(t.Context(), ref, currency)
	if err != nil {
		t.Fatalf("EnsureAccount(%s, %s) = %v, want nil", ref, currency, err)
	}
	return id
}

// accounts is the cast of the canonical commission split: the source that
// gives, the member bucket and the house remainder that receive (D6).
type accounts struct {
	source, member, house wallet.LedgerAccountID
}

// newLedger builds a ledger on a ticking clock from anchor with the three
// split accounts ensured, which is the setup nearly every test below
// starts from.
func newLedger(t *testing.T) (*memory.Ledger, accounts) {
	t.Helper()
	l := memory.New(memory.WithClock(tickingClock(anchor)))
	return l, accounts{
		source: mustEnsure(t, l, wallet.HouseAccount("network-receivable"), eur),
		member: mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), eur),
		house:  mustEnsure(t, l, wallet.HouseAccount("rounding-remainder"), eur),
	}
}

// split is the canonical commission split under key: the source gives
// 1000, the member takes 900, the house keeps the remainder, and the
// whole sums to zero (D6).
func split(key string, a accounts) wallet.Transfer {
	return wallet.Transfer{
		IdempotencyKey: key,
		Postings: []wallet.Posting{
			{Account: a.source, Amount: amt(-1000, eur)},
			{Account: a.member, Amount: amt(900, eur)},
			{Account: a.house, Amount: amt(100, eur)},
		},
		Reference: "entry " + key,
	}
}

// assertBalance asks the ledger for the account's balance in want's
// currency and compares to the minor unit.
func assertBalance(t *testing.T, l *memory.Ledger, account wallet.LedgerAccountID, want money.Amount) {
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

		l := memory.New()
		ref := wallet.MemberAccount(memberID, wallet.StageConfirmed)
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
		l := memory.New()
		ids := []wallet.LedgerAccountID{
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), eur),
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), gbp),
			mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur),
			mustEnsure(t, l, wallet.HouseAccount("rounding-remainder"), eur),
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

		l := memory.New()
		if _, err := l.EnsureAccount(t.Context(), wallet.AccountRef{}, eur); !errors.Is(err, wallet.ErrInvalidAccountRef) {
			t.Fatalf("EnsureAccount(zero ref) = %v, want an error wrapping %v", err, wallet.ErrInvalidAccountRef)
		}
	})

	t.Run("a malformed currency is refused", func(t *testing.T) {
		t.Parallel()

		l := memory.New()
		ref := wallet.MemberAccount(memberID, wallet.StagePending)
		if _, err := l.EnsureAccount(t.Context(), ref, "eur"); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Fatalf("EnsureAccount(lowercase currency) = %v, want an error wrapping %v", err, money.ErrInvalidCurrency)
		}
	})
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
			IdempotencyKey: "entry-1",
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

// stages is the cast the withdrawal reservation needs: the house account
// money arrives over, the member's confirmed bucket and the reserved
// bucket a withdrawal request moves it into (D9).
type stages struct {
	house, confirmed, reserved wallet.LedgerAccountID
}

// confirmedBalance is what a member holds in the confirmed bucket before
// each solvency test makes its own move: enough to be drawn on, exactly
// enough to be drained, and one minor unit short of what would overdraw
// it. One figure, shared, so every case below reads against the same
// balance.
const confirmedBalance = 1000

// funded builds a ledger whose member holds exactly confirmedBalance in
// the confirmed bucket, credited from a house account the way money
// really enters the closed set of accounts - which leaves the house
// account at -confirmedBalance, and is already the exemption at work.
func funded(t *testing.T) (*memory.Ledger, stages) {
	t.Helper()
	l := memory.New(memory.WithClock(tickingClock(anchor)))
	s := stages{
		house:     mustEnsure(t, l, wallet.HouseAccount("network-receivable"), eur),
		confirmed: mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur),
		reserved:  mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StageReserved), eur),
	}
	credit := wallet.Transfer{
		IdempotencyKey: "funding",
		Postings: []wallet.Posting{
			{Account: s.house, Amount: amt(-confirmedBalance, eur)},
			{Account: s.confirmed, Amount: amt(confirmedBalance, eur)},
		},
		Reference: "confirmed earnings",
	}
	if _, err := l.Post(t.Context(), credit); err != nil {
		t.Fatalf("Post(funding) = %v, want nil", err)
	}
	return l, s
}

// reserve is the withdrawal reservation itself: minor leaves the member's
// confirmed bucket for their reserved one (D9).
func reserve(key string, s stages, minor int64) wallet.Transfer {
	return wallet.Transfer{
		IdempotencyKey: key,
		Postings: []wallet.Posting{
			{Account: s.confirmed, Amount: amt(-minor, eur)},
			{Account: s.reserved, Amount: amt(minor, eur)},
		},
		Reference: "withdrawal request " + key,
	}
}

// TestPostNeverLeavesAMemberNegative is the port's rule 6: a member's
// stage account may not be left holding less than nothing, house accounts
// are exempt, and the judgement is on what the transfer leaves behind.
func TestPostNeverLeavesAMemberNegative(t *testing.T) {
	t.Parallel()

	// Every case starts from the funded ledger: the member holds
	// confirmedBalance and the house account, having funded it, sits at
	// the negative of the same figure.
	const held = confirmedBalance

	tests := []struct {
		name string
		// transfer is the movement under test, built over the funded
		// accounts.
		transfer func(stages) wallet.Transfer
		// wantErr is nil when the transfer must record.
		wantErr error
		// The three balances afterwards, in minor units.
		wantHouse, wantConfirmed, wantReserved int64
	}{
		{
			name:          "a reservation inside the balance is recorded",
			transfer:      func(s stages) wallet.Transfer { return reserve("wd-1", s, 400) },
			wantHouse:     -held,
			wantConfirmed: 600,
			wantReserved:  400,
		},
		{
			name:          "a reservation for the whole balance is recorded",
			transfer:      func(s stages) wallet.Transfer { return reserve("wd-1", s, held) },
			wantHouse:     -held,
			wantConfirmed: 0,
			wantReserved:  held,
		},
		{
			name:          "a reservation one minor unit past the balance is refused",
			transfer:      func(s stages) wallet.Transfer { return reserve("wd-1", s, held+1) },
			wantErr:       wallet.ErrInsufficientFunds,
			wantHouse:     -held,
			wantConfirmed: held,
			wantReserved:  0,
		},
		{
			name: "an empty member bucket cannot give a single minor unit",
			transfer: func(s stages) wallet.Transfer {
				// The reserved bucket holds nothing, and a member bucket
				// holding nothing is already at its floor.
				return wallet.Transfer{
					IdempotencyKey: "wd-1",
					Postings: []wallet.Posting{
						{Account: s.reserved, Amount: amt(-1, eur)},
						{Account: s.confirmed, Amount: amt(1, eur)},
					},
				}
			},
			wantErr:       wallet.ErrInsufficientFunds,
			wantHouse:     -held,
			wantConfirmed: held,
			wantReserved:  0,
		},
		{
			name: "a house account may go as negative as the money entering demands",
			transfer: func(s stages) wallet.Transfer {
				return wallet.Transfer{
					IdempotencyKey: "credit-1",
					Postings: []wallet.Posting{
						{Account: s.house, Amount: amt(-5000, eur)},
						{Account: s.confirmed, Amount: amt(5000, eur)},
					},
				}
			},
			wantHouse:     -6000,
			wantConfirmed: 6000,
			wantReserved:  0,
		},
		{
			name: "a member drawn past the balance and handed most of it back is judged on the net",
			transfer: func(s stages) wallet.Transfer {
				// Net: the confirmed bucket gives 500 and the reserved one
				// takes it, however large the gross movements are.
				return wallet.Transfer{
					IdempotencyKey: "wd-1",
					Postings: []wallet.Posting{
						{Account: s.confirmed, Amount: amt(-5000, eur)},
						{Account: s.reserved, Amount: amt(5000, eur)},
						{Account: s.reserved, Amount: amt(-4500, eur)},
						{Account: s.confirmed, Amount: amt(4500, eur)},
					},
				}
			},
			wantHouse:     -held,
			wantConfirmed: 500,
			wantReserved:  500,
		},
		{
			name: "a member whose net leaves it short is refused however it is spelled",
			transfer: func(s stages) wallet.Transfer {
				// The same shape, netting to 1500 out of a bucket of 1000.
				return wallet.Transfer{
					IdempotencyKey: "wd-1",
					Postings: []wallet.Posting{
						{Account: s.confirmed, Amount: amt(-5000, eur)},
						{Account: s.reserved, Amount: amt(5000, eur)},
						{Account: s.reserved, Amount: amt(-3500, eur)},
						{Account: s.confirmed, Amount: amt(3500, eur)},
					},
				}
			},
			wantErr:       wallet.ErrInsufficientFunds,
			wantHouse:     -held,
			wantConfirmed: held,
			wantReserved:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, s := funded(t)
			_, err := l.Post(t.Context(), tc.transfer(s))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Post() = %v, want an error wrapping %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Post() = %v, want nil", err)
			}
			// A refusal applies nothing at all, so the balances say both
			// what was recorded and what was not.
			assertBalance(t, l, s.house, amt(tc.wantHouse, eur))
			assertBalance(t, l, s.confirmed, amt(tc.wantConfirmed, eur))
			assertBalance(t, l, s.reserved, amt(tc.wantReserved, eur))
		})
	}

	t.Run("a refused transfer does not consume its key", func(t *testing.T) {
		t.Parallel()

		// The corrected retry is the same withdrawal request under the
		// same derived key (D8): a refusal that burned the key would turn
		// "you asked for more than you have" into a permanent conflict.
		l, s := funded(t)
		if _, err := l.Post(t.Context(), reserve("wd-1", s, held+1)); !errors.Is(err, wallet.ErrInsufficientFunds) {
			t.Fatalf("Post(overdrawn) = %v, want an error wrapping %v", err, wallet.ErrInsufficientFunds)
		}
		if _, err := l.Post(t.Context(), reserve("wd-1", s, held)); err != nil {
			t.Fatalf("Post(corrected retry) = %v, want nil", err)
		}
		assertBalance(t, l, s.confirmed, amt(0, eur))
		assertBalance(t, l, s.reserved, amt(held, eur))
	})
}

// TestConcurrentReservationsCannotDoubleSpend is D9's scenario as a race:
// two withdrawal requests reach the ledger at once, each claiming the
// whole confirmed balance. Nothing above the ledger stops them - there is
// no one-open-request constraint on withdrawal_request - so this refusal
// is the entire double-spend defence, and exactly one request must win.
func TestConcurrentReservationsCannotDoubleSpend(t *testing.T) {
	t.Parallel()

	const held = confirmedBalance
	l, s := funded(t)

	// Distinct keys: these are two different requests fighting over one
	// balance, not one request retried, so idempotency cannot be what
	// separates them.
	keys := []string{"wd-1", "wd-2"}
	errs := make([]error, len(keys))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = l.Post(t.Context(), reserve(key, s, held))
		}()
	}
	close(start)
	wg.Wait()

	recorded, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			recorded++
		case errors.Is(err, wallet.ErrInsufficientFunds):
			refused++
		default:
			t.Fatalf("Post(%s) = %v, want nil or an error wrapping %v", keys[i], err, wallet.ErrInsufficientFunds)
		}
	}
	if recorded != 1 || refused != 1 {
		t.Fatalf("%d reservations recorded and %d were refused, want exactly one of each: one balance funds one withdrawal", recorded, refused)
	}
	// The money is where the winner put it, and the loser took nothing:
	// the confirmed bucket is empty rather than overdrawn.
	assertBalance(t, l, s.confirmed, amt(0, eur))
	assertBalance(t, l, s.reserved, amt(held, eur))
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
		second, err := l.Post(t.Context(), split("entry-1 ", a))
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

		// The ledger must have copied what it recorded: a caller reusing
		// its metadata map for the next transfer is ordinary Go, and it
		// must not rewrite what the key remembers.
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

	// The port's concurrency promise, verbatim: of two (here, many)
	// concurrent posts of one key exactly one records, both learning the
	// same reference. Run under -race, this is also the data-race check.
	const workers = 64

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
	// One winner: the balances moved once, not sixty-four times.
	assertBalance(t, l, a.source, amt(-1000, eur))
	assertBalance(t, l, a.member, amt(900, eur))
	assertBalance(t, l, a.house, amt(100, eur))
}

func TestConcurrentDistinctWork(t *testing.T) {
	t.Parallel()

	t.Run("distinct keys all record", func(t *testing.T) {
		t.Parallel()

		const workers = 32
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

		const workers = 32
		l := memory.New()
		ref := wallet.MemberAccount(memberID, wallet.StageConfirmed)
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
		// race detector, which needs real interleaving to bite on.
		const writers, readers, posts = 4, 4, 8
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

	// The reversal is rebuilt from the original with every amount
	// flipped through Neg, under its own key - never copied out of
	// History, whose postings carry provenance the port refuses.
	reversal := wallet.Transfer{
		IdempotencyKey: "entry-1-reversal",
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

	// Every balance is back where it started, and the history still
	// shows both movements: reversal is a new fact, not an erasure.
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
		IdempotencyKey: "entry-1-recycled",
		Postings:       []wallet.Posting{fromHistory, {Account: a.source, Amount: amt(-900, eur)}},
	}
	if _, err := l.Post(t.Context(), recycled); !errors.Is(err, wallet.ErrRecycledPosting) {
		t.Fatalf("Post(recycled) = %v, want an error wrapping %v", err, wallet.ErrRecycledPosting)
	}
}

// collectHistory drains a History stream into a slice, failing the test
// on any error, immediate or yielded.
func collectHistory(t *testing.T, l *memory.Ledger, account wallet.LedgerAccountID, window wallet.Window) []wallet.Posting {
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
	// ticking clock hands each Post exactly one instant - and returns
	// the references in posting order.
	seed := func(t *testing.T) (*memory.Ledger, accounts, []wallet.TransferRef) {
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
			// want holds the PostedAt offsets, in seconds from anchor,
			// of the postings the window must yield, in order.
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
		// nothing - the tie-break must be the order the ledger recorded.
		l := memory.New(memory.WithClock(func() time.Time { return anchor }))
		source := mustEnsure(t, l, wallet.HouseAccount("network-receivable"), eur)
		member := mustEnsure(t, l, wallet.MemberAccount(memberID, wallet.StagePending), eur)
		refs := make([]wallet.TransferRef, 3)
		for i, key := range []string{"entry-1", "entry-2", "entry-3"} {
			ref, err := l.Post(t.Context(), wallet.Transfer{
				IdempotencyKey: key,
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

		// The snapshot promise, both halves: no deadlock when the loop
		// body posts, and the in-flight iteration keeps answering as of
		// the History call while a fresh one sees the new posting.
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
			t.Fatalf("the in-flight iteration yielded %d postings, want the 3 that existed when History was called", seen)
		}
		if got := collectHistory(t, l, a.member, wallet.Window{}); len(got) != 4 {
			t.Fatalf("a fresh History yielded %d postings, want 4", len(got))
		}
	})

	t.Run("a consumer may stop early", func(t *testing.T) {
		t.Parallel()

		l, a, _ := seed(t)
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
		// The ledger still answers: stopping early leaked no lock.
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

	// The memory ledger does no I/O, but it stands in for three
	// implementations that do: honouring cancellation identically is
	// part of being the reference. Post in particular must consume
	// nothing - the retry with a live context is the same key.
	l, a := newLedger(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := l.EnsureAccount(ctx, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur); !errors.Is(err, context.Canceled) {
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
