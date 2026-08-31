// The tests for rail.go. Nothing here touches a database: the port is a
// contract, and what is pinned is that an instruction no rail could act on is
// refused before any rail is contacted, and that a failure's classification
// survives being wrapped - because a caller reading the wrong one either pays
// a member twice or leaves them unpaid.

package payout_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// anInstruction is a payment the domain would hand a rail, and the value
// every case below varies one field of.
func anInstruction() payout.Instruction {
	return payout.Instruction{
		IdempotencyKey: "withdrawal:6f9619ff-8b86-d011-b42d-00c04fc964ff",
		Amount:         money.Amount{Minor: 1250, Currency: "EUR"},
		Destination:    payout.DestinationRef{Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1"},
		Descriptor:     "Example Cashback",
	}
}

func TestAnInstructionNoRailCouldActOnIsRefusedFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil func(*payout.Instruction)
		// why names what the refusal is protecting, so a later reader
		// deleting a case has to argue with the reason rather than the rule.
		why string
	}{
		{
			name: "a payment somebody could act on",
			why:  "the reference case",
		},
		{
			name:  "no idempotency key",
			spoil: func(i *payout.Instruction) { i.IdempotencyKey = "  " },
			why:   "without one a retry is a second payment",
		},
		{
			name:  "an amount of nothing",
			spoil: func(i *payout.Instruction) { i.Amount.Minor = 0 },
			why:   "a rail is not how money comes back",
		},
		{
			name:  "a negative amount",
			spoil: func(i *payout.Instruction) { i.Amount.Minor = -1 },
			why:   "payout_amount_positive says the same in the schema",
		},
		{
			name:  "no currency",
			spoil: func(i *payout.Instruction) { i.Amount.Currency = "" },
			why:   "money is a pair and never a bare number (C-6)",
		},
		{
			name:  "a rail nothing could carry it on",
			spoil: func(i *payout.Instruction) { i.Destination.Kind = "cheque" },
			why:   "the kind is what picks the rail",
		},
		{
			name:  "nothing saying where the details live",
			spoil: func(i *payout.Instruction) { i.Destination.DetailsRef = "" },
			why:   "a rail cannot resolve a destination it was not pointed at",
		},
		{
			name:  "nothing to show on the statement",
			spoil: func(i *payout.Instruction) { i.Descriptor = " " },
			why:   "a line a member cannot recognise is how a payment becomes a chargeback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			instruction := anInstruction()
			if tt.spoil == nil {
				if err := instruction.Validate(); err != nil {
					t.Fatalf("Validate() refused a payment somebody could act on: %v", err)
				}
				return
			}
			tt.spoil(&instruction)
			if err := instruction.Validate(); !errors.Is(err, payout.ErrInvalidInstruction) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidInstruction (%s)", err, tt.why)
			}
		})
	}
}

// TestAClassificationSurvivesWrapping is the property the whole classification
// rests on. A rail wraps its own error in one of the two, a caller a few
// frames up asks which it is, and getting the answer wrong costs real money in
// both directions.
func TestAClassificationSurvivesWrapping(t *testing.T) {
	t.Parallel()

	cause := errors.New("the rail hung up")

	retryable := fmt.Errorf("submitting: %w", payout.Retryable(cause))
	if !errors.Is(retryable, payout.ErrRailRetryable) {
		t.Errorf("a wrapped retryable failure = %v, which a caller would not retry", retryable)
	}
	if errors.Is(retryable, payout.ErrRailTerminal) {
		t.Errorf("a retryable failure also reads as terminal; a reservation would be released for a payment in flight")
	}
	if !errors.Is(retryable, cause) {
		t.Errorf("the classification lost the cause, so nobody can see what happened")
	}

	terminal := fmt.Errorf("submitting: %w", payout.Terminal(cause))
	if !errors.Is(terminal, payout.ErrRailTerminal) {
		t.Errorf("a wrapped terminal failure = %v, which a caller would retry forever", terminal)
	}
	if errors.Is(terminal, payout.ErrRailRetryable) {
		t.Errorf("a terminal failure also reads as retryable; a member's balance would be held for a payment that never happens")
	}
}

// TestAClassificationWithNoCauseStillClassifies: a rail that has nothing to
// add must still say which kind of failure it had, and the bare sentinel is
// what it returns.
func TestAClassificationWithNoCauseStillClassifies(t *testing.T) {
	t.Parallel()

	if !errors.Is(payout.Retryable(nil), payout.ErrRailRetryable) {
		t.Error("Retryable(nil) does not read as retryable")
	}
	if !errors.Is(payout.Terminal(nil), payout.ErrRailTerminal) {
		t.Error("Terminal(nil) does not read as terminal")
	}
}

func TestARailReferenceIsSomethingTheRailActuallyGave(t *testing.T) {
	t.Parallel()

	ref, err := payout.NewRailReference("PMT-2026-0001")
	if err != nil {
		t.Fatalf("NewRailReference(): %v", err)
	}
	if ref.Ref() != "PMT-2026-0001" {
		t.Errorf("Ref() = %q, want the reference the rail gave", ref.Ref())
	}
	if err := ref.Validate(); err != nil {
		t.Errorf("Validate() on a real reference: %v", err)
	}

	for _, blank := range []string{"", "   "} {
		if _, err := payout.NewRailReference(blank); !errors.Is(err, payout.ErrInvalidRailReference) {
			t.Errorf("NewRailReference(%q) = %v, want one wrapping ErrInvalidRailReference", blank, err)
		}
	}
	// The zero value is refused too: a payment recorded before its rail
	// answered would be one nobody could follow.
	var never payout.RailReference
	if err := never.Validate(); !errors.Is(err, payout.ErrInvalidRailReference) {
		t.Errorf("the zero reference validates: %v", err)
	}
	if never.String() != "(none)" {
		t.Errorf("the zero reference prints as %q, want it to read as absent", never.String())
	}
}

func TestRailStatusIsTheSchemasClosedSet(t *testing.T) {
	t.Parallel()

	for _, valid := range []payout.RailStatus{payout.StatusSubmitted, payout.StatusSettled, payout.StatusFailed} {
		if !valid.Valid() {
			t.Errorf("%q is not accepted, but payout_state_known accepts it", valid)
		}
	}
	for _, invalid := range []payout.RailStatus{"", "pending", "SETTLED", "sent"} {
		if invalid.Valid() {
			t.Errorf("%q is accepted, but payout_state_known would refuse it", invalid)
		}
	}
}

// TestARailIsToldNothingAboutTheMember is contract rule 3. A rail that cannot
// name the member cannot log the member, leak the member, or make a decision
// about the member - and the narrowing happens in one place so a caller
// cannot assemble a wider reference by hand.
func TestARailIsToldNothingAboutTheMember(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	recorded, err := d.Record(ctx, payout.NewDestination{
		AccountID: member, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	ref := recorded.ToRef()
	if ref.Kind != payout.KindSEPA {
		t.Errorf("Kind = %q, want the destination's own", ref.Kind)
	}
	if ref.DetailsRef != "vault:member/sepa/1" {
		t.Errorf("DetailsRef = %q, want the destination's own", ref.DetailsRef)
	}
	// The whole check: rendering it as text carries neither the member nor
	// the destination row, so nothing a rail logs can name either.
	rendered := fmt.Sprintf("%+v", ref)
	if strings.Contains(rendered, member.String()) {
		t.Errorf("the reference a rail sees names the member: %s", rendered)
	}
	if strings.Contains(rendered, recorded.ID.String()) {
		t.Errorf("the reference a rail sees names the destination row: %s", rendered)
	}
	if err := ref.Validate(); err != nil {
		t.Errorf("a real destination produced a reference no rail could act on: %v", err)
	}
}
