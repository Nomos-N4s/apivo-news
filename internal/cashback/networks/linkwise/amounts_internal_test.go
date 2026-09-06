package linkwise

// Reading Linkwise's decimal money strings as exact minor units (C-6).
//
// Internal, because the parser is private and because what is being pinned is
// arithmetic rather than an interface: the failure this file exists to catch
// is one cent, on some values and not others, reconciling against a network
// statement as a drift nobody can source.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestAmountsAreReadExactly(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		raw  string
		want int64
	}{
		// The two the recording actually carries.
		{raw: "19.51", want: 1951},
		{raw: "2.93", want: 293},
		// A short fraction is PADDED, not scaled: "19.5" is nineteen euros
		// fifty, so 1950 minor units and not 195.
		{raw: "19.5", want: 1950},
		{raw: "19", want: 1900},
		{raw: "0", want: 0},
		{raw: "0.00", want: 0},
		{raw: "0.07", want: 7},
		{raw: ".51", want: 51},
		// A correction really can be negative, and the sign passes through:
		// this port records what the network said rather than deciding it
		// was impossible.
		{raw: "-19.51", want: -1951},
		{raw: "+19.51", want: 1951},
		{raw: "-0.01", want: -1},
		// Whitespace around the value, which a hand-edited fixture picks up.
		{raw: "  19.51  ", want: 1951},
		// Large but storable: the evidence row's column is a bigint too.
		{raw: "92233720368547758.07", want: math.MaxInt64},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := minorUnits(tt.raw)
			if err != nil {
				t.Fatalf("minorUnits(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("minorUnits(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// TestNoFloatIsEverConstructed is the whole reason this parser exists,
// expressed as a property rather than as a claim about the implementation.
//
// It walks ten thousand consecutive hundredths at two magnitudes and requires
// the exact answer for every one. The two magnitudes are what make it
// discriminating, because the two float routes fail at different places:
//
//   - Truncating - float64(x)*100 cast to int64 - is wrong on 573 of the
//     first block. The first is 0.29, whose hundred-fold is
//     28.999999999999996.
//   - Rounding is exact on the first block and wrong on half of the second:
//     a float64 has no room left for hundredths around 1e14, so the route
//     that passes every test written at the scale of a shopping basket stops
//     being exact at the scale of a year's turnover.
func TestNoFloatIsEverConstructed(t *testing.T) {
	t.Parallel()

	for _, base := range []int64{
		// Retail sizes, where a truncating float is already wrong.
		0,
		// Above where a float64 can still hold hundredths exactly, where a
		// ROUNDING float is wrong too.
		100 * 1e14,
	} {
		t.Run(strconv.FormatInt(base, 10), func(t *testing.T) {
			t.Parallel()
			var wrong int
			for i := range int64(10_000) {
				want := base + i
				raw := strconv.FormatInt(want/100, 10) + "." + fmt.Sprintf("%02d", want%100)
				got, err := minorUnits(raw)
				if err != nil {
					t.Fatalf("minorUnits(%q): %v", raw, err)
				}
				if got != want {
					wrong++
					if wrong <= 3 {
						t.Errorf("minorUnits(%q) = %d, want %d", raw, got, want)
					}
				}
			}
			if wrong > 0 {
				t.Errorf("%d of 10000 hundredths were read wrongly", wrong)
			}
		})
	}
}

// TestAThirdDecimalPlaceIsRefusedRatherThanRounded.
//
// The exponent is a fact about the currency, and this adapter states two
// because Linkwise trades in Greece and SE Europe - EUR, RON, BGN, RSD, all
// two-decimal. A third place is therefore evidence that the assumption is
// wrong for the programme that produced it, and rounding it away would
// destroy exactly the evidence needed to find that out.
func TestAThirdDecimalPlaceIsRefusedRatherThanRounded(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"19.512", "0.001", "-1.234", "1.0000"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := minorUnits(raw)
			if !errors.Is(err, ErrNotAnAmount) {
				t.Fatalf("minorUnits(%q) = %d, %v; want ErrNotAnAmount", raw, got, err)
			}
			if !strings.Contains(err.Error(), "decimal places") {
				t.Errorf("the refusal does not say what is wrong: %v", err)
			}
		})
	}
}

func TestWhatIsNotAnAmountIsRefused(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, raw string }{
		{name: "empty", raw: ""},
		{name: "only whitespace", raw: "   "},
		{name: "only a sign", raw: "-"},
		{name: "only a point", raw: "."},
		{name: "ending in a point", raw: "19."},
		{name: "two points", raw: "19.5.1"},
		// Every one of these is accepted by strconv.ParseFloat.
		{name: "an exponent", raw: "1.9e1"},
		{name: "hexadecimal", raw: "0x13"},
		{name: "infinity", raw: "Inf"},
		{name: "not a number", raw: "NaN"},
		// And these are the shapes a network really does emit sometimes.
		{name: "a thousands separator", raw: "1,951.00"},
		{name: "a currency symbol", raw: "19.51 EUR"},
		{name: "a symbol in front", raw: "€19.51"},
		{name: "a trailing sign", raw: "19.51-"},
		{name: "an inner space", raw: "19. 51"},
		// Too large for the minor units the evidence row stores.
		{name: "beyond int64", raw: "92233720368547758.08"},
		{name: "absurd", raw: "999999999999999999999.99"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := minorUnits(tt.raw)
			if !errors.Is(err, ErrNotAnAmount) {
				t.Fatalf("minorUnits(%q) = %d, %v; want ErrNotAnAmount", tt.raw, got, err)
			}
			if got != 0 {
				t.Errorf("a refused value still produced %d minor units", got)
			}
		})
	}
}

// TestARefusalQuotesTheValue. A money field that will not parse is a field
// somebody has to look at, and "not a decimal amount" without the value sends
// them to the whole window.
func TestARefusalQuotesTheValue(t *testing.T) {
	t.Parallel()

	_, err := minorUnits("1,951.00")
	if err == nil {
		t.Fatal("a thousands separator was accepted")
	}
	if !strings.Contains(err.Error(), "1,951.00") {
		t.Errorf("the refusal does not quote the value: %v", err)
	}
}
