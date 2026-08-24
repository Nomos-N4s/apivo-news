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
	"math"
	"math/bits"
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
	// ErrNoAmounts reports a [Sum] with no operands. There is no answer to
	// return: a total of nothing would be a number with no currency, and C-6
	// says no such thing exists.
	ErrNoAmounts = errors.New("money: sum of no amounts has no currency")
	// ErrRateOutOfRange reports a rate outside zero to [BasisPointsScale],
	// matching the rate_bps between 0 and 10000 check the schema carries.
	ErrRateOutOfRange = errors.New("money: rate must be between 0 and 10000 basis points")
	// ErrInvalidRounding reports a rounding mode that was never named,
	// including the zero value of [Rounding]. There is no default mode: the
	// direction money rounds in is a policy decision, not a language default.
	ErrInvalidRounding = errors.New("money: rounding mode not specified")
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

// pairwise validates both operands and confirms they share a currency. Every
// binary operation starts here, so there is exactly one place where the rule
// against implicit conversion lives.
func pairwise(a, b Amount) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if a.Currency != b.Currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	return nil
}

// addMinor adds two counts of minor units, reporting false rather than
// wrapping. The bounds are tested before the addition rather than after it:
// the overflowing value is never computed, so there is nothing to leak into a
// result by mistake.
func addMinor(x, y int64) (int64, bool) {
	if y > 0 && x > math.MaxInt64-y {
		return 0, false
	}
	if y < 0 && x < math.MinInt64-y {
		return 0, false
	}
	return x + y, true
}

// subMinor subtracts two counts of minor units under the same rule as
// [addMinor]. It is not addMinor of a negation: negating math.MinInt64 is
// itself an overflow, so subtraction has to be checked in its own terms.
func subMinor(x, y int64) (int64, bool) {
	if y < 0 && x > math.MaxInt64+y {
		return 0, false
	}
	if y > 0 && x < math.MinInt64+y {
		return 0, false
	}
	return x - y, true
}

// Add returns a + b.
//
// It returns an error wrapping [ErrCurrencyMismatch] when the currencies
// differ - there is no exchange rate here and none will be invented - and an
// error wrapping [ErrOverflow] when the total leaves the int64 range. An
// overflowing sum is never returned wrapped: a balance that silently reads as
// its own negation is worse than a failed posting.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := pairwise(a, b); err != nil {
		return Amount{}, err
	}
	sum, ok := addMinor(a.Minor, b.Minor)
	if !ok {
		return Amount{}, fmt.Errorf("%w: %s + %s", ErrOverflow, a, b)
	}
	return Amount{Minor: sum, Currency: a.Currency}, nil
}

// Sub returns a - b, under the same currency and overflow rules as [Amount.Add].
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := pairwise(a, b); err != nil {
		return Amount{}, err
	}
	diff, ok := subMinor(a.Minor, b.Minor)
	if !ok {
		return Amount{}, fmt.Errorf("%w: %s - %s", ErrOverflow, a, b)
	}
	return Amount{Minor: diff, Currency: a.Currency}, nil
}

// Neg returns -a: the same amount on the other side of a transfer.
//
// The one amount it cannot negate is the smallest representable int64, whose
// magnitude has no positive counterpart. That returns an error wrapping
// [ErrOverflow] rather than the same negative number back, which is what an
// unchecked negation would hand over.
func (a Amount) Neg() (Amount, error) {
	if err := a.Validate(); err != nil {
		return Amount{}, err
	}
	neg, ok := subMinor(0, a.Minor)
	if !ok {
		return Amount{}, fmt.Errorf("%w: -(%s)", ErrOverflow, a)
	}
	return Amount{Minor: neg, Currency: a.Currency}, nil
}

// Abs returns the magnitude of a, with the same overflow case as [Amount.Neg].
func (a Amount) Abs() (Amount, error) {
	if err := a.Validate(); err != nil {
		return Amount{}, err
	}
	if a.Minor >= 0 {
		return a, nil
	}
	return a.Neg()
}

// Cmp compares a and b, returning -1 if a is the smaller, 0 if they are the
// same amount and +1 if a is the larger.
//
// The comparison is made directly rather than by subtracting, so two amounts
// far apart in the int64 range compare correctly instead of overflowing.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := pairwise(a, b); err != nil {
		return 0, err
	}
	switch {
	case a.Minor < b.Minor:
		return -1, nil
	case a.Minor > b.Minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Sum totals amounts, which must all share one currency.
//
// It is the shape C-1 is checked in: the postings of a transfer sum to zero,
// so a caller sums them and asks whether the result [Amount.IsZero]. Summing
// no amounts is an error rather than a zero, because the currency of that zero
// is unknowable. Currencies are not netted against each other here; a transfer
// spanning several currencies balances within each one, which is the caller's
// grouping to make.
func Sum(amounts ...Amount) (Amount, error) {
	if len(amounts) == 0 {
		return Amount{}, ErrNoAmounts
	}
	total := amounts[0]
	if err := total.Validate(); err != nil {
		return Amount{}, err
	}
	for _, next := range amounts[1:] {
		var err error
		if total, err = total.Add(next); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}

// BasisPoints is a rate in hundredths of a percent: 250 basis points is 2.5%,
// and [BasisPointsScale] is the whole.
//
// Rates are expressed this way rather than as a fraction for the same reason
// amounts are minor units - a percentage held as a float is a rounding error
// with a plausible name - and the width matches the int column the schema
// stores rate_bps and member_share_bps in.
type BasisPoints int32

// BasisPointsScale is one hundred percent: the denominator every rate divides
// by, and the largest rate this package accepts.
const BasisPointsScale BasisPoints = 10000

// basisPointsScaleMagnitude is BasisPointsScale as an unsigned divisor, folded
// at compile time so no runtime conversion of a signed constant is needed.
const basisPointsScaleMagnitude = uint64(BasisPointsScale)

// Valid reports whether the rate is between zero and [BasisPointsScale]
// inclusive. Rates above the whole are rejected rather than allowed to produce
// a share larger than what was split.
func (b BasisPoints) Valid() bool { return b >= 0 && b <= BasisPointsScale }

// Rounding names the direction a split rounds in when a rate does not divide
// an amount into whole minor units.
//
// Every mode is explicit and the zero value is none of them, so a caller that
// forgets to choose gets an error rather than whatever integer division
// happened to do. Which mode a given split uses is a policy decision belonging
// to the code that owns the rate - for cashback, the plan's Q4 answer is to
// round to the member's favour and post the remainder to the house.
type Rounding uint8

const (
	// roundUnspecified is the zero value of Rounding and is deliberately not a
	// mode. Its existence is what makes a forgotten argument an error.
	roundUnspecified Rounding = iota
	// RoundTowardZero truncates: the fraction is dropped and the magnitude
	// never grows. This is what Go's integer division does, named so that
	// choosing it is a decision rather than an accident.
	RoundTowardZero
	// RoundAwayFromZero takes any fraction up to the next whole minor unit,
	// increasing the magnitude.
	RoundAwayFromZero
	// RoundFloor rounds toward negative infinity: down for positive amounts,
	// away from zero for negative ones.
	RoundFloor
	// RoundCeil rounds toward positive infinity: up for positive amounts,
	// toward zero for negative ones. Applied to a credit, this is the mode
	// that rounds to the member's favour.
	RoundCeil
	// RoundHalfAwayFromZero is commercial rounding: half a minor unit or more
	// goes up in magnitude, less stays.
	RoundHalfAwayFromZero
	// RoundHalfEven is banker's rounding: exactly half goes to the even number
	// of minor units, so a long run of splits does not drift in one direction
	// the way half-away-from-zero does.
	RoundHalfEven
)

// Valid reports whether r is one of the named modes. The zero value is not.
func (r Rounding) Valid() bool {
	switch r {
	case RoundTowardZero, RoundAwayFromZero, RoundFloor, RoundCeil, RoundHalfAwayFromZero, RoundHalfEven:
		return true
	default:
		return false
	}
}

// String names the mode, so an error about rounding says which one rather than
// printing a number.
func (r Rounding) String() string {
	switch r {
	case RoundTowardZero:
		return "toward zero"
	case RoundAwayFromZero:
		return "away from zero"
	case RoundFloor:
		return "floor"
	case RoundCeil:
		return "ceil"
	case RoundHalfAwayFromZero:
		return "half away from zero"
	case RoundHalfEven:
		return "half even"
	case roundUnspecified:
		return "unspecified"
	default:
		return "unknown rounding mode " + strconv.FormatUint(uint64(r), 10)
	}
}

// magnitude returns the absolute value of v as an unsigned count.
//
// Negating v directly would overflow for the smallest representable int64,
// whose magnitude has no positive counterpart in the signed range, so it is
// built one step short of the boundary and completed unsigned.
func magnitude(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// signedMagnitude reassembles a signed count of minor units from a magnitude
// and a sign.
//
// The caller guarantees the magnitude is representable: [Amount.Split] only
// ever passes back a magnitude no larger than the one it took apart, and the
// one magnitude with no negative-side counterpart problem - two to the
// sixty-third - is exactly the smallest representable int64.
func signedMagnitude(mag uint64, negative bool) int64 {
	if !negative {
		return int64(mag)
	}
	if mag == 1<<63 {
		return math.MinInt64
	}
	return -int64(mag)
}

// roundsAway reports whether a magnitude of units minor units, carrying a
// further fraction of fraction parts in [BasisPointsScale], grows by one under
// mode.
//
// The question is asked about the magnitude rather than the signed value, so
// each mode reduces to "does this get further from zero", and the sign is
// reapplied afterwards. That is also why the sign is a parameter: floor and
// ceil are the two modes whose answer depends on which side of zero the amount
// sits.
func roundsAway(units, fraction uint64, negative bool, mode Rounding) bool {
	if fraction == 0 {
		return false
	}
	switch mode {
	case RoundAwayFromZero:
		return true
	case RoundFloor:
		return negative
	case RoundCeil:
		return !negative
	case RoundHalfAwayFromZero:
		return 2*fraction >= basisPointsScaleMagnitude
	case RoundHalfEven:
		switch {
		case 2*fraction > basisPointsScaleMagnitude:
			return true
		case 2*fraction < basisPointsScaleMagnitude:
			return false
		default:
			// Exactly half: grow only when the units are odd, which is what
			// leaves an even number behind.
			return units%2 == 1
		}
	}
	// RoundTowardZero drops the fraction, and so does any mode Split has
	// already rejected: the whole units stand as they are.
	return false
}

// Split divides a at a basis-point rate, returning the share that rate buys
// and the remainder that is left.
//
// It is the operation behind research D6's "member share = commission_minor x
// rate_bps / 10000", and its contract is one line:
//
//	share plus remainder equals a, exactly, for every rate and every mode.
//
// That is why the remainder is returned rather than discarded. Post the share
// to the member and the remainder to the house account, and the transfer sums
// to zero by construction - C-1 held by arithmetic rather than by luck.
// Rounding that quietly disappears is the classic way a ledger stops
// balancing, so there is no way to call this and not be handed what was
// rounded off. The remainder is the whole of what is not the share: the
// house's own cut as well as any sub-minor-unit fraction the rounding
// produced.
//
// mode must be named. There is no default, because "whatever integer division
// happens to do" is not a policy anyone chose; the plan's Q4 leaves the
// direction as configuration, so the constant lives with the caller and this
// package supplies the modes it can be spelled in.
//
// The arithmetic is exact at every step. The product is formed in 128 bits and
// divided back down, so a rate applied to an amount near the int64 limit is
// neither approximated nor overflowed, and no intermediate value is ever a
// float. Rates outside zero to [BasisPointsScale] are rejected, matching the
// schema's check on rate_bps.
func (a Amount) Split(rate BasisPoints, mode Rounding) (share, remainder Amount, err error) {
	if err = a.Validate(); err != nil {
		return Amount{}, Amount{}, err
	}
	if !rate.Valid() {
		return Amount{}, Amount{}, fmt.Errorf("%w: %d", ErrRateOutOfRange, int32(rate))
	}
	if !mode.Valid() {
		return Amount{}, Amount{}, fmt.Errorf("%w: %s", ErrInvalidRounding, mode)
	}

	negative := a.Minor < 0
	// rate is non-negative by the check above, so widening it keeps its value.
	hi, lo := bits.Mul64(magnitude(a.Minor), uint64(rate))
	// bits.Div64 panics if the quotient will not fit in 64 bits. It cannot
	// here: rate is at most BasisPointsScale, so hi is at most
	// (2^63 * 10^4) >> 64, which is 5000, and 5000 is below the divisor.
	units, fraction := bits.Div64(hi, lo, basisPointsScaleMagnitude)
	if roundsAway(units, fraction, negative, mode) {
		units++
	}

	share = Amount{Minor: signedMagnitude(units, negative), Currency: a.Currency}
	// The share never exceeds a in magnitude and never differs from it in
	// sign, because the rate is at most the whole, so this cannot overflow.
	if remainder, err = a.Sub(share); err != nil {
		return Amount{}, Amount{}, err
	}
	return share, remainder, nil
}

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
