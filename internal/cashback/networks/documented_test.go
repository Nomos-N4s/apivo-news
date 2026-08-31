// The tests for documented.go: that a declaration a cashback.network row
// would refuse is refused here instead, and that the one unit conversion on
// it goes the right way.

package networks_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// aDeclaration is Awin's numbers, which are the reference case every rule
// below is varied from.
func aDeclaration() networks.Documented {
	return networks.Documented{
		ID:                 "awin",
		DisplayName:        "Awin",
		ClickRefParam:      "clickref",
		MaxQueryWindowDays: 31,
		RateLimitPerMinute: 20,
	}
}

func TestADeclarationTheNetworkTableWouldRefuseIsRefusedFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		declare func() networks.Documented
		wantErr bool
	}{
		{name: "what a network publishes", declare: aDeclaration},
		{
			name:    "no network id",
			declare: func() networks.Documented { d := aDeclaration(); d.ID = ""; return d },
			wantErr: true,
		},
		{
			name:    "an id the column's format check would refuse",
			declare: func() networks.Documented { d := aDeclaration(); d.ID = "Awin"; return d },
			wantErr: true,
		},
		{
			name:    "no display name",
			declare: func() networks.Documented { d := aDeclaration(); d.DisplayName = "  "; return d },
			wantErr: true,
		},
		{
			name:    "no click-reference parameter, which loses every click",
			declare: func() networks.Documented { d := aDeclaration(); d.ClickRefParam = ""; return d },
			wantErr: true,
		},
		{
			name:    "a query window of no days",
			declare: func() networks.Documented { d := aDeclaration(); d.MaxQueryWindowDays = 0; return d },
			wantErr: true,
		},
		{
			name:    "a rate that permits nothing",
			declare: func() networks.Documented { d := aDeclaration(); d.RateLimitPerMinute = 0; return d },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.declare().Validate()
			if tt.wantErr {
				if !errors.Is(err, networks.ErrInvalidDocumentedNetwork) {
					t.Fatalf("Validate() = %v, want one wrapping ErrInvalidDocumentedNetwork", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() refused what a network publishes: %v", err)
			}
		})
	}
}

// TestTheDeclarationBecomesLimitsInTheRightUnits pins the two conversions a
// declaration carries: whole days become a duration, and the rate stays per
// minute rather than being divided somewhere it should not be.
func TestTheDeclarationBecomesLimitsInTheRightUnits(t *testing.T) {
	t.Parallel()

	limits := aDeclaration().Limits()
	if want := 31 * 24 * time.Hour; limits.MaxWindow != want {
		t.Errorf("MaxWindow = %s, want %s", limits.MaxWindow, want)
	}
	if want := 20; limits.RequestsPerMinute != want {
		t.Errorf("RequestsPerMinute = %d, want %d", limits.RequestsPerMinute, want)
	}
	if err := limits.Validate(); err != nil {
		t.Errorf("the limits a published declaration produces describe no queryable network: %v", err)
	}
}
