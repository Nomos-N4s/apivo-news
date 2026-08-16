package translation_test

import (
	"errors"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// alphaCaps is the budget the alpha runs at: $0.02 an article, $25 a month.
var alphaCaps = translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000}

func TestCapsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		caps    translation.Caps
		wantErr bool
	}{
		{name: "the alpha budget", caps: alphaCaps},
		{name: "unset caps are not an unlimited budget", caps: translation.Caps{}, wantErr: true},
		{name: "zero ceiling refuses every call", caps: translation.Caps{PerArticleMicroUSD: 0, MonthlyMicroUSD: 25_000_000}, wantErr: true},
		{name: "zero cap halts the month before it starts", caps: translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 0}, wantErr: true},
		{name: "negative ceiling", caps: translation.Caps{PerArticleMicroUSD: -1, MonthlyMicroUSD: 25_000_000}, wantErr: true},
		{name: "negative cap", caps: translation.Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: -1}, wantErr: true},
		{name: "one article could halt the month", caps: translation.Caps{PerArticleMicroUSD: 30_000, MonthlyMicroUSD: 25_000}, wantErr: true},
		{name: "a ceiling equal to the cap is a one-article month, but legal", caps: translation.Caps{PerArticleMicroUSD: 25_000, MonthlyMicroUSD: 25_000}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.caps.Validate()
			if tt.wantErr && !errors.Is(err, translation.ErrCapsNotConfigured) {
				t.Fatalf("Validate() = %v, want ErrCapsNotConfigured", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestCapsOverCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cost int64
		want bool
	}{
		{name: "a free call", cost: 0},
		{name: "well under", cost: 1_500},
		{name: "exactly at the ceiling is still affordable", cost: 20_000},
		{name: "one micro-USD over", cost: 20_001, want: true},
		{name: "a runaway call", cost: 5_000_000, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := alphaCaps.OverCeiling(tt.cost); got != tt.want {
				t.Fatalf("OverCeiling(%d) = %v, want %v", tt.cost, got, tt.want)
			}
		})
	}
}

func TestCapsReached(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spent int64
		want  bool
	}{
		{name: "an empty month", spent: 0},
		{name: "one micro-USD short", spent: 24_999_999},
		{name: "exactly on the cap: the last call the month gets", spent: 25_000_000, want: true},
		{name: "past it, as an over-ceiling call can leave a month", spent: 30_000_000, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := alphaCaps.Reached(tt.spent); got != tt.want {
				t.Fatalf("Reached(%d) = %v, want %v", tt.spent, got, tt.want)
			}
		})
	}
}
