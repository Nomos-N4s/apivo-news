package earnings_test

// What a reversal writes, and what it must leave alone (T071, SC-010, C-3).
//
// The property the whole design rests on is a negative: the credit being
// undone is not touched. Everything else here exists to make sure the second
// entry is a faithful record of the first rather than an approximation of it.

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// creditedEntry is a credit sitting in the given state, ready to be undone.
func creditedEntry(t *testing.T, state earnings.State) earnings.Entry {
	t.Helper()
	amount, err := money.New(3000, "EUR")
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return earnings.Entry{
		ID:     uuid.New(),
		Member: uuid.New(),
		Brand:  "apivo-de",
		Report: uuid.New(),
		Click:  uuid.New(),
		State:  state,
		Amount: amount,
	}
}

// reverser builds the reverser over a working machine.
func reverser(t *testing.T, entries earnings.EntryStore, ledger *fakeLedger) *earnings.Reversals {
	t.Helper()
	r, err := earnings.NewReversals(machine(t, entries, ledger))
	if err != nil {
		t.Fatalf("NewReversals(): %v", err)
	}
	return r
}

// TestAReversalNeverTouchesTheCreditItUndoes is the property. The schema says
// it too - entry_guard makes reversed terminal and the migration's own test
// refuses a reversal that changed the original - but by then the money has
// moved, so it has to be true here first.
func TestAReversalNeverTouchesTheCreditItUndoes(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	entries, ledger := &fakeEntries{row: anEntry(original.Member, earnings.StateReversed)}, &fakeLedger{}

	if _, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
		Original: original,
		Report:   uuid.New(),
		Reason:   "the network reversed the sale",
	}); err != nil {
		t.Fatalf("Reverse(): %v", err)
	}

	if len(entries.moves) != 0 {
		t.Errorf("reversing moved the original entry %d time(s); it must be left exactly as it was", len(entries.moves))
	}
	if len(entries.transitions) != 1 {
		t.Fatalf("recorded %d transition(s), want one - the reversal's own opening", len(entries.transitions))
	}
	if entries.transitions[0].FromState.Valid {
		t.Errorf("the reversal's transition comes from %q; a reversal is born, so it comes from nothing",
			entries.transitions[0].FromState.String)
	}
}

// TestTheReversalCarriesTheOriginalsFacts. It undoes exactly that credit, so
// member, brand, click and amount are the original's; only the evidence
// differs, and it must.
func TestTheReversalCarriesTheOriginalsFacts(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	reversingReport := uuid.New()
	entries, ledger := &fakeEntries{row: anEntry(original.Member, earnings.StateReversed)}, &fakeLedger{}

	if _, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
		Original: original, Report: reversingReport,
	}); err != nil {
		t.Fatalf("Reverse(): %v", err)
	}

	created := entries.created
	if uuid.UUID(created.AccountID.Bytes) != original.Member {
		t.Errorf("the reversal credits %v, want the original's member %v",
			uuid.UUID(created.AccountID.Bytes), original.Member)
	}
	if created.BrandID != original.Brand {
		t.Errorf("the reversal names brand %q, want %q", created.BrandID, original.Brand)
	}
	if created.AmountMinor != original.Amount.Minor {
		t.Errorf("the reversal is %d, want the original's %d", created.AmountMinor, original.Amount.Minor)
	}
	if created.State != string(earnings.StateReversed) {
		t.Errorf("the reversal opens as %q, want reversed", created.State)
	}
	if uuid.UUID(created.ReversalOfID.Bytes) != original.ID {
		t.Errorf("the reversal undoes %v, want %v", uuid.UUID(created.ReversalOfID.Bytes), original.ID)
	}
	// The evidence is the only thing that differs, and entry_one_per_report
	// is why it has to.
	if uuid.UUID(created.NetworkTransactionID.Bytes) != reversingReport {
		t.Errorf("the reversal cites %v, want the superseding report %v",
			uuid.UUID(created.NetworkTransactionID.Bytes), reversingReport)
	}
}

// TestTheMoneyLeavesTheStageTheCreditReached is the case that would go wrong
// quietly. A confirmed credit and a pending one sit in different accounts;
// debiting the wrong one leaves the member's confirmed balance untouched
// while another bucket goes negative.
func TestTheMoneyLeavesTheStageTheCreditReached(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state earnings.State
		stage wallet.Stage
	}{
		{earnings.StateHeld, wallet.StageHeld},
		{earnings.StatePending, wallet.StagePending},
		{earnings.StateConfirmed, wallet.StageConfirmed},
		{earnings.StateReserved, wallet.StageReserved},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			original := creditedEntry(t, tc.state)
			entries := &fakeEntries{row: anEntry(original.Member, earnings.StateReversed)}
			ledger := &fakeLedger{}

			if _, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
				Original: original, Report: uuid.New(),
			}); err != nil {
				t.Fatalf("Reverse(): %v", err)
			}

			var sawStage, sawHouse bool
			for _, ref := range ledger.ensured {
				if _, stage, ok := ref.Member(); ok {
					sawStage = true
					if stage != tc.stage {
						t.Errorf("reversing a %s credit debited the %s account, want %s", tc.state, stage, tc.stage)
					}
				}
				if name, ok := ref.House(); ok {
					sawHouse = true
					// Back where it came from, never the clawback account:
					// that one absorbs a loss already paid out (Q3), which
					// this is not.
					if name != receivable {
						t.Errorf("a reversal returned money to %q, want %q", name, receivable)
					}
				}
			}
			if !sawStage || !sawHouse {
				t.Errorf("the reversal touched stage=%v house=%v, want both", sawStage, sawHouse)
			}
		})
	}
}

// TestAReversalNeedsEvidenceOfItsOwn. A status change is a new superseding
// row, so the reversing report always exists; citing the original's would be
// refused by entry_one_per_report only AFTER the transfer had posted, which
// is a reconciliation problem rather than an answer.
func TestAReversalNeedsEvidenceOfItsOwn(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	for _, tc := range []struct {
		name   string
		report uuid.UUID
	}{
		{name: "no report at all", report: uuid.Nil},
		{name: "the report it undoes", report: original.Report},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, ledger := &fakeEntries{}, &fakeLedger{}

			_, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
				Original: original, Report: tc.report,
			})

			if !errors.Is(err, earnings.ErrNoReversingReport) {
				t.Fatalf("Reverse() error = %v, want one wrapping %v", err, earnings.ErrNoReversingReport)
			}
			if len(ledger.posted) != 0 {
				t.Errorf("posted %d transfer(s) before refusing, want none", len(ledger.posted))
			}
		})
	}
}

// TestAnEntryHoldingNothingCannotBeReversed. Paid money has left and
// reversed money is already undone; neither has anything in a stage account
// to take back, and entry_reversed_at_most_once refuses the second reversal
// in the schema.
func TestAnEntryHoldingNothingCannotBeReversed(t *testing.T) {
	t.Parallel()

	for _, state := range []earnings.State{earnings.StatePaid, earnings.StateReversed} {
		original := creditedEntry(t, state)
		entries, ledger := &fakeEntries{}, &fakeLedger{}

		_, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
			Original: original, Report: uuid.New(),
		})

		if !errors.Is(err, earnings.ErrNotReversible) {
			t.Errorf("Reverse() of a %s entry error = %v, want one wrapping %v",
				state, err, earnings.ErrNotReversible)
		}
		if len(ledger.posted) != 0 {
			t.Errorf("reversing a %s entry posted %d transfer(s), want none", state, len(ledger.posted))
		}
	}
}

// TestNothingIsWrittenWhenTheReversingTransferIsRefused. Same rule as every
// other move: a state recorded with no posting behind it is the disagreement
// C-1 exists to prevent.
func TestNothingIsWrittenWhenTheReversingTransferIsRefused(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	entries := &fakeEntries{}
	ledger := &fakeLedger{postErr: errors.New("ledger unreachable")}

	_, err := reverser(t, entries, ledger).Reverse(t.Context(), &fakeOutbox{}, earnings.Reversal{
		Original: original, Report: uuid.New(),
	})

	if !errors.Is(err, earnings.ErrNotPosted) {
		t.Fatalf("Reverse() error = %v, want one wrapping %v", err, earnings.ErrNotPosted)
	}
	if entries.creations != 0 || len(entries.transitions) != 0 {
		t.Errorf("a refused transfer still wrote %d reversing entry(ies) and %d transition(s), want none",
			entries.creations, len(entries.transitions))
	}
}

// TestAReverserIsRefusedWithoutAMachine covers the construction refusal. It
// shares ErrNoEntries with the confirmer rather than declaring a second name
// for the same fact: both are "there is no machine to move entries with", and
// two errors saying that would be two things a caller has to match on.
func TestAReverserIsRefusedWithoutAMachine(t *testing.T) {
	t.Parallel()

	if _, err := earnings.NewReversals(nil); !errors.Is(err, earnings.ErrNoEntries) {
		t.Errorf("NewReversals(nil) error = %v, want %v", err, earnings.ErrNoEntries)
	}
}

// TestAReversalAnnouncesBothFacts. Two things happened and two are
// announced: an entry that did not exist now does, and it moved into the
// state it was born in. A consumer following creations would otherwise be
// blind to money owed back, and one following moves would see an entry that
// appeared already reversed with no transfer to trace it by.
func TestAReversalAnnouncesBothFacts(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	entries := &fakeEntries{row: anEntry(original.Member, earnings.StateReversed)}
	ledger, out := &fakeLedger{}, &fakeOutbox{}

	reversing, err := reverser(t, entries, ledger).Reverse(t.Context(), out, earnings.Reversal{
		Original: original,
		Report:   uuid.New(),
		Reason:   "the network took it back",
	})
	if err != nil {
		t.Fatalf("Reverse(): %v", err)
	}

	created := out.only(t, earnings.TypeEntryCreated)
	if created.Subject != reversing.ID.String() {
		t.Errorf("the creation is about %q, want the reversing entry %s", created.Subject, reversing.ID)
	}
	// Born reversed, and said so: an entry announced as held would have a
	// consumer counting money toward a balance the schema says is gone.
	if created.Payload["state"] != string(earnings.StateReversed) {
		t.Errorf("the creation says the entry is %v, want %s", created.Payload["state"], earnings.StateReversed)
	}
	if created.Payload["account_id"] != original.Member.String() {
		t.Errorf("the creation names member %v, want %s", created.Payload["account_id"], original.Member)
	}

	moved := out.only(t, earnings.TypeEntryStateChanged)
	if moved.Subject != reversing.ID.String() {
		t.Errorf("the move is about %q, want the reversing entry %s", moved.Subject, reversing.ID)
	}
	// From nothing: the reversing entry came into being reversed, and the
	// stream says so rather than inventing a state it was never in.
	if moved.Payload["from"] != "" {
		t.Errorf("the move says it came from %v, want nothing", moved.Payload["from"])
	}
	if moved.Payload["to"] != string(earnings.StateReversed) {
		t.Errorf("the move says it went to %v, want %s", moved.Payload["to"], earnings.StateReversed)
	}
	// Never the original: the credit being undone is not touched, and it is
	// not announced as having moved either.
	for _, e := range out.events {
		if e.Subject == original.ID.String() {
			t.Errorf("%s was announced about the original entry, which this reversal must leave alone", e.Type)
		}
	}
}

// TestAReversalThatCannotBeAnnouncedFails, for the reason every other
// announcement failure is fatal: the append shares the caller's transaction,
// so carrying on would commit nothing while reporting success.
func TestAReversalThatCannotBeAnnouncedFails(t *testing.T) {
	t.Parallel()

	original := creditedEntry(t, earnings.StateConfirmed)
	entries := &fakeEntries{row: anEntry(original.Member, earnings.StateReversed)}
	ledger := &fakeLedger{}
	out := &fakeOutbox{err: errOutboxRefused}

	_, err := reverser(t, entries, ledger).Reverse(t.Context(), out, earnings.Reversal{
		Original: original,
		Report:   uuid.New(),
	})

	if !errors.Is(err, earnings.ErrNotAnnounced) {
		t.Fatalf("Reverse() error = %v, want one wrapping %v", err, earnings.ErrNotAnnounced)
	}
}
