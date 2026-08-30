package wallet_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every per-currency rule, and they live
// here rather than in the package because which currencies a deployment
// trades in is configuration, not a property of the port.
const (
	eur = money.Currency("EUR")
	gbp = money.Currency("GBP")
)

// memberID is a fixed member identity, so a failing test prints the same
// reference every run.
var memberID = uuid.MustParse("2b7e1516-28ae-d2a6-abf7-158809cf4f3c")

// amt builds an amount as a struct literal, the way an adapter decoding two
// columns would, so validation is exercised on values the constructors never
// blessed.
func amt(minor int64, currency money.Currency) money.Amount {
	return money.Amount{Minor: minor, Currency: currency}
}

// posting builds an input posting: an account and a signed amount, with the
// ledger-owned provenance fields left zero as Post requires.
func posting(account string, minor int64, currency money.Currency) wallet.Posting {
	return wallet.Posting{Account: wallet.LedgerAccountID(account), Amount: amt(minor, currency)}
}

// balanced is a well-formed transfer the failure cases below break one rule
// at a time. Three postings, because the canonical transfer in this domain
// is a commission split: the source account gives, the member and the house
// receive, and the whole sums to zero (D6).
func balanced() wallet.Transfer {
	return wallet.Transfer{
		IdempotencyKey: "entry-credit-7f3a",
		Postings: []wallet.Posting{
			posting("source", -1000, eur),
			posting("member-pending", 900, eur),
			posting("house-remainder", 100, eur),
		},
		Reference: "entry 7f3a",
	}
}

func TestTransferValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// transfer is either built in place or derived from the balanced
		// base with exactly one rule broken.
		transfer wallet.Transfer
		// wantErr is the sentinel the failure must wrap; nil means the
		// transfer is legal.
		wantErr error
		// wantIn are fragments the error message must carry, so a refusal
		// points at the offending part rather than merely naming a rule.
		wantIn []string
	}{
		{
			name:     "the canonical commission split",
			transfer: balanced(),
		},
		{
			name: "two postings balancing in one currency",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -250, eur),
					posting("b", 250, eur),
				},
			},
		},
		{
			name: "two currencies, each balancing independently",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -1000, eur),
					posting("b", 1000, eur),
					posting("c", -50, gbp),
					posting("d", 50, gbp),
				},
			},
		},
		{
			name: "one account cancelling itself while others move",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
					posting("b", -50, eur),
					posting("c", 50, eur),
				},
			},
		},
		{
			name: "an account appearing twice on the same side",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -60, eur),
					posting("a", -40, eur),
					posting("b", 100, eur),
				},
			},
		},
		{
			name: "balancing at the int64 edge",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", math.MaxInt64, eur),
					posting("b", -math.MaxInt64, eur),
				},
			},
		},
		{
			name: "balancing across the whole int64 range",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", math.MinInt64, eur),
					posting("b", math.MaxInt64, eur),
					posting("c", 1, eur),
				},
			},
		},
		{
			name: "a blank reference and nil metadata are legal",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -1, eur),
					posting("b", 1, eur),
				},
				Reference: "",
				Metadata:  nil,
			},
		},
		{
			name: "empty idempotency key",
			transfer: func() wallet.Transfer {
				tr := balanced()
				tr.IdempotencyKey = ""
				return tr
			}(),
			wantErr: wallet.ErrMissingIdempotencyKey,
		},
		{
			name: "whitespace idempotency key",
			transfer: func() wallet.Transfer {
				tr := balanced()
				tr.IdempotencyKey = " \t\n"
				return tr
			}(),
			wantErr: wallet.ErrMissingIdempotencyKey,
		},
		{
			name: "the key is checked before the postings",
			transfer: wallet.Transfer{
				IdempotencyKey: "",
				Postings: []wallet.Posting{
					posting("a", 1, eur),
				},
			},
			wantErr: wallet.ErrMissingIdempotencyKey,
		},
		{
			name:     "no postings",
			transfer: wallet.Transfer{IdempotencyKey: "k"},
			wantErr:  wallet.ErrTooFewPostings,
			wantIn:   []string{"got 0"},
		},
		{
			name: "one posting cannot be double entry",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings:       []wallet.Posting{posting("a", 100, eur)},
			},
			wantErr: wallet.ErrTooFewPostings,
			wantIn:  []string{"got 1"},
		},
		{
			name: "a posting naming no account",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("", 100, eur),
				},
			},
			wantErr: wallet.ErrMissingAccount,
			wantIn:  []string{"posting 2 of 2"},
		},
		{
			name: "a posting naming a whitespace account",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("  ", -100, eur),
					posting("b", 100, eur),
				},
			},
			wantErr: wallet.ErrMissingAccount,
			wantIn:  []string{"posting 1 of 2"},
		},
		{
			name: "a posting copied from history keeps its transfer reference",
			transfer: func() wallet.Transfer {
				tr := balanced()
				tr.Postings[1].TransferRef = "tref-0001"
				return tr
			}(),
			wantErr: wallet.ErrRecycledPosting,
			wantIn:  []string{"posting 2 of 3"},
		},
		{
			name: "a posting copied from history keeps its posted-at",
			transfer: func() wallet.Transfer {
				tr := balanced()
				tr.Postings[2].PostedAt = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
				return tr
			}(),
			wantErr: wallet.ErrRecycledPosting,
			wantIn:  []string{"posting 3 of 3"},
		},
		{
			name: "a posting with no currency",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("b", 100, ""),
				},
			},
			wantErr: money.ErrInvalidCurrency,
			wantIn:  []string{"posting 2 of 2"},
		},
		{
			name: "a posting with a lowercase currency",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, "eur"),
					posting("b", 100, eur),
				},
			},
			wantErr: money.ErrInvalidCurrency,
			wantIn:  []string{"posting 1 of 2"},
		},
		{
			name: "a posting moving nothing",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("b", 0, eur),
					posting("c", 100, eur),
				},
			},
			wantErr: wallet.ErrZeroPosting,
			wantIn:  []string{"posting 2 of 3"},
		},
		{
			name: "one currency off by one minor unit",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -1000, eur),
					posting("b", 999, eur),
				},
			},
			wantErr: wallet.ErrUnbalanced,
			wantIn:  []string{"EUR nets to -1 EUR"},
		},
		{
			name: "one currency with both postings on the same side",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", 100, eur),
					posting("b", 100, eur),
				},
			},
			wantErr: wallet.ErrUnbalanced,
			wantIn:  []string{"EUR nets to 200 EUR"},
		},
		{
			name: "the split that dropped its remainder",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("source", -1000, eur),
					posting("member", 999, eur),
				},
			},
			wantErr: wallet.ErrUnbalanced,
		},
		{
			name: "two currencies where only one is off",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -1000, eur),
					posting("b", 1000, eur),
					posting("c", -50, gbp),
					posting("d", 49, gbp),
				},
			},
			wantErr: wallet.ErrUnbalanced,
			wantIn:  []string{"GBP nets to -1 GBP"},
		},
		{
			name: "one currency netted against another",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", 100, eur),
					posting("b", -100, gbp),
				},
			},
			wantErr: wallet.ErrMixedCurrency,
			wantIn:  []string{"EUR nets to 100 EUR", "GBP nets to -100 GBP"},
		},
		{
			name: "cross-currency netting spread over three postings",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", 100, eur),
					posting("b", -60, gbp),
					posting("c", -40, gbp),
				},
			},
			wantErr: wallet.ErrMixedCurrency,
		},
		{
			name: "two currencies each off in unrelated ways",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", 100, eur),
					posting("b", -90, gbp),
				},
			},
			wantErr: wallet.ErrMixedCurrency,
		},
		{
			name: "a net that overflows is refused, not wrapped",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", math.MaxInt64, eur),
					posting("b", 1, eur),
				},
			},
			wantErr: money.ErrOverflow,
		},
		{
			// The shape a caller bug makes when the source and the
			// destination resolve to the same account. It balances
			// perfectly and moves nothing, and minting a TransferRef for
			// it would file proof of a payment that never happened.
			name: "a source and a destination that are one account",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
				},
			},
			wantErr: wallet.ErrNoMovement,
			wantIn:  []string{`"a"`},
		},
		{
			name: "two accounts each cancelling themselves",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
					posting("b", -50, eur),
					posting("b", 50, eur),
				},
			},
			wantErr: wallet.ErrNoMovement,
			wantIn:  []string{`"a", "b"`},
		},
		{
			name: "a split whose every share lands back on its source",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("source", -1000, eur),
					posting("source", 900, eur),
					posting("source", 100, eur),
				},
			},
			wantErr: wallet.ErrNoMovement,
		},
		{
			name: "cancelling in two currencies at once",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
					posting("b", -50, gbp),
					posting("b", 50, gbp),
				},
			},
			wantErr: wallet.ErrNoMovement,
			wantIn:  []string{`"a", "b"`},
		},
		{
			// One account holding two currencies is two holdings and one
			// account, and the refusal must name it once.
			name: "one account cancelling itself in two currencies",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
					posting("a", -50, gbp),
					posting("a", 50, gbp),
				},
			},
			wantErr: wallet.ErrNoMovement,
			wantIn:  []string{`("a")`},
		},
		{
			name: "a posting moving nothing is reported before the transfer's stillness",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", -100, eur),
					posting("a", 100, eur),
					posting("b", 0, eur),
				},
			},
			wantErr: wallet.ErrZeroPosting,
			wantIn:  []string{"posting 3 of 3"},
		},
		{
			// The currency nets stay inside int64 the whole way through;
			// only account a's own running total leaves it, which the
			// per-currency pass alone would never have noticed.
			name: "one account's own postings overflowing",
			transfer: wallet.Transfer{
				IdempotencyKey: "k",
				Postings: []wallet.Posting{
					posting("a", math.MaxInt64, eur),
					posting("b", -math.MaxInt64, eur),
					posting("a", 1, eur),
					posting("b", -1, eur),
				},
			},
			wantErr: money.ErrOverflow,
			wantIn:  []string{`account "a"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.transfer.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error wrapping %v", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error wrapping %v", err, tc.wantErr)
			}
			for _, fragment := range tc.wantIn {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("Validate() = %q, want it to mention %q", err, fragment)
				}
			}
		})
	}
}

// TestTransferValidateLeavesTheTransferAlone pins that validation only
// reads: the postings a caller hands over come back byte-identical, so a
// refused transfer can be corrected and retried without wondering what the
// check touched.
func TestTransferValidateLeavesTheTransferAlone(t *testing.T) {
	t.Parallel()

	tr := balanced()
	want := balanced()
	_ = tr.Validate()
	if len(tr.Postings) != len(want.Postings) {
		t.Fatalf("Validate() changed the posting count: got %d, want %d", len(tr.Postings), len(want.Postings))
	}
	for i := range tr.Postings {
		if tr.Postings[i] != want.Postings[i] {
			t.Errorf("Validate() changed posting %d: got %+v, want %+v", i+1, tr.Postings[i], want.Postings[i])
		}
	}
	if tr.IdempotencyKey != want.IdempotencyKey || tr.Reference != want.Reference {
		t.Errorf("Validate() changed the transfer envelope: got %+v, want %+v", tr, want)
	}
}

// TestSentinelsAreDistinct guards the taxonomy itself: the conformance
// suite tells failures apart with errors.Is, so two sentinels that alias
// each other would let an implementation pass the wrong test.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		wallet.ErrMissingIdempotencyKey,
		wallet.ErrTooFewPostings,
		wallet.ErrMissingAccount,
		wallet.ErrZeroPosting,
		wallet.ErrRecycledPosting,
		wallet.ErrUnbalanced,
		wallet.ErrMixedCurrency,
		wallet.ErrNoMovement,
		wallet.ErrInvalidAccountRef,
		wallet.ErrInvalidWindow,
		wallet.ErrUnknownAccount,
		wallet.ErrIdempotencyConflict,
		wallet.ErrInsufficientFunds,
		wallet.ErrUnsupportedTransfer,
		wallet.ErrOutOfBalance,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if got, want := errors.Is(a, b), i == j; got != want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", a, b, got, want)
			}
		}
	}
}

func TestAccountRefValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  wallet.AccountRef
		// wantErr nil means the reference names an account.
		wantErr error
	}{
		{name: "a member's held bucket", ref: wallet.MemberAccount(memberID, wallet.StageHeld)},
		{name: "a member's pending bucket", ref: wallet.MemberAccount(memberID, wallet.StagePending)},
		{name: "a member's confirmed bucket", ref: wallet.MemberAccount(memberID, wallet.StageConfirmed)},
		{name: "a member's reserved bucket", ref: wallet.MemberAccount(memberID, wallet.StageReserved)},
		{name: "a configured house account", ref: wallet.HouseAccount("rounding-remainder")},
		{name: "a house name with interior spaces", ref: wallet.HouseAccount("clawback loss")},
		{name: "the zero reference", ref: wallet.AccountRef{}, wantErr: wallet.ErrInvalidAccountRef},
		{name: "a member reference with the nil id", ref: wallet.MemberAccount(uuid.Nil, wallet.StageConfirmed), wantErr: wallet.ErrInvalidAccountRef},
		{name: "a member reference with no stage", ref: wallet.MemberAccount(memberID, 0), wantErr: wallet.ErrInvalidAccountRef},
		{name: "a member reference with an unknown stage", ref: wallet.MemberAccount(memberID, wallet.Stage(99)), wantErr: wallet.ErrInvalidAccountRef},
		{name: "a house reference with no name", ref: wallet.HouseAccount(""), wantErr: wallet.ErrInvalidAccountRef},
		{name: "a house reference with a whitespace name", ref: wallet.HouseAccount("   "), wantErr: wallet.ErrInvalidAccountRef},
		{name: "a padded house name is rejected, not trimmed", ref: wallet.HouseAccount(" rounding-remainder"), wantErr: wallet.ErrInvalidAccountRef},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.ref.Validate()
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr == nil:
			case !errors.Is(err, tc.wantErr):
				t.Fatalf("Validate() = %v, want an error wrapping %v", err, tc.wantErr)
			}
		})
	}
}

func TestAccountRefAccessors(t *testing.T) {
	t.Parallel()

	t.Run("a member reference round-trips through Member", func(t *testing.T) {
		t.Parallel()

		ref := wallet.MemberAccount(memberID, wallet.StageConfirmed)
		gotID, gotStage, ok := ref.Member()
		if !ok || gotID != memberID || gotStage != wallet.StageConfirmed {
			t.Fatalf("Member() = (%v, %v, %v), want (%v, %v, true)", gotID, gotStage, ok, memberID, wallet.StageConfirmed)
		}
		if _, ok := ref.House(); ok {
			t.Error("House() reported ok for a member reference")
		}
	})

	t.Run("a house reference round-trips through House", func(t *testing.T) {
		t.Parallel()

		ref := wallet.HouseAccount("rounding-remainder")
		name, ok := ref.House()
		if !ok || name != "rounding-remainder" {
			t.Fatalf("House() = (%q, %v), want (%q, true)", name, ok, "rounding-remainder")
		}
		if _, _, ok := ref.Member(); ok {
			t.Error("Member() reported ok for a house reference")
		}
	})

	t.Run("the zero reference is neither", func(t *testing.T) {
		t.Parallel()

		var ref wallet.AccountRef
		if _, _, ok := ref.Member(); ok {
			t.Error("Member() reported ok for the zero reference")
		}
		if _, ok := ref.House(); ok {
			t.Error("House() reported ok for the zero reference")
		}
	})

	t.Run("references are comparable identities", func(t *testing.T) {
		t.Parallel()

		// Adapters key maps on (ref, currency), so equality must mean
		// same account and nothing else.
		first := wallet.MemberAccount(memberID, wallet.StagePending)
		second := wallet.MemberAccount(memberID, wallet.StagePending)
		if first != second {
			t.Error("identical member references compare unequal")
		}
		if wallet.MemberAccount(memberID, wallet.StagePending) == wallet.MemberAccount(memberID, wallet.StageConfirmed) {
			t.Error("two stages of one member compare equal")
		}
		if wallet.HouseAccount("a") == wallet.HouseAccount("b") {
			t.Error("two house accounts compare equal")
		}
	})
}

func TestAccountRefString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  wallet.AccountRef
		want string
	}{
		{
			name: "a member reference names the member and the stage",
			ref:  wallet.MemberAccount(memberID, wallet.StageConfirmed),
			want: "member 2b7e1516-28ae-d2a6-abf7-158809cf4f3c (confirmed)",
		},
		{
			name: "a house reference names the configured account",
			ref:  wallet.HouseAccount("rounding-remainder"),
			want: `house "rounding-remainder"`,
		},
		{
			name: "the zero reference says so",
			ref:  wallet.AccountRef{},
			want: "unspecified account",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.ref.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStageValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stage wallet.Stage
		want  bool
	}{
		{name: "held", stage: wallet.StageHeld, want: true},
		{name: "pending", stage: wallet.StagePending, want: true},
		{name: "confirmed", stage: wallet.StageConfirmed, want: true},
		{name: "reserved", stage: wallet.StageReserved, want: true},
		{name: "the zero value is not a stage", stage: 0, want: false},
		{name: "an unnamed value is not a stage", stage: wallet.Stage(99), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.stage.Valid(); got != tc.want {
				t.Errorf("Stage(%d).Valid() = %v, want %v", uint8(tc.stage), got, tc.want)
			}
		})
	}
}

func TestStageString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stage wallet.Stage
		want  string
	}{
		{name: "held", stage: wallet.StageHeld, want: "held"},
		{name: "pending", stage: wallet.StagePending, want: "pending"},
		{name: "confirmed", stage: wallet.StageConfirmed, want: "confirmed"},
		{name: "reserved", stage: wallet.StageReserved, want: "reserved"},
		{name: "zero value", stage: 0, want: "unspecified"},
		{name: "unnamed value", stage: wallet.Stage(99), want: "unknown stage 99"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.stage.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// anchor is an arbitrary fixed instant the window tests measure from.
var anchor = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func TestWindowValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		window  wallet.Window
		wantErr error
	}{
		{name: "the zero window is the whole of history", window: wallet.Window{}},
		{name: "a lower bound alone", window: wallet.Window{From: anchor}},
		{name: "an upper bound alone", window: wallet.Window{To: anchor}},
		{name: "an ordered pair of bounds", window: wallet.Window{From: anchor, To: anchor.Add(time.Hour)}},
		{name: "an empty but ordered window", window: wallet.Window{From: anchor, To: anchor}},
		{
			name:    "swapped bounds",
			window:  wallet.Window{From: anchor, To: anchor.Add(-time.Nanosecond)},
			wantErr: wallet.ErrInvalidWindow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.window.Validate()
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr == nil:
			case !errors.Is(err, tc.wantErr):
				t.Fatalf("Validate() = %v, want an error wrapping %v", err, tc.wantErr)
			}
		})
	}
}

func TestWindowContains(t *testing.T) {
	t.Parallel()

	bounded := wallet.Window{From: anchor, To: anchor.Add(time.Hour)}
	tests := []struct {
		name   string
		window wallet.Window
		at     time.Time
		want   bool
	}{
		{name: "inside a bounded window", window: bounded, at: anchor.Add(time.Minute), want: true},
		{name: "exactly at From is inside", window: bounded, at: anchor, want: true},
		{name: "exactly at To is outside, so adjacent windows share no posting", window: bounded, at: anchor.Add(time.Hour), want: false},
		{name: "before From", window: bounded, at: anchor.Add(-time.Nanosecond), want: false},
		{name: "after To", window: bounded, at: anchor.Add(2 * time.Hour), want: false},
		{name: "the zero window contains everything", window: wallet.Window{}, at: anchor, want: true},
		{name: "a zero From imposes no lower bound", window: wallet.Window{To: anchor}, at: anchor.Add(-24 * time.Hour), want: true},
		{name: "a zero To imposes no upper bound", window: wallet.Window{From: anchor}, at: anchor.Add(24 * time.Hour), want: true},
		{name: "an empty window contains nothing, not even its own bound", window: wallet.Window{From: anchor, To: anchor}, at: anchor, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.window.Contains(tc.at); got != tc.want {
				t.Errorf("Contains(%s) = %v, want %v", tc.at.Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}
