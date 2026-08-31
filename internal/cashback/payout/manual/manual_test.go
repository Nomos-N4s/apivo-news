// The tests for the manual rail. What is pinned is C-5 - the same
// instruction submitted twice lands on one payment - and the two things this
// rail must refuse rather than pretend about.

package manual_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/manual"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// anInstruction is a payment an operator would be asked to carry out.
func anInstruction() payout.Instruction {
	return payout.Instruction{
		IdempotencyKey: "withdrawal:6f9619ff-8b86-d011-b42d-00c04fc964ff",
		Amount:         money.Amount{Minor: 1250, Currency: "EUR"},
		Destination:    payout.DestinationRef{Kind: payout.KindManual, DetailsRef: "vault:member/manual/1"},
		Descriptor:     "Example Cashback",
	}
}

func TestTheRailNamesItself(t *testing.T) {
	t.Parallel()

	if got := manual.New().Kind(); got != payout.KindManual {
		t.Errorf("Kind() = %q, want %q", got, payout.KindManual)
	}
}

// TestTheSameInstructionAlwaysLandsOnTheSameReference is C-5 for this rail.
// A remembered set of keys could be lost with a process or disagree with
// itself across replicas; a derived reference cannot do either.
func TestTheSameInstructionAlwaysLandsOnTheSameReference(t *testing.T) {
	t.Parallel()

	rail := manual.New()
	first, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	again, err := manual.New().Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("the second Submit(): %v", err)
	}
	if first.Ref() != again.Ref() {
		t.Errorf("two submissions of one instruction gave %s and %s; the second is a second payment", first, again)
	}

	// And the KEY is the identity, not the instruction. A retry whose
	// descriptor was re-rendered, or that somehow carried a different
	// amount, is still the same payment - the withdrawal it was derived
	// from has not changed. A reference built from the whole instruction
	// would mint a second payment for a difference that means nothing.
	varied := anInstruction()
	varied.Descriptor = "Example Cashback (retry)"
	varied.Amount.Minor = 9900
	sameKey, err := rail.Submit(t.Context(), varied)
	if err != nil {
		t.Fatalf("Submit() of a varied retry: %v", err)
	}
	if sameKey.Ref() != first.Ref() {
		t.Errorf("a retry on the same key gave %s, want %s - a second reference is a second payment", sameKey, first)
	}

	// A different withdrawal is a different payment, or the key would be
	// merging two members' money into one transfer.
	other := anInstruction()
	other.IdempotencyKey = "withdrawal:00000000-0000-0000-0000-000000000001"
	third, err := rail.Submit(t.Context(), other)
	if err != nil {
		t.Fatalf("Submit() of another withdrawal: %v", err)
	}
	if third.Ref() == first.Ref() {
		t.Errorf("two different withdrawals both got %s", third)
	}
}

// TestTheReferenceSaysNobodyHasPaidYet: an operator reading a payout row has
// to tell "I have not done this" from "here is what the bank called it".
func TestTheReferenceSaysNobodyHasPaidYet(t *testing.T) {
	t.Parallel()

	ref, err := manual.New().Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	if !strings.HasPrefix(ref.Ref(), "manual:") {
		t.Errorf("Submit() gave %s, which does not read as a payment nobody has made yet", ref)
	}
	if err := ref.Validate(); err != nil {
		t.Errorf("the reference is not one the payout row would accept: %v", err)
	}
}

// TestStatusNeverGuesses. What moves a manual payout on is a person going to
// a bank, and they tell the system by recording the settlement against the
// payout row. A rail that answered "settled" because time had passed would be
// reporting money as delivered on the strength of a clock.
func TestStatusNeverGuesses(t *testing.T) {
	t.Parallel()

	rail := manual.New()
	ref, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	for i := range 3 {
		got, err := rail.Status(t.Context(), ref)
		if err != nil {
			t.Fatalf("Status() call %d: %v", i+1, err)
		}
		if got != payout.StatusSubmitted {
			t.Errorf("Status() = %q, want %q - this rail cannot know what a person did", got, payout.StatusSubmitted)
		}
	}
}

func TestWhatTheRailRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spoil   func(*payout.Instruction)
		wantErr error
	}{
		{
			name:    "an instruction no rail could act on",
			spoil:   func(i *payout.Instruction) { i.IdempotencyKey = "" },
			wantErr: payout.ErrInvalidInstruction,
		},
		{
			name:    "nothing to type onto the transfer",
			spoil:   func(i *payout.Instruction) { i.Descriptor = "" },
			wantErr: payout.ErrInvalidInstruction,
		},
		{
			name:    "a destination for another rail",
			spoil:   func(i *payout.Instruction) { i.Destination.Kind = payout.KindSEPA },
			wantErr: payout.ErrRailTerminal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			instruction := anInstruction()
			tt.spoil(&instruction)
			got, err := manual.New().Submit(t.Context(), instruction)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Submit() = %v, want one wrapping %v", err, tt.wantErr)
			}
			if got.Ref() != "" {
				t.Errorf("Submit() returned %s beside a refusal", got)
			}
		})
	}
}

// TestAnAbandonedSubmissionIsRetryable: nothing was contacted and nothing can
// have half-happened, so the same key is exactly the right thing to re-send.
func TestAnAbandonedSubmissionIsRetryable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := manual.New().Submit(ctx, anInstruction()); !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("Submit() on an abandoned request = %v, want one wrapping ErrRailRetryable", err)
	}
}
