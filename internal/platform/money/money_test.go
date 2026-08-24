package money_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every mixed-currency rejection, and they
// live here rather than in the package because no currency is a property of
// the money type - which one a deployment trades in is brand configuration.
const (
	eur = money.Currency("EUR")
	gbp = money.Currency("GBP")
)

// allModes is every rounding mode the package names. Tests that assert a
// property of splitting - that the share and the remainder complete the
// original amount, above all - run over all of them, so a mode added later
// inherits the guarantees rather than quietly opting out of them.
var allModes = []money.Rounding{
	money.RoundTowardZero,
	money.RoundAwayFromZero,
	money.RoundFloor,
	money.RoundCeil,
	money.RoundHalfAwayFromZero,
	money.RoundHalfEven,
}

func TestCurrencyValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code money.Currency
		want bool
	}{
		{name: "three uppercase letters", code: "EUR", want: true},
		{name: "another well formed code", code: "GBP", want: true},
		{name: "all letters accepted, register not consulted", code: "ZZZ", want: true},
		{name: "empty, which is what an unset Amount carries", code: "", want: false},
		{name: "too short", code: "EU", want: false},
		{name: "too long", code: "EURO", want: false},
		{name: "lowercase is not upcased", code: "eur", want: false},
		{name: "mixed case", code: "Eur", want: false},
		{name: "digits", code: "E1R", want: false},
		{name: "padding", code: "EU ", want: false},
		{name: "symbol", code: "EU$", want: false},
		{name: "three runes but more than three bytes", code: "EU€", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.code.Valid(); got != tc.want {
				t.Errorf("Currency(%q).Valid() = %v, want %v", string(tc.code), got, tc.want)
			}
		})
	}
}

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	t.Run("accepts a well formed code", func(t *testing.T) {
		t.Parallel()

		got, err := money.ParseCurrency("EUR")
		if err != nil {
			t.Fatalf("ParseCurrency(\"EUR\") returned error: %v", err)
		}
		if got != eur {
			t.Errorf("ParseCurrency(\"EUR\") = %q, want %q", string(got), string(eur))
		}
	})

	t.Run("rejects malformed codes and names the input", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{"", "eur", "EURO", "E1R"} {
			got, err := money.ParseCurrency(in)
			if !errors.Is(err, money.ErrInvalidCurrency) {
				t.Errorf("ParseCurrency(%q) error = %v, want ErrInvalidCurrency", in, err)
			}
			if got != "" {
				t.Errorf("ParseCurrency(%q) = %q, want the empty currency on failure", in, string(got))
			}
			if in != "" && !strings.Contains(err.Error(), in) {
				t.Errorf("ParseCurrency(%q) error %q does not name the input", in, err)
			}
		}
	})
}

func TestCurrencyString(t *testing.T) {
	t.Parallel()

	if got := eur.String(); got != "EUR" {
		t.Errorf("Currency.String() = %q, want %q", got, "EUR")
	}
	if got := money.Currency("").String(); got != "" {
		t.Errorf("empty Currency.String() = %q, want %q", got, "")
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		minor    int64
		currency money.Currency
		wantErr  error
	}{
		{name: "positive", minor: 1234, currency: eur},
		{name: "negative, which is an ordinary debit", minor: -1234, currency: eur},
		{name: "zero", minor: 0, currency: gbp},
		{name: "largest representable", minor: math.MaxInt64, currency: eur},
		{name: "smallest representable", minor: math.MinInt64, currency: eur},
		{name: "no currency", minor: 1, currency: "", wantErr: money.ErrInvalidCurrency},
		{name: "malformed currency", minor: 1, currency: "eur", wantErr: money.ErrInvalidCurrency},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := money.New(tc.minor, tc.currency)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("New(%d, %q) error = %v, want %v", tc.minor, string(tc.currency), err, tc.wantErr)
				}
				if got != (money.Amount{}) {
					t.Errorf("New(%d, %q) = %v, want the zero Amount on failure", tc.minor, string(tc.currency), got)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%d, %q) returned error: %v", tc.minor, string(tc.currency), err)
			}
			if got.Minor != tc.minor || got.Currency != tc.currency {
				t.Errorf("New(%d, %q) = %v, want %d %s", tc.minor, string(tc.currency), got, tc.minor, tc.currency)
			}
		})
	}
}

func TestZero(t *testing.T) {
	t.Parallel()

	got, err := money.Zero(eur)
	if err != nil {
		t.Fatalf("Zero(EUR) returned error: %v", err)
	}
	if got.Minor != 0 || got.Currency != eur {
		t.Errorf("Zero(EUR) = %v, want 0 EUR", got)
	}
	if got == (money.Amount{}) {
		t.Error("Zero(EUR) equals the zero Amount value; a zero balance must still carry its currency")
	}

	if _, err := money.Zero(""); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Zero(\"\") error = %v, want ErrInvalidCurrency", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	if err := (money.Amount{Minor: 1, Currency: eur}).Validate(); err != nil {
		t.Errorf("Validate() on a well formed amount returned error: %v", err)
	}
	// The zero value is the case that matters: a struct literal built by an
	// adapter that forgot the currency column must not pass for money.
	if err := (money.Amount{}).Validate(); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Validate() on the zero Amount = %v, want ErrInvalidCurrency", err)
	}
	if err := (money.Amount{Minor: 5, Currency: "€"}).Validate(); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Validate() on a symbol currency = %v, want ErrInvalidCurrency", err)
	}
}

func TestSignPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		minor                           int64
		wantZero, wantPositive, wantNeg bool
	}{
		{name: "positive", minor: 1, wantPositive: true},
		{name: "negative", minor: -1, wantNeg: true},
		{name: "zero", minor: 0, wantZero: true},
		{name: "largest", minor: math.MaxInt64, wantPositive: true},
		{name: "smallest", minor: math.MinInt64, wantNeg: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := money.Amount{Minor: tc.minor, Currency: eur}
			if got := a.IsZero(); got != tc.wantZero {
				t.Errorf("(%v).IsZero() = %v, want %v", a, got, tc.wantZero)
			}
			if got := a.IsPositive(); got != tc.wantPositive {
				t.Errorf("(%v).IsPositive() = %v, want %v", a, got, tc.wantPositive)
			}
			if got := a.IsNegative(); got != tc.wantNeg {
				t.Errorf("(%v).IsNegative() = %v, want %v", a, got, tc.wantNeg)
			}
		})
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b money.Amount
		want bool
	}{
		{
			name: "same units, same currency",
			a:    money.Amount{Minor: 100, Currency: eur},
			b:    money.Amount{Minor: 100, Currency: eur},
			want: true,
		},
		{
			name: "different units",
			a:    money.Amount{Minor: 100, Currency: eur},
			b:    money.Amount{Minor: 101, Currency: eur},
		},
		{
			name: "same units, different currency",
			a:    money.Amount{Minor: 100, Currency: eur},
			b:    money.Amount{Minor: 100, Currency: gbp},
		},
		{
			name: "zero in different currencies is not one zero",
			a:    money.Amount{Minor: 0, Currency: eur},
			b:    money.Amount{Minor: 0, Currency: gbp},
		},
		{
			name: "a currency-less amount equals only itself",
			a:    money.Amount{Minor: 0},
			b:    money.Amount{Minor: 0, Currency: eur},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Errorf("(%v).Equal(%v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := tc.b.Equal(tc.a); got != tc.want {
				t.Errorf("Equal is not symmetric: (%v).Equal(%v) = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// binaryCase drives Add, Sub and Cmp alike: the same operand pairs have to be
// rejected by all three, and writing the mixed-currency and unset-currency
// cases once is what stops one of them quietly acquiring an exception.
type binaryCase struct {
	name    string
	a, b    money.Amount
	want    int64 // for Add and Sub, the expected minor units
	wantCmp int
	wantErr error
}

func mixedAndMalformedCases() []binaryCase {
	return []binaryCase{
		{
			name:    "mixed currencies are rejected, never converted",
			a:       money.Amount{Minor: 100, Currency: eur},
			b:       money.Amount{Minor: 100, Currency: gbp},
			wantErr: money.ErrCurrencyMismatch,
		},
		{
			name:    "left operand carries no currency",
			a:       money.Amount{Minor: 100},
			b:       money.Amount{Minor: 100, Currency: eur},
			wantErr: money.ErrInvalidCurrency,
		},
		{
			name:    "right operand carries no currency",
			a:       money.Amount{Minor: 100, Currency: eur},
			b:       money.Amount{Minor: 100},
			wantErr: money.ErrInvalidCurrency,
		},
		{
			name:    "two currency-less amounts are still not one currency",
			a:       money.Amount{Minor: 1},
			b:       money.Amount{Minor: 1},
			wantErr: money.ErrInvalidCurrency,
		},
	}
}

func TestAdd(t *testing.T) {
	t.Parallel()

	tests := append([]binaryCase{
		{
			name: "two positives",
			a:    money.Amount{Minor: 1234, Currency: eur},
			b:    money.Amount{Minor: 766, Currency: eur},
			want: 2000,
		},
		{
			name: "a credit and a debit",
			a:    money.Amount{Minor: 1234, Currency: eur},
			b:    money.Amount{Minor: -1234, Currency: eur},
			want: 0,
		},
		{
			name: "two negatives",
			a:    money.Amount{Minor: -5, Currency: gbp},
			b:    money.Amount{Minor: -7, Currency: gbp},
			want: -12,
		},
		{
			name: "adding zero",
			a:    money.Amount{Minor: 99, Currency: eur},
			b:    money.Amount{Minor: 0, Currency: eur},
			want: 99,
		},
		{
			name: "up to the largest representable amount, but not past it",
			a:    money.Amount{Minor: math.MaxInt64 - 1, Currency: eur},
			b:    money.Amount{Minor: 1, Currency: eur},
			want: math.MaxInt64,
		},
		{
			name: "down to the smallest representable amount",
			a:    money.Amount{Minor: math.MinInt64 + 1, Currency: eur},
			b:    money.Amount{Minor: -1, Currency: eur},
			want: math.MinInt64,
		},
		{
			name:    "positive overflow is detected, not wrapped",
			a:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			b:       money.Amount{Minor: 1, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{
			name:    "negative overflow is detected, not wrapped",
			a:       money.Amount{Minor: math.MinInt64, Currency: eur},
			b:       money.Amount{Minor: -1, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{
			name:    "the two extremes together overflow in the positive direction",
			a:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			b:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			wantErr: money.ErrOverflow,
		},
	}, mixedAndMalformedCases()...)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.a.Add(tc.b)
			assertAmount(t, "Add", tc.a, tc.b, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestSub(t *testing.T) {
	t.Parallel()

	tests := append([]binaryCase{
		{
			name: "ordinary difference",
			a:    money.Amount{Minor: 2000, Currency: eur},
			b:    money.Amount{Minor: 766, Currency: eur},
			want: 1234,
		},
		{
			name: "difference crossing zero",
			a:    money.Amount{Minor: 5, Currency: eur},
			b:    money.Amount{Minor: 12, Currency: eur},
			want: -7,
		},
		{
			name: "subtracting a negative",
			a:    money.Amount{Minor: 5, Currency: gbp},
			b:    money.Amount{Minor: -7, Currency: gbp},
			want: 12,
		},
		{
			name: "an amount less itself is zero",
			a:    money.Amount{Minor: math.MinInt64, Currency: eur},
			b:    money.Amount{Minor: math.MinInt64, Currency: eur},
			want: 0,
		},
		{
			name: "up to the largest representable amount",
			a:    money.Amount{Minor: math.MaxInt64 - 1, Currency: eur},
			b:    money.Amount{Minor: -1, Currency: eur},
			want: math.MaxInt64,
		},
		{
			// The other guard's boundary. The case above lands exactly on the
			// top of the range and pins the y < 0 branch; this lands exactly
			// on the bottom and pins the y > 0 one, which would otherwise be
			// defended only from the failing side - an off-by-one there would
			// reject a subtraction that is perfectly representable.
			name: "down to the smallest representable amount",
			a:    money.Amount{Minor: math.MinInt64 + 1, Currency: eur},
			b:    money.Amount{Minor: 1, Currency: eur},
			want: math.MinInt64,
		},
		{
			name:    "and one step past the bottom of the range",
			a:       money.Amount{Minor: math.MinInt64 + 1, Currency: eur},
			b:       money.Amount{Minor: 2, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{
			name:    "positive overflow is detected",
			a:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			b:       money.Amount{Minor: -1, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{
			name:    "negative overflow is detected",
			a:       money.Amount{Minor: math.MinInt64, Currency: eur},
			b:       money.Amount{Minor: 1, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{
			name:    "subtracting the smallest representable amount from a positive one",
			a:       money.Amount{Minor: 1, Currency: eur},
			b:       money.Amount{Minor: math.MinInt64, Currency: eur},
			wantErr: money.ErrOverflow,
		},
	}, mixedAndMalformedCases()...)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.a.Sub(tc.b)
			assertAmount(t, "Sub", tc.a, tc.b, got, err, tc.want, tc.wantErr)
		})
	}
}

// assertAmount checks one binary result: the error kind when a failure is
// expected, and both the units and the currency when it is not.
func assertAmount(t *testing.T, op string, a, b, got money.Amount, err error, want int64, wantErr error) {
	t.Helper()

	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Fatalf("(%v).%s(%v) error = %v, want %v", a, op, b, err, wantErr)
		}
		if got != (money.Amount{}) {
			t.Errorf("(%v).%s(%v) = %v, want the zero Amount on failure", a, op, b, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("(%v).%s(%v) returned error: %v", a, op, b, err)
	}
	if got.Minor != want {
		t.Errorf("(%v).%s(%v) = %d minor units, want %d", a, op, b, got.Minor, want)
	}
	if got.Currency != a.Currency {
		t.Errorf("(%v).%s(%v) currency = %q, want %q", a, op, b, got.Currency, a.Currency)
	}
}

func TestNeg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a       money.Amount
		want    int64
		wantErr error
	}{
		{name: "positive becomes negative", a: money.Amount{Minor: 1234, Currency: eur}, want: -1234},
		{name: "negative becomes positive", a: money.Amount{Minor: -1234, Currency: eur}, want: 1234},
		{name: "zero stays zero", a: money.Amount{Minor: 0, Currency: gbp}, want: 0},
		{name: "largest representable negates", a: money.Amount{Minor: math.MaxInt64, Currency: eur}, want: math.MinInt64 + 1},
		{
			name:    "the smallest representable amount has no positive counterpart",
			a:       money.Amount{Minor: math.MinInt64, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{name: "no currency", a: money.Amount{Minor: 1}, wantErr: money.ErrInvalidCurrency},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.a.Neg()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("(%v).Neg() error = %v, want %v", tc.a, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("(%v).Neg() returned error: %v", tc.a, err)
			}
			if got.Minor != tc.want || got.Currency != tc.a.Currency {
				t.Errorf("(%v).Neg() = %v, want %d %s", tc.a, got, tc.want, tc.a.Currency)
			}
		})
	}
}

func TestAbs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a       money.Amount
		want    int64
		wantErr error
	}{
		{name: "positive is unchanged", a: money.Amount{Minor: 1234, Currency: eur}, want: 1234},
		{name: "negative is flipped", a: money.Amount{Minor: -1234, Currency: eur}, want: 1234},
		{name: "zero", a: money.Amount{Minor: 0, Currency: eur}, want: 0},
		{
			name:    "the smallest representable amount has no magnitude in range",
			a:       money.Amount{Minor: math.MinInt64, Currency: eur},
			wantErr: money.ErrOverflow,
		},
		{name: "no currency", a: money.Amount{Minor: -1}, wantErr: money.ErrInvalidCurrency},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.a.Abs()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("(%v).Abs() error = %v, want %v", tc.a, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("(%v).Abs() returned error: %v", tc.a, err)
			}
			if got.Minor != tc.want || got.Currency != tc.a.Currency {
				t.Errorf("(%v).Abs() = %v, want %d %s", tc.a, got, tc.want, tc.a.Currency)
			}
		})
	}
}

func TestCmp(t *testing.T) {
	t.Parallel()

	tests := append([]binaryCase{
		{
			name:    "smaller",
			a:       money.Amount{Minor: 1, Currency: eur},
			b:       money.Amount{Minor: 2, Currency: eur},
			wantCmp: -1,
		},
		{
			name:    "larger",
			a:       money.Amount{Minor: 2, Currency: eur},
			b:       money.Amount{Minor: 1, Currency: eur},
			wantCmp: 1,
		},
		{
			name:    "equal",
			a:       money.Amount{Minor: 2, Currency: eur},
			b:       money.Amount{Minor: 2, Currency: eur},
			wantCmp: 0,
		},
		{
			name:    "negative is below zero",
			a:       money.Amount{Minor: -1, Currency: gbp},
			b:       money.Amount{Minor: 0, Currency: gbp},
			wantCmp: -1,
		},
		{
			// A subtraction-based comparison would overflow here and report
			// the wrong order.
			name:    "the two extremes compare without overflowing",
			a:       money.Amount{Minor: math.MinInt64, Currency: eur},
			b:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			wantCmp: -1,
		},
		{
			name:    "and in the other direction",
			a:       money.Amount{Minor: math.MaxInt64, Currency: eur},
			b:       money.Amount{Minor: math.MinInt64, Currency: eur},
			wantCmp: 1,
		},
	}, mixedAndMalformedCases()...)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.a.Cmp(tc.b)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("(%v).Cmp(%v) error = %v, want %v", tc.a, tc.b, err, tc.wantErr)
				}
				if got != 0 {
					t.Errorf("(%v).Cmp(%v) = %d, want 0 on failure", tc.a, tc.b, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("(%v).Cmp(%v) returned error: %v", tc.a, tc.b, err)
			}
			if got != tc.wantCmp {
				t.Errorf("(%v).Cmp(%v) = %d, want %d", tc.a, tc.b, got, tc.wantCmp)
			}
		})
	}
}

func TestSum(t *testing.T) {
	t.Parallel()

	t.Run("totals a run of amounts", func(t *testing.T) {
		t.Parallel()

		got, err := money.Sum(
			money.Amount{Minor: 1000, Currency: eur},
			money.Amount{Minor: -250, Currency: eur},
			money.Amount{Minor: -750, Currency: eur},
		)
		if err != nil {
			t.Fatalf("Sum returned error: %v", err)
		}
		// This is the shape C-1 is checked in: the postings of a transfer
		// total zero, in one currency.
		if !got.IsZero() || got.Currency != eur {
			t.Errorf("Sum of a balanced transfer = %v, want 0 EUR", got)
		}
	})

	t.Run("a single amount is its own total", func(t *testing.T) {
		t.Parallel()

		one := money.Amount{Minor: 42, Currency: gbp}
		got, err := money.Sum(one)
		if err != nil || got != one {
			t.Errorf("Sum(%v) = %v, %v, want %v, <nil>", one, got, err, one)
		}
	})

	t.Run("no amounts has no currency and so no answer", func(t *testing.T) {
		t.Parallel()

		if _, err := money.Sum(); !errors.Is(err, money.ErrNoAmounts) {
			t.Errorf("Sum() error = %v, want ErrNoAmounts", err)
		}
	})

	t.Run("rejects a mixed-currency run", func(t *testing.T) {
		t.Parallel()

		_, err := money.Sum(
			money.Amount{Minor: 1, Currency: eur},
			money.Amount{Minor: 1, Currency: gbp},
		)
		if !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Errorf("Sum across currencies error = %v, want ErrCurrencyMismatch", err)
		}
	})

	t.Run("rejects a currency-less first amount", func(t *testing.T) {
		t.Parallel()

		if _, err := money.Sum(money.Amount{Minor: 1}); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("Sum of an unset amount error = %v, want ErrInvalidCurrency", err)
		}
	})

	t.Run("rejects a currency-less later amount", func(t *testing.T) {
		t.Parallel()

		_, err := money.Sum(money.Amount{Minor: 1, Currency: eur}, money.Amount{Minor: 1})
		if !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("Sum with an unset amount error = %v, want ErrInvalidCurrency", err)
		}
	})

	t.Run("reports overflow rather than wrapping a total", func(t *testing.T) {
		t.Parallel()

		_, err := money.Sum(
			money.Amount{Minor: math.MaxInt64, Currency: eur},
			money.Amount{Minor: 1, Currency: eur},
		)
		if !errors.Is(err, money.ErrOverflow) {
			t.Errorf("Sum past the int64 range error = %v, want ErrOverflow", err)
		}
	})
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    money.Amount
		want string
	}{
		{name: "positive", a: money.Amount{Minor: 1234, Currency: eur}, want: "1234 EUR"},
		{name: "negative", a: money.Amount{Minor: -1234, Currency: gbp}, want: "-1234 GBP"},
		{name: "zero", a: money.Amount{Minor: 0, Currency: eur}, want: "0 EUR"},
		{name: "sub-unit values are units, not a decimal", a: money.Amount{Minor: 7, Currency: eur}, want: "7 EUR"},
		{name: "no currency says so", a: money.Amount{Minor: 5}, want: "5 <no currency>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.a.String()
			if got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			// The point of printing minor units is that the output can never
			// be mistaken for a formatted price.
			if strings.ContainsAny(got, ".,") {
				t.Errorf("String() = %q, which reads as a formatted decimal", got)
			}
		})
	}
}

func TestBasisPointsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rate money.BasisPoints
		want bool
	}{
		{rate: 0, want: true},
		{rate: 1, want: true},
		{rate: 250, want: true},
		{rate: money.BasisPointsScale, want: true},
		{rate: money.BasisPointsScale + 1},
		{rate: -1},
		{rate: math.MinInt32},
		{rate: math.MaxInt32},
	}

	for _, tc := range tests {
		if got := tc.rate.Valid(); got != tc.want {
			t.Errorf("BasisPoints(%d).Valid() = %v, want %v", int32(tc.rate), got, tc.want)
		}
	}
}

func TestRoundingValid(t *testing.T) {
	t.Parallel()

	for _, mode := range allModes {
		if !mode.Valid() {
			t.Errorf("%s is a named mode but reports invalid", mode)
		}
	}
	// The zero value is what a caller who forgot the argument passes, and it
	// is deliberately not a mode.
	var unset money.Rounding
	if unset.Valid() {
		t.Error("the zero Rounding reports valid; a forgotten mode must be an error, not truncation")
	}
	if money.Rounding(99).Valid() {
		t.Error("an unnamed Rounding reports valid")
	}
}

func TestRoundingString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode money.Rounding
		want string
	}{
		{mode: money.RoundTowardZero, want: "toward zero"},
		{mode: money.RoundAwayFromZero, want: "away from zero"},
		{mode: money.RoundFloor, want: "floor"},
		{mode: money.RoundCeil, want: "ceil"},
		{mode: money.RoundHalfAwayFromZero, want: "half away from zero"},
		{mode: money.RoundHalfEven, want: "half even"},
		{mode: money.Rounding(0), want: "unspecified"},
		{mode: money.Rounding(99), want: "unknown rounding mode 99"},
	}

	for _, tc := range tests {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("Rounding(%d).String() = %q, want %q", uint8(tc.mode), got, tc.want)
		}
	}
}

// splitCase is one amount split at one rate under one mode, with the share the
// mode is required to produce. The remainder is never stated: it is always
// what completes the amount, and every case asserts that rather than a number
// somebody typed.
type splitCase struct {
	name      string
	minor     int64
	rate      money.BasisPoints
	mode      money.Rounding
	wantShare int64
}

func TestSplitRoundingAtTheMinorUnit(t *testing.T) {
	t.Parallel()

	tests := []splitCase{
		// Half of 1001 minor units is 500.5: the case every mode answers
		// differently, and the one a ledger loses money in.
		{name: "exactly half, toward zero", minor: 1001, rate: 5000, mode: money.RoundTowardZero, wantShare: 500},
		{name: "exactly half, away from zero", minor: 1001, rate: 5000, mode: money.RoundAwayFromZero, wantShare: 501},
		{name: "exactly half, floor", minor: 1001, rate: 5000, mode: money.RoundFloor, wantShare: 500},
		{name: "exactly half, ceil", minor: 1001, rate: 5000, mode: money.RoundCeil, wantShare: 501},
		{name: "exactly half, half away from zero", minor: 1001, rate: 5000, mode: money.RoundHalfAwayFromZero, wantShare: 501},
		{name: "exactly half, half even lands on the even units", minor: 1001, rate: 5000, mode: money.RoundHalfEven, wantShare: 500},
		// 501.5, where half-even goes the other way: 501 is odd, so the even
		// neighbour is above.
		{name: "exactly half, half even from odd units", minor: 1003, rate: 5000, mode: money.RoundHalfEven, wantShare: 502},
		{name: "exactly half, half away from zero from odd units", minor: 1003, rate: 5000, mode: money.RoundHalfAwayFromZero, wantShare: 502},

		// Below half: 0.1001 of a minor unit.
		{name: "below half, toward zero", minor: 1001, rate: 1, mode: money.RoundTowardZero, wantShare: 0},
		{name: "below half, away from zero", minor: 1001, rate: 1, mode: money.RoundAwayFromZero, wantShare: 1},
		{name: "below half, ceil", minor: 1001, rate: 1, mode: money.RoundCeil, wantShare: 1},
		{name: "below half, floor", minor: 1001, rate: 1, mode: money.RoundFloor, wantShare: 0},
		{name: "below half, half away from zero", minor: 1001, rate: 1, mode: money.RoundHalfAwayFromZero, wantShare: 0},
		{name: "below half, half even", minor: 1001, rate: 1, mode: money.RoundHalfEven, wantShare: 0},

		// Above half: 0.9009 of a minor unit.
		{name: "above half, half away from zero", minor: 1001, rate: 9, mode: money.RoundHalfAwayFromZero, wantShare: 1},
		{name: "above half, half even", minor: 1001, rate: 9, mode: money.RoundHalfEven, wantShare: 1},
		{name: "above half, toward zero", minor: 1001, rate: 9, mode: money.RoundTowardZero, wantShare: 0},

		// The same fractions on a negative amount. Floor and ceil are the two
		// modes that change sides; the rest are symmetric about zero.
		{name: "negative, exactly half, toward zero", minor: -1001, rate: 5000, mode: money.RoundTowardZero, wantShare: -500},
		{name: "negative, exactly half, away from zero", minor: -1001, rate: 5000, mode: money.RoundAwayFromZero, wantShare: -501},
		{name: "negative, exactly half, floor goes down", minor: -1001, rate: 5000, mode: money.RoundFloor, wantShare: -501},
		{name: "negative, exactly half, ceil goes up", minor: -1001, rate: 5000, mode: money.RoundCeil, wantShare: -500},
		{name: "negative, exactly half, half away from zero", minor: -1001, rate: 5000, mode: money.RoundHalfAwayFromZero, wantShare: -501},
		{name: "negative, exactly half, half even", minor: -1001, rate: 5000, mode: money.RoundHalfEven, wantShare: -500},

		// The same tie one minor unit along, where the truncated units are
		// ODD. Every case above lands on -500, which is even, so half-even and
		// plain truncation agree there and no test tells them apart on the
		// debit side. That matters here specifically: reversals and clawbacks
		// are the negative path, and a reversal that rounds differently from
		// the credit it reverses leaves a residue in the ledger.
		{name: "negative, odd units tie, toward zero", minor: -1003, rate: 5000, mode: money.RoundTowardZero, wantShare: -501},
		{name: "negative, odd units tie, away from zero", minor: -1003, rate: 5000, mode: money.RoundAwayFromZero, wantShare: -502},
		{name: "negative, odd units tie, floor goes down", minor: -1003, rate: 5000, mode: money.RoundFloor, wantShare: -502},
		{name: "negative, odd units tie, ceil goes up", minor: -1003, rate: 5000, mode: money.RoundCeil, wantShare: -501},
		{name: "negative, odd units tie, half away from zero", minor: -1003, rate: 5000, mode: money.RoundHalfAwayFromZero, wantShare: -502},
		{name: "negative, odd units tie, half even leaves the even neighbour", minor: -1003, rate: 5000, mode: money.RoundHalfEven, wantShare: -502},

		// And the credit side of the same tie, so the pair is complete and the
		// symmetry is visible in the table rather than only in a property.
		{name: "odd units tie, toward zero", minor: 1003, rate: 5000, mode: money.RoundTowardZero, wantShare: 501},
		{name: "odd units tie, away from zero", minor: 1003, rate: 5000, mode: money.RoundAwayFromZero, wantShare: 502},
		{name: "odd units tie, floor", minor: 1003, rate: 5000, mode: money.RoundFloor, wantShare: 501},
		{name: "odd units tie, ceil", minor: 1003, rate: 5000, mode: money.RoundCeil, wantShare: 502},
		{name: "negative, below half, ceil truncates toward zero", minor: -1001, rate: 1, mode: money.RoundCeil, wantShare: 0},
		{name: "negative, below half, floor", minor: -1001, rate: 1, mode: money.RoundFloor, wantShare: -1},

		// Nothing to round: the fraction is zero, so no mode moves the share.
		{name: "divides exactly, toward zero", minor: 1000, rate: 5000, mode: money.RoundTowardZero, wantShare: 500},
		{name: "divides exactly, away from zero", minor: 1000, rate: 5000, mode: money.RoundAwayFromZero, wantShare: 500},
		{name: "divides exactly, ceil", minor: 1000, rate: 5000, mode: money.RoundCeil, wantShare: 500},
		{name: "divides exactly, half even leaves odd units alone", minor: 2000, rate: 5000, mode: money.RoundHalfEven, wantShare: 1000},

		// Zero splits to zero in every direction; there is no fraction to
		// round and no remainder to place.
		{name: "zero, away from zero", minor: 0, rate: 5000, mode: money.RoundAwayFromZero, wantShare: 0},
		{name: "zero, ceil", minor: 0, rate: 1, mode: money.RoundCeil, wantShare: 0},

		// The boundary rates.
		{name: "the whole rate hands over everything", minor: 1001, rate: money.BasisPointsScale, mode: money.RoundTowardZero, wantShare: 1001},
		{name: "the whole rate on a negative amount", minor: -1001, rate: money.BasisPointsScale, mode: money.RoundAwayFromZero, wantShare: -1001},
		{name: "a zero rate hands over nothing", minor: 1001, rate: 0, mode: money.RoundAwayFromZero, wantShare: 0},
		{name: "a zero rate on a negative amount", minor: -1001, rate: 0, mode: money.RoundFloor, wantShare: 0},
		{name: "one basis point of one minor unit, away from zero", minor: 1, rate: 1, mode: money.RoundAwayFromZero, wantShare: 1},
		{name: "one basis point of one minor unit, toward zero", minor: 1, rate: 1, mode: money.RoundTowardZero, wantShare: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := money.Amount{Minor: tc.minor, Currency: eur}
			share, remainder, err := a.Split(tc.rate, tc.mode)
			if err != nil {
				t.Fatalf("(%v).Split(%d, %s) returned error: %v", a, int32(tc.rate), tc.mode, err)
			}
			if share.Minor != tc.wantShare {
				t.Errorf("(%v).Split(%d, %s) share = %d, want %d", a, int32(tc.rate), tc.mode, share.Minor, tc.wantShare)
			}
			assertCompletes(t, a, share, remainder)
		})
	}
}

// assertCompletes is the invariant the whole package turns on: what came out
// of a split adds back up to what went in, in the same currency. If this ever
// fails, a transfer built from the two postings does not sum to zero and C-1
// is broken.
func assertCompletes(t *testing.T, a, share, remainder money.Amount) {
	t.Helper()

	if share.Currency != a.Currency || remainder.Currency != a.Currency {
		t.Fatalf("split of %v produced %v and %v; a share or remainder changed currency", a, share, remainder)
	}
	back, err := share.Add(remainder)
	if err != nil {
		t.Fatalf("split of %v produced %v and %v, which do not add: %v", a, share, remainder, err)
	}
	if back != a {
		t.Fatalf("split of %v produced %v and %v, which total %v; the difference is money that left the ledger", a, share, remainder, back)
	}
}

func TestSplitCompletesTheAmountAtEveryRate(t *testing.T) {
	t.Parallel()

	// The extremes are in the list because the 128-bit product is exactly
	// where a naive implementation overflows, and zero is in it because a
	// split of nothing must still be two postings that balance.
	amounts := []int64{
		0, 1, -1, 7, -7, 99, -99, 1000, -1000, 12345, -12345, 99999999,
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
	}

	for _, mode := range allModes {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()

			for _, minor := range amounts {
				a := money.Amount{Minor: minor, Currency: eur}
				for rate := money.BasisPoints(0); rate <= money.BasisPointsScale; rate++ {
					share, remainder, err := a.Split(rate, mode)
					if err != nil {
						t.Fatalf("(%v).Split(%d, %s) returned error: %v", a, int32(rate), mode, err)
					}
					assertCompletes(t, a, share, remainder)
					assertShareWithinAmount(t, a, share, rate, mode)
				}
			}
		})
	}
}

// assertShareWithinAmount checks that a share of at most the whole is at most
// the whole: it never overshoots the amount and never crosses zero to the
// other side of it. Stated without an absolute value, because the smallest
// representable amount has none.
func assertShareWithinAmount(t *testing.T, a, share money.Amount, rate money.BasisPoints, mode money.Rounding) {
	t.Helper()

	switch {
	case a.Minor >= 0 && (share.Minor < 0 || share.Minor > a.Minor):
		t.Fatalf("(%v).Split(%d, %s) share = %v, outside 0..%d", a, int32(rate), mode, share, a.Minor)
	case a.Minor < 0 && (share.Minor > 0 || share.Minor < a.Minor):
		t.Fatalf("(%v).Split(%d, %s) share = %v, outside %d..0", a, int32(rate), mode, share, a.Minor)
	}
}

func TestSplitReversesACreditExactly(t *testing.T) {
	t.Parallel()

	// A reversal splits the negation of what the credit split. Unless the two
	// round to exactly mirrored shares, reversing an entry leaves a residue
	// behind in the ledger, so this is the property clawbacks rest on.
	//
	// Four modes are symmetric about zero and must mirror exactly. Floor and
	// ceil are deliberately not - they are defined by direction on the number
	// line, not by distance from zero - and they mirror into each other
	// instead, which is asserted rather than left as a surprise.
	symmetric := []money.Rounding{
		money.RoundTowardZero,
		money.RoundAwayFromZero,
		money.RoundHalfAwayFromZero,
		money.RoundHalfEven,
	}
	// Ties, near-ties and ordinary values, all negatable.
	amounts := []int64{1, 7, 1001, 1002, 1003, 12345, 99999999, math.MaxInt64}
	rates := []money.BasisPoints{1, 3, 2500, 4999, 5000, 5001, 6500, 9999, money.BasisPointsScale}

	for _, minor := range amounts {
		credit := money.Amount{Minor: minor, Currency: eur}
		debit, err := credit.Neg()
		if err != nil {
			t.Fatalf("(%v).Neg() returned error: %v", credit, err)
		}

		for _, rate := range rates {
			for _, mode := range symmetric {
				assertMirrored(t, credit, debit, rate, mode, mode)
			}
			// floor(-x) is -ceil(x): the two swap roles across zero.
			assertMirrored(t, credit, debit, rate, money.RoundCeil, money.RoundFloor)
			assertMirrored(t, credit, debit, rate, money.RoundFloor, money.RoundCeil)
		}
	}
}

// assertMirrored checks that splitting debit under debitMode produces exactly
// the negation of splitting credit under creditMode, in both the share and the
// remainder.
func assertMirrored(t *testing.T, credit, debit money.Amount, rate money.BasisPoints, creditMode, debitMode money.Rounding) {
	t.Helper()

	creditShare, creditRest, err := credit.Split(rate, creditMode)
	if err != nil {
		t.Fatalf("(%v).Split(%d, %s) returned error: %v", credit, int32(rate), creditMode, err)
	}
	debitShare, debitRest, err := debit.Split(rate, debitMode)
	if err != nil {
		t.Fatalf("(%v).Split(%d, %s) returned error: %v", debit, int32(rate), debitMode, err)
	}

	wantShare, err := creditShare.Neg()
	if err != nil {
		t.Fatalf("(%v).Neg() returned error: %v", creditShare, err)
	}
	wantRest, err := creditRest.Neg()
	if err != nil {
		t.Fatalf("(%v).Neg() returned error: %v", creditRest, err)
	}

	if debitShare != wantShare || debitRest != wantRest {
		t.Errorf("splitting %v at %d %s gave %v and %v; reversing %v at %s gave %v and %v, which does not mirror it",
			credit, int32(rate), creditMode, creditShare, creditRest, debit, debitMode, debitShare, debitRest)
	}
}

func TestSplitMatchesExactIntegerArithmetic(t *testing.T) {
	t.Parallel()

	// An independent oracle in arbitrary precision, so the 128-bit product and
	// division are checked against arithmetic that has no width to overflow.
	// Only the four unambiguous modes are oracled here: big.Int's Quo
	// truncates toward zero and its Div floors, which are two of them
	// outright, and the other two follow without restating this package's
	// rounding rules in the test.
	amounts := []int64{
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
		999999999999999999, -999999999999999999, 12345678901234567, -1,
	}
	rates := []money.BasisPoints{1, 3, 7, 9, 1234, 4999, 5000, 5001, 9999, money.BasisPointsScale}

	scale := big.NewInt(int64(money.BasisPointsScale))
	one := big.NewInt(1)

	for _, minor := range amounts {
		a := money.Amount{Minor: minor, Currency: eur}
		for _, rate := range rates {
			product := new(big.Int).Mul(big.NewInt(minor), big.NewInt(int64(rate)))

			truncated, truncRem := new(big.Int).QuoRem(product, scale, new(big.Int))
			away := new(big.Int).Set(truncated)
			if truncRem.Sign() != 0 {
				if minor < 0 {
					away.Sub(away, one)
				} else {
					away.Add(away, one)
				}
			}
			// Div is Euclidean and the divisor is positive, so it floors.
			floor := new(big.Int).Div(product, scale)
			ceil := new(big.Int).Set(floor)
			if new(big.Int).Mod(product, scale).Sign() != 0 {
				ceil.Add(ceil, one)
			}

			for _, want := range []struct {
				mode money.Rounding
				want *big.Int
			}{
				{mode: money.RoundTowardZero, want: truncated},
				{mode: money.RoundAwayFromZero, want: away},
				{mode: money.RoundFloor, want: floor},
				{mode: money.RoundCeil, want: ceil},
			} {
				share, remainder, err := a.Split(rate, want.mode)
				if err != nil {
					t.Fatalf("(%v).Split(%d, %s) returned error: %v", a, int32(rate), want.mode, err)
				}
				if !want.want.IsInt64() || share.Minor != want.want.Int64() {
					t.Errorf("(%v).Split(%d, %s) share = %d, want %s", a, int32(rate), want.mode, share.Minor, want.want)
				}
				assertCompletes(t, a, share, remainder)
			}
		}
	}
}

func TestSplitRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a       money.Amount
		rate    money.BasisPoints
		mode    money.Rounding
		wantErr error
	}{
		{
			name:    "an amount with no currency",
			a:       money.Amount{Minor: 1000},
			rate:    5000,
			mode:    money.RoundCeil,
			wantErr: money.ErrInvalidCurrency,
		},
		{
			name:    "a negative rate",
			a:       money.Amount{Minor: 1000, Currency: eur},
			rate:    -1,
			mode:    money.RoundCeil,
			wantErr: money.ErrRateOutOfRange,
		},
		{
			name:    "a rate above the whole",
			a:       money.Amount{Minor: 1000, Currency: eur},
			rate:    money.BasisPointsScale + 1,
			mode:    money.RoundCeil,
			wantErr: money.ErrRateOutOfRange,
		},
		{
			name:    "a forgotten rounding mode",
			a:       money.Amount{Minor: 1000, Currency: eur},
			rate:    5000,
			mode:    money.Rounding(0),
			wantErr: money.ErrInvalidRounding,
		},
		{
			name:    "a rounding mode that was never named",
			a:       money.Amount{Minor: 1000, Currency: eur},
			rate:    5000,
			mode:    money.Rounding(99),
			wantErr: money.ErrInvalidRounding,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			share, remainder, err := tc.a.Split(tc.rate, tc.mode)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("(%v).Split(%d, %s) error = %v, want %v", tc.a, int32(tc.rate), tc.mode, err, tc.wantErr)
			}
			if share != (money.Amount{}) || remainder != (money.Amount{}) {
				t.Errorf("(%v).Split(%d, %s) = %v, %v, want two zero Amounts on failure", tc.a, int32(tc.rate), tc.mode, share, remainder)
			}
		})
	}
}

func TestSplitPostsTheRemainderToTheHouse(t *testing.T) {
	t.Parallel()

	// The worked example from research D6: a commission arrives, the member's
	// configured share of it is credited rounded to the member's favour, and
	// what is left - the house's own cut plus the fraction of a cent the
	// rounding created - is the other posting. The two postings and the
	// commission are a transfer that sums to zero, which is C-1.
	commission := money.Amount{Minor: 12345, Currency: eur}
	const memberShare = money.BasisPoints(6500)

	member, house, err := commission.Split(memberShare, money.RoundCeil)
	if err != nil {
		t.Fatalf("Split returned error: %v", err)
	}

	// 12345 x 6500 / 10000 is 8024.25, and the member's favour rounds up.
	if member.Minor != 8025 {
		t.Errorf("member share = %v, want 8025 EUR", member)
	}
	if house.Minor != 4320 {
		t.Errorf("house remainder = %v, want 4320 EUR", house)
	}

	debit, err := commission.Neg()
	if err != nil {
		t.Fatalf("Neg returned error: %v", err)
	}
	transfer, err := money.Sum(debit, member, house)
	if err != nil {
		t.Fatalf("Sum of the transfer's postings returned error: %v", err)
	}
	if !transfer.IsZero() {
		t.Errorf("the transfer's postings total %v, want zero; a transfer that does not balance is not postable", transfer)
	}
}

func TestMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    money.Amount
		want string
	}{
		{name: "ordinary amount", a: money.Amount{Minor: 1234, Currency: eur}, want: `{"minor":1234,"currency":"EUR"}`},
		{name: "negative", a: money.Amount{Minor: -1234, Currency: gbp}, want: `{"minor":-1234,"currency":"GBP"}`},
		{name: "zero", a: money.Amount{Minor: 0, Currency: eur}, want: `{"minor":0,"currency":"EUR"}`},
		{
			name: "a whole number of major units is still minor units",
			a:    money.Amount{Minor: 100, Currency: eur},
			want: `{"minor":100,"currency":"EUR"}`,
		},
		{
			name: "largest representable",
			a:    money.Amount{Minor: math.MaxInt64, Currency: eur},
			want: `{"minor":9223372036854775807,"currency":"EUR"}`,
		},
		{
			name: "smallest representable",
			a:    money.Amount{Minor: math.MinInt64, Currency: eur},
			want: `{"minor":-9223372036854775808,"currency":"EUR"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := json.Marshal(tc.a)
			if err != nil {
				t.Fatalf("json.Marshal(%v) returned error: %v", tc.a, err)
			}
			if string(got) != tc.want {
				t.Errorf("json.Marshal(%v) = %s, want %s", tc.a, got, tc.want)
			}
			// The whole point: no decimal ever crosses an API boundary. Read
			// back as raw JSON so the assertion is about the number itself
			// rather than about the letters in a currency code.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatalf("re-reading %s returned error: %v", got, err)
			}
			minor := string(fields["minor"])
			if strings.ContainsAny(minor, ".eE") {
				t.Errorf("json.Marshal(%v) encoded the units as %s, which carries a decimal or an exponent", tc.a, minor)
			}
		})
	}
}

func TestMarshalJSONRejectsAnAmountWithNoCurrency(t *testing.T) {
	t.Parallel()

	// Units on the wire without a currency are a number that is not money, so
	// there is nothing to encode.
	if _, err := json.Marshal(money.Amount{Minor: 1234}); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("json.Marshal of an amount with no currency error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := json.Marshal(money.Amount{Minor: 1234, Currency: "eur"}); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("json.Marshal of a malformed currency error = %v, want ErrInvalidCurrency", err)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want money.Amount
	}{
		{name: "ordinary amount", in: `{"minor":1234,"currency":"EUR"}`, want: money.Amount{Minor: 1234, Currency: eur}},
		{name: "negative", in: `{"minor":-1234,"currency":"GBP"}`, want: money.Amount{Minor: -1234, Currency: gbp}},
		{name: "zero", in: `{"minor":0,"currency":"EUR"}`, want: money.Amount{Minor: 0, Currency: eur}},
		{name: "fields in either order", in: `{"currency":"EUR","minor":7}`, want: money.Amount{Minor: 7, Currency: eur}},
		{name: "whitespace", in: "{ \"minor\" : 7 , \"currency\" : \"EUR\" }", want: money.Amount{Minor: 7, Currency: eur}},
		{
			name: "largest representable",
			in:   `{"minor":9223372036854775807,"currency":"EUR"}`,
			want: money.Amount{Minor: math.MaxInt64, Currency: eur},
		},
		{
			name: "smallest representable",
			in:   `{"minor":-9223372036854775808,"currency":"EUR"}`,
			want: money.Amount{Minor: math.MinInt64, Currency: eur},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got money.Amount
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("json.Unmarshal(%s) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnmarshalJSONAcceptsSurroundingWhitespace(t *testing.T) {
	t.Parallel()

	// Whitespace is the one thing allowed to surround the object, so the check
	// for trailing content must not become a check for trailing bytes. Called
	// directly, because json.Unmarshal trims the value before the method sees
	// it and would prove nothing about the method itself.
	for _, in := range []string{
		"{\"minor\":7,\"currency\":\"EUR\"}\n\t  ",
		"  \n{\"minor\":7,\"currency\":\"EUR\"}",
		"\t {\"minor\":7,\"currency\":\"EUR\"} \n",
	} {
		var got money.Amount
		if err := got.UnmarshalJSON([]byte(in)); err != nil {
			t.Errorf("UnmarshalJSON(%q) returned error: %v", in, err)
			continue
		}
		if want := (money.Amount{Minor: 7, Currency: eur}); got != want {
			t.Errorf("UnmarshalJSON(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestUnmarshalJSONRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "a decimal", in: `{"minor":12.34,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "a decimal that happens to be whole", in: `{"minor":1234.0,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "an exponent", in: `{"minor":1.234e3,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "a negative decimal", in: `{"minor":-0.01,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "units past the int64 range", in: `{"minor":99999999999999999999,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "no minor units at all", in: `{"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "null minor units", in: `{"minor":null,"currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "units in quotes", in: `{"minor":"1234","currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "a decimal in quotes", in: `{"minor":"12.34","currency":"EUR"}`, wantErr: money.ErrNotMinorUnits},
		{name: "no currency", in: `{"minor":1234}`, wantErr: money.ErrInvalidCurrency},
		{name: "an empty currency", in: `{"minor":1234,"currency":""}`, wantErr: money.ErrInvalidCurrency},
		{name: "a lowercase currency", in: `{"minor":1234,"currency":"eur"}`, wantErr: money.ErrInvalidCurrency},
		{name: "a currency symbol", in: `{"minor":1234,"currency":"€"}`, wantErr: money.ErrInvalidCurrency},
		{name: "a field this type does not define", in: `{"minor":1234,"currency":"EUR","major":12}`, wantErr: money.ErrMalformedJSON},
		// encoding/json matches struct tags case-insensitively, so a
		// case-varied key is not an unknown field to it but a second spelling
		// of the same one - and the last spelling wins silently, even with
		// DisallowUnknownFields set. For a type whose contract is "exactly
		// this object", a caller who believes they sent 1 while their payload
		// also carries 9999 must be told, not quietly given one of them.
		{name: "a case-varied duplicate of the units", in: `{"minor":1,"Minor":9999,"currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		{name: "a case-varied duplicate of the currency", in: `{"minor":1,"currency":"EUR","CURRENCY":"GBP"}`, wantErr: money.ErrMalformedJSON},
		{name: "a case-varied units key on its own", in: `{"Minor":42,"currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		{name: "a case-varied currency key on its own", in: `{"minor":42,"Currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		{name: "an exact duplicate of the units", in: `{"minor":1,"minor":9999,"currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		{name: "an exact duplicate of the currency", in: `{"minor":1,"currency":"EUR","currency":"GBP"}`, wantErr: money.ErrMalformedJSON},
		{name: "null", in: `null`, wantErr: money.ErrMalformedJSON},
		{name: "a formatted price as a string", in: `"12.34 EUR"`, wantErr: money.ErrMalformedJSON},
		{name: "a bare number", in: `1234`, wantErr: money.ErrMalformedJSON},
		{name: "an array", in: `[1234,"EUR"]`, wantErr: money.ErrMalformedJSON},
		{name: "truncated JSON", in: `{"minor":1234,`, wantErr: money.ErrMalformedJSON},
		{name: "nothing at all", in: ``, wantErr: money.ErrMalformedJSON},
		{name: "whitespace only", in: "  \n\t", wantErr: money.ErrMalformedJSON},
		{name: "not JSON at all", in: `xyz`, wantErr: money.ErrMalformedJSON},
		{name: "an object that never closes", in: `{`, wantErr: money.ErrMalformedJSON},
		{name: "a member with no closing brace", in: `{"minor":1`, wantErr: money.ErrMalformedJSON},
		{name: "a key with no value", in: `{"minor":`, wantErr: money.ErrMalformedJSON},
		{name: "a currency that is not a string", in: `{"minor":1,"currency":123}`, wantErr: money.ErrMalformedJSON},
		{name: "a currency that is an array", in: `{"minor":1,"currency":["EUR"]}`, wantErr: money.ErrMalformedJSON},
		// A well-formed document carrying no currency: the shape is fine, the
		// currency is not, so it is the currency error rather than the JSON
		// one - the same answer as an empty code.
		{name: "a currency that is null", in: `{"minor":1,"currency":null}`, wantErr: money.ErrInvalidCurrency},
		{name: "a second amount trailing the first", in: `{"minor":1,"currency":"EUR"} {"minor":2,"currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		{name: "a second amount with nothing between them", in: `{"minor":1,"currency":"EUR"}{"minor":2,"currency":"EUR"}`, wantErr: money.ErrMalformedJSON},
		// A stray closing delimiter is the case a Decoder.More() check waves
		// through: it asks whether another VALUE is coming, and a `}` is not
		// the start of one, so a malformed body reads as a complete amount.
		{name: "a stray closing brace", in: `{"minor":1,"currency":"EUR"}}`, wantErr: money.ErrMalformedJSON},
		{name: "a stray closing bracket", in: `{"minor":1,"currency":"EUR"}]`, wantErr: money.ErrMalformedJSON},
		{name: "a stray comma", in: `{"minor":1,"currency":"EUR"},`, wantErr: money.ErrMalformedJSON},
		{name: "trailing garbage", in: `{"minor":1,"currency":"EUR"} not json`, wantErr: money.ErrMalformedJSON},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Called directly rather than through json.Unmarshal so that
			// trailing content reaches the method rather than being caught by
			// the standard decoder first.
			before := money.Amount{Minor: 999, Currency: gbp}
			got := before
			err := got.UnmarshalJSON([]byte(tc.in))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UnmarshalJSON(%s) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			if got != before {
				t.Errorf("UnmarshalJSON(%s) left the amount as %v; a rejected decode must not half-write one", tc.in, got)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()

	// The wire shape is what the ledger port passes around, so it is exercised
	// inside a struct as well as on its own.
	type posting struct {
		Account string       `json:"account"`
		Amount  money.Amount `json:"amount"`
	}

	amounts := []money.Amount{
		{Minor: 0, Currency: eur},
		{Minor: 1, Currency: eur},
		{Minor: -1, Currency: gbp},
		{Minor: 8025, Currency: eur},
		{Minor: math.MaxInt64, Currency: eur},
		{Minor: math.MinInt64, Currency: gbp},
	}

	for _, want := range amounts {
		encoded, err := json.Marshal(posting{Account: "member", Amount: want})
		if err != nil {
			t.Fatalf("json.Marshal of a posting holding %v returned error: %v", want, err)
		}
		var got posting
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
		}
		if got.Amount != want {
			t.Errorf("round trip of %v produced %v", want, got.Amount)
		}
	}
}

func TestPackageHasNoFloatPath(t *testing.T) {
	t.Parallel()

	// C-6 says floating point in a money position is unrepresentable. In Go
	// that is only true while nobody adds a float, so it is asserted here
	// rather than trusted: the package's own source is parsed and any float
	// type, float literal, float conversion or decimal dependency fails the
	// build. The source is parsed without comments, because this package
	// writes about floats at length and the rule is about code.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if node.Name == "float32" || node.Name == "float64" {
					t.Errorf("%s: %s has no place in a money package", fset.Position(node.Pos()), node.Name)
				}
			case *ast.BasicLit:
				if node.Kind == token.FLOAT || node.Kind == token.IMAG {
					t.Errorf("%s: %s is a floating point literal", fset.Position(node.Pos()), node.Value)
				}
			case *ast.SelectorExpr:
				if strings.Contains(node.Sel.Name, "Float") {
					t.Errorf("%s: %s is a floating point conversion", fset.Position(node.Pos()), node.Sel.Name)
				}
			case *ast.ImportSpec:
				if strings.Contains(node.Path.Value, "decimal") {
					t.Errorf("%s: %s is a decimal dependency, and int64 minor units already represent money exactly",
						fset.Position(node.Pos()), node.Path.Value)
				}
			}
			return true
		})
	}

	if scanned < 2 {
		t.Fatalf("scanned %d Go files; expected at least the package and its tests", scanned)
	}
}
