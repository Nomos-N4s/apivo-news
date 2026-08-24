// Package money is the only representation of a monetary amount in this
// repository. It exists to make constitution invariant C-6 - all monetary
// amounts are integer minor units carrying an explicit ISO-4217 currency -
// true in Go, rather than merely written down in a document.
//
// There is no float64 and no decimal string anywhere in this package, in its
// API or in its internals. That is deliberate and absolute. A float in a money
// position is a rounding error waiting for an audit to find it, and a decimal
// string is a float that has not been parsed yet. An int64 of minor units
// represents every amount this product will ever handle exactly, which is also
// why there is no decimal dependency here: it would be a step backwards from a
// type that is already exact.
//
// Three rules follow from that, and every operation below enforces them:
//
//   - Currency is never implicit and never coerced. Adding one currency to
//     another is an error, not a conversion.
//   - Overflow is detected and returned as an error, never wrapped. A silently
//     wrapped int64 is a balance that reads as its own negation.
//   - Rounding is never silent. Splitting an amount returns the remainder as a
//     first-class value, so the caller can post it to a house account and keep
//     every transfer summing to zero (C-1). Rounding that disappears is the
//     classic way a ledger stops balancing.
//
// Display formatting is deliberately absent. Grouping separators, symbol
// placement and the decimal mark are locale decisions the frontend owns; the
// String method here prints minor units precisely so that it can never be
// mistaken for something member-facing.
package money

import (
	"errors"
	"fmt"
	"strconv"
)

// Sentinel errors. Every failure this package reports wraps exactly one of
// them, so a caller can branch with errors.Is on the kind of failure while the
// message carries the offending values.
var (
	// ErrInvalidCurrency reports a currency code that is not three uppercase
	// ASCII letters, including the empty code an unset Amount carries.
	ErrInvalidCurrency = errors.New("money: invalid ISO-4217 currency code")
	// ErrCurrencyMismatch reports an operation spanning two different
	// currencies. It is never a conversion request: this package holds no
	// exchange rates and will not invent one.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow reports an arithmetic result outside the int64 range. The
	// operation is abandoned; no wrapped value is ever returned.
	ErrOverflow = errors.New("money: arithmetic overflow")
)

// currencyCodeLen is the length of an ISO-4217 alphabetic code, and of the
// char(3) column every stored amount sits beside.
const currencyCodeLen = 3

// Currency is an ISO-4217 alphabetic currency code: exactly three uppercase
// ASCII letters.
//
// Membership of the ISO-4217 register is deliberately not checked here. The
// register changes, a copy of it embedded in this package would go stale and
// begin rejecting real currencies, and which currencies a deployment actually
// trades in is brand configuration rather than a property of the money type.
// The format check is what makes an amount storable and a mistyped code
// visible.
type Currency string

// Valid reports whether c is three uppercase ASCII letters. The empty
// currency, which is what an Amount built as a bare struct literal carries, is
// not valid: C-6 has no notion of an amount without a currency.
func (c Currency) Valid() bool {
	if len(c) != currencyCodeLen {
		return false
	}
	for i := range len(c) {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// String returns the code itself, so a Currency prints as "EUR" rather than as
// its underlying string type.
func (c Currency) String() string { return string(c) }

// ParseCurrency validates s and returns it as a Currency.
//
// Lowercase input is rejected rather than upcased. This package coerces
// nothing: a code arriving in the wrong case means some caller assembled it by
// hand, and that is worth seeing rather than smoothing over.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(s)
	if !c.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
	}
	return c, nil
}

// Amount is a monetary amount: a signed count of minor units - cents for EUR,
// pence for GBP - together with the currency those units belong to. Negative
// amounts are ordinary; a ledger posting is a debit or a credit.
//
// Amount is comparable, so it works as a map key and with ==, which compares
// the currency as well as the units.
//
// The zero value is not a usable amount: it carries no currency, and every
// operation here rejects it. Build amounts with [New], or with [Zero] for an
// explicit nil balance in a known currency. The fields are exported because
// adapters decode straight into them from two database columns, which means a
// struct literal can carry an invalid currency - [Amount.Validate] is how code
// at a trust boundary checks one it did not construct itself.
type Amount struct {
	// Minor is the amount in minor units of Currency. It is never scaled,
	// never fractional and never a float.
	Minor int64
	// Currency is the ISO-4217 code the minor units are denominated in. It is
	// never defaulted and never inferred.
	Currency Currency
}

// New returns the amount of minor units in currency, rejecting a currency code
// that is not well formed.
func New(minor int64, currency Currency) (Amount, error) {
	if !currency.Valid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(currency))
	}
	return Amount{Minor: minor, Currency: currency}, nil
}

// Zero returns the zero amount in currency. It exists because the zero Amount
// value carries no currency and so is not a balance: an account holding
// nothing still holds nothing in a particular currency.
func Zero(currency Currency) (Amount, error) { return New(0, currency) }

// Validate reports whether a is well formed, returning an error wrapping
// [ErrInvalidCurrency] when it is not. Every operation in this package
// validates its own operands, so calling this directly is needed only at a
// trust boundary - decoding a row, or accepting a struct literal from another
// package - where the answer is wanted before any arithmetic.
func (a Amount) Validate() error {
	if !a.Currency.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.Currency))
	}
	return nil
}

// IsZero reports whether the amount is zero minor units. It says nothing about
// whether the currency is valid; a zero balance in an unset currency is still
// not a balance.
func (a Amount) IsZero() bool { return a.Minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (a Amount) IsPositive() bool { return a.Minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (a Amount) IsNegative() bool { return a.Minor < 0 }

// Equal reports whether a and b are the same number of minor units in the same
// currency. Amounts in different currencies are never equal, including when
// both are zero: nothing in EUR and nothing in GBP are different postings, and
// treating them as one is how a mixed-currency transfer starts to look
// balanced.
func (a Amount) Equal(b Amount) bool { return a == b }

// String renders the amount for logs, errors and test failures as minor units
// followed by the currency code - "1234 EUR", not "12.34 EUR".
//
// That is not a shortcoming. Display formatting is locale work the frontend
// owns, and printing minor units means this output can never be pasted onto a
// member-facing surface and mistaken for a price. An amount with no currency
// says so, rather than printing bare units that look valid.
func (a Amount) String() string {
	units := strconv.FormatInt(a.Minor, 10)
	if a.Currency == "" {
		return units + " <no currency>"
	}
	return units + " " + string(a.Currency)
}
