package earnings_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// confirmedEntries builds entries in the order ConfirmedEntriesFor returns
// them: oldest first.
func confirmedEntries(t *testing.T, minors ...int64) []earnings.Entry {
	t.Helper()
	entries := make([]earnings.Entry, 0, len(minors))
	for _, minor := range minors {
		entries = append(entries, earnings.Entry{
			ID:     uuid.New(),
			State:  earnings.StateConfirmed,
			Amount: eur(t, minor),
		})
	}
	return entries
}

// TestAWithdrawalTakesTheOldestEntriesFirst is the rule in one test: money
// leaves in the order it was earned.
func TestAWithdrawalTakesTheOldestEntriesFirst(t *testing.T) {
	entries := confirmedEntries(t, 1000, 2000, 3000)

	taking, total, err := earnings.Covering(entries, eur(t, 2500))
	if err != nil {
		t.Fatalf("Covering(): %v", err)
	}
	if len(taking) != 2 {
		t.Fatalf("took %d entries, want the two oldest", len(taking))
	}
	if taking[0].ID != entries[0].ID || taking[1].ID != entries[1].ID {
		t.Errorf("took %s and %s, want the two oldest %s and %s",
			taking[0].ID, taking[1].ID, entries[0].ID, entries[1].ID)
	}
	if want := eur(t, 3000); total != want {
		t.Errorf("reserved %s, want %s", total, want)
	}
}

// TestAWithdrawalStopsAtTheEntryThatCoversIt. Everything after the covering
// entry stays confirmed, so a member who asks for a little does not have
// their whole balance locked up awaiting an operator.
func TestAWithdrawalStopsAtTheEntryThatCoversIt(t *testing.T) {
	entries := confirmedEntries(t, 5000, 5000, 5000)

	taking, total, err := earnings.Covering(entries, eur(t, 100))
	if err != nil {
		t.Fatalf("Covering(): %v", err)
	}
	if len(taking) != 1 {
		t.Fatalf("took %d entries for 1.00, want one", len(taking))
	}
	if want := eur(t, 5000); total != want {
		t.Errorf("reserved %s, want %s - an entry is whole or it is not taken", total, want)
	}
}

// TestAnExactAmountTakesNoEntryItDoesNotNeed is the boundary the >= is
// written for: the running total reaching the amount exactly must stop there
// and not reach for one more.
func TestAnExactAmountTakesNoEntryItDoesNotNeed(t *testing.T) {
	entries := confirmedEntries(t, 1000, 2000, 4000)

	taking, total, err := earnings.Covering(entries, eur(t, 3000))
	if err != nil {
		t.Fatalf("Covering(): %v", err)
	}
	if len(taking) != 2 {
		t.Fatalf("took %d entries for an amount two cover exactly, want two", len(taking))
	}
	if want := eur(t, 3000); total != want {
		t.Errorf("reserved %s, want exactly the %s asked for", total, want)
	}
}

// TestTooLittleConfirmedMoneyReportsTheShortfall. US4 scenario 1: the refusal
// carries the arithmetic, because that is what reaches the member.
func TestTooLittleConfirmedMoneyReportsTheShortfall(t *testing.T) {
	entries := confirmedEntries(t, 1000, 2000)

	_, _, err := earnings.Covering(entries, eur(t, 5000))
	var short earnings.ShortConfirmedBalance
	if !errors.As(err, &short) {
		t.Fatalf("Covering() = %v, want a %T", err, short)
	}
	if want := eur(t, 3000); short.Have != want {
		t.Errorf("Have = %s, want %s", short.Have, want)
	}
	if want := eur(t, 5000); short.Want != want {
		t.Errorf("Want = %s, want %s", short.Want, want)
	}
	if want := eur(t, 2000); short.Short != want {
		t.Errorf("Short = %s, want %s", short.Short, want)
	}
}

// TestNoConfirmedMoneyIsShortOfTheWholeAmount. The empty set is the ordinary
// first request, and it must report a shortfall rather than an empty
// reservation somebody could post.
func TestNoConfirmedMoneyIsShortOfTheWholeAmount(t *testing.T) {
	_, _, err := earnings.Covering(nil, eur(t, 5000))
	var short earnings.ShortConfirmedBalance
	if !errors.As(err, &short) {
		t.Fatalf("Covering(nil) = %v, want a %T", err, short)
	}
	if want := eur(t, 5000); short.Short != want {
		t.Errorf("Short = %s, want the whole amount %s", short.Short, want)
	}
	if !short.Have.IsZero() {
		t.Errorf("Have = %s, want zero", short.Have)
	}
}

// TestAWithdrawalReservesOneCurrency. C-6: an account holds one currency, so
// an entry in another is a different balance. Refused rather than skipped -
// skipping would answer about a set the caller did not pass.
func TestAWithdrawalReservesOneCurrency(t *testing.T) {
	other, err := money.New(5000, money.Currency("GBP"))
	if err != nil {
		t.Fatalf("money.New(5000, GBP): %v", err)
	}
	entries := confirmedEntries(t, 1000)
	entries = append(entries, earnings.Entry{ID: uuid.New(), State: earnings.StateConfirmed, Amount: other})

	if _, _, err := earnings.Covering(entries, eur(t, 4000)); err == nil {
		t.Fatal("Covering() over two currencies succeeded, want a refusal")
	}
}

// TestReservingNothingIsRefused. A withdrawal for zero is not a withdrawal,
// and one for a negative amount is a payment in.
func TestReservingNothingIsRefused(t *testing.T) {
	entries := confirmedEntries(t, 1000)

	for _, minor := range []int64{0, -100} {
		if _, _, err := earnings.Covering(entries, eur(t, minor)); !errors.Is(err, earnings.ErrNothingToReserve) {
			t.Errorf("Covering(%d) = %v, want one wrapping %v", minor, err, earnings.ErrNothingToReserve)
		}
	}
	if _, _, err := earnings.Covering(entries, money.Amount{}); !errors.Is(err, earnings.ErrNothingToReserve) {
		t.Errorf("Covering(zero value) = %v, want one wrapping %v", err, earnings.ErrNothingToReserve)
	}
}

// TestShortConfirmedBalanceSaysAllThreeFigures. "Insufficient" without them
// is a message somebody has to reproduce to act on, and the shortfall is
// what reaches the member (US4 scenario 1).
func TestShortConfirmedBalanceSaysAllThreeFigures(t *testing.T) {
	err := earnings.ShortConfirmedBalance{
		Want:  eur(t, 5000),
		Have:  eur(t, 3000),
		Short: eur(t, 2000),
	}
	// Minor units, as money.Amount.String renders them: this string is for a
	// log, and the member-facing figures travel as {minor, currency}.
	for _, want := range []string{"5000 EUR", "3000 EUR", "2000 EUR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to name %s", err.Error(), want)
		}
	}
}
