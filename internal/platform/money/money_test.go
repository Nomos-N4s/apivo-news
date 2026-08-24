package money_test

import (
	"errors"
	"math"
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
