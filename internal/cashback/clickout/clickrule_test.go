package clickout_test

// What the click rule is, and what "off" means for each of its halves.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
)

// TestARuleOfZeroBoundsNothing covers the off switch. A deployment that has
// turned the rule off gets every click it asks for - not a limit of zero,
// which would refuse every one of them.
func TestARuleOfZeroBoundsNothing(t *testing.T) {
	t.Parallel()

	rates := &fakeRates{byAccount: counted(9999, clickedAt.Add(-time.Minute))}
	if err := limiter(t, rates, clickout.ClickRule{}).Allow(t.Context(), uuid.New(), clickout.NewContextDigest("ua/1.0"), clickedAt); err != nil {
		t.Fatalf("Allow() with no rule = %v, want it to allow", err)
	}
	if rates.accountReads != 0 || rates.contextReads != 0 {
		t.Errorf("a rule that bounds nothing still counted (%d, %d) time(s)", rates.accountReads, rates.contextReads)
	}
	if (clickout.ClickRule{}).Applies() {
		t.Error("an empty rule reports that it applies")
	}
	if !clickout.DefaultClickRule().Applies() {
		t.Error("the default rule reports that it applies to nothing")
	}
}
