// Turning Linkwise's decimal money strings into minor units, exactly (C-6).
//
// A file of its own because this is the one place in the adapter where a
// wrong answer is a wrong number of euros rather than a missing field, and
// because the obvious implementation is wrong. Measured, over every hundredth
// from 0.00 to 99.99:
//
//   - float64(x) * 100 truncated to an integer is wrong on 573 of the 10,000.
//     The first is 0.29, which parses to a float whose hundred-fold is
//     28.999999999999996 and truncates to 28. A cent lost on some values and
//     not others reconciles against a network statement as a drift nobody can
//     source.
//   - float64(x) * 100 ROUNDED is exact at retail sizes - all 10,000 - and
//     stops being exact somewhere above 1e12 minor units: around 1e14 it is
//     wrong on half of them. So that route is not wrong, it is wrong
//     eventually, which is worse: it passes every test written at the scale
//     of a shopping basket.
//
// So the string is read as a string: split at the point, pad the fraction,
// concatenate, parse once as an integer. No float is constructed at any step,
// and exactness is a property of the method rather than of the magnitude.

package linkwise

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotAnAmount reports a money field this adapter will not translate: not a
// number at all, carrying an exponent or a separator, or carrying more
// decimal places than the currency has minor units.
//
// Refused rather than rounded, in every case. A third decimal place in a
// money field means the assumption this adapter is built on - that Linkwise's
// currencies are two-decimal - is wrong for the programme that produced it,
// and rounding it away would hide exactly the evidence needed to find out.
var ErrNotAnAmount = errors.New("linkwise: money field is not a decimal amount this adapter can read")

// minorUnitExponent is how many decimal places Linkwise's amounts carry.
//
// TWO, and it is stated here rather than derived from the currency because
// [money.Currency] deliberately holds no register of exponents - the ISO
// register changes, and an embedded copy would go stale and start refusing
// real currencies. Two is right for every currency Linkwise trades in: it
// operates in Greece and SE Europe, so EUR, RON, BGN and RSD, all of which
// are two-decimal. It would be WRONG for a zero-decimal currency such as JPY,
// which is why [minorUnits] refuses a value carrying more decimal places than
// this rather than rounding one away.
const minorUnitExponent = 2

// minorUnits parses a decimal string into minor units at
// [minorUnitExponent] decimal places.
//
// What it accepts is deliberately narrow: an optional sign, digits, at most
// one point, digits. No exponent, no thousands separator, no currency symbol,
// no spaces inside. The recording carries "19.51" and "2.93"; everything else
// is a shape nobody has seen this API produce, and accepting one on a guess is
// how a mis-parsed amount becomes a mis-paid member.
//
// A negative amount is ordinary and passes through with its sign: an amended
// transaction really can be a correction, and this port's job is to record
// what the network said rather than to decide it was impossible.
func minorUnits(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("%w: it is empty", ErrNotAnAmount)
	}

	negative := false
	switch s[0] {
	case '-':
		negative, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	units, fraction, hasPoint := strings.Cut(s, ".")
	switch {
	case units == "" && fraction == "":
		return 0, fmt.Errorf("%w: %s carries no digits", ErrNotAnAmount, strconv.Quote(raw))
	case !allDigits(units) || !allDigits(fraction):
		// This is what catches an exponent, a thousands separator, a second
		// point and a currency symbol, in one check rather than four.
		return 0, fmt.Errorf("%w: %s is not a plain decimal number", ErrNotAnAmount, strconv.Quote(raw))
	case hasPoint && fraction == "":
		// "19." - accepted by some parsers as 19. Refused, because a money
		// field that ends in a point is a field something truncated.
		return 0, fmt.Errorf("%w: %s ends in a decimal point", ErrNotAnAmount, strconv.Quote(raw))
	case len(fraction) > minorUnitExponent:
		return 0, fmt.Errorf("%w: %s carries %d decimal places and this adapter reads %d; a third place means the currency is not the two-decimal one this network was assumed to trade in, and rounding it away would hide that",
			ErrNotAnAmount, strconv.Quote(raw), len(fraction), minorUnitExponent)
	}

	// Pad rather than scale: "19.5" is 1950 minor units, not 195.
	padded := units + fraction + strings.Repeat("0", minorUnitExponent-len(fraction))
	minor, err := strconv.ParseInt(padded, 10, 64)
	if err != nil {
		// The only way here is a value too large for an int64, the digits
		// having already been checked. That is a real refusal rather than a
		// theoretical one: the column is bigint too, so an amount that
		// overflows here would not have been storable either.
		return 0, fmt.Errorf("%w: %s does not fit in the minor units the evidence row stores", ErrNotAnAmount, strconv.Quote(raw))
	}
	if negative {
		minor = -minor
	}
	return minor, nil
}

// allDigits reports whether every byte is an ASCII digit. The empty string
// is all digits, which is what makes "19" and ".51" both readable while
// "19." is refused above by its own case.
func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
