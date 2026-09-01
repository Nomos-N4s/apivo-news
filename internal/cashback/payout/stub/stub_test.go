// The tests for the stub rail. The three modes exist so the ways a real rail
// ruins a member's day can be rehearsed on purpose, and what is pinned here
// is that each one leaves the caller able to do the right thing.

package stub_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

func anInstruction() payout.Instruction {
	return payout.Instruction{
		IdempotencyKey: "withdrawal:6f9619ff-8b86-d011-b42d-00c04fc964ff",
		Amount:         money.Amount{Minor: 1250, Currency: "EUR"},
		Destination:    payout.DestinationRef{Kind: payout.KindStub, DetailsRef: "vault:member/stub/1"},
		Descriptor:     "Example Cashback",
	}
}

func TestARailCarriesAPaymentAndCanBeAskedAboutIt(t *testing.T) {
	t.Parallel()

	rail := stub.New()
	if rail.Kind() != payout.KindStub {
		t.Errorf("Kind() = %q, want %q", rail.Kind(), payout.KindStub)
	}

	ref, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	got, err := rail.Status(t.Context(), ref)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if got != payout.StatusSubmitted {
		t.Errorf("Status() = %q, want %q", got, payout.StatusSubmitted)
	}

	if err := rail.Settle(ref); err != nil {
		t.Fatalf("Settle(): %v", err)
	}
	if got, err := rail.Status(t.Context(), ref); err != nil || got != payout.StatusSettled {
		t.Errorf("Status() = (%q, %v), want settled", got, err)
	}
}

// TestATimeoutIsRetryableAndTheRetryFindsOnePayment is the dangerous
// scenario, rehearsed. A submission that times out may already have landed,
// so the caller must re-send the SAME key and must end with one payment - and
// must not release the reservation in between.
func TestATimeoutIsRetryableAndTheRetryFindsOnePayment(t *testing.T) {
	t.Parallel()

	rail := stub.New(stub.WithTimeout())
	_, err := rail.Submit(t.Context(), anInstruction())
	if !errors.Is(err, payout.ErrRailRetryable) {
		t.Fatalf("a timeout = %v, want one wrapping ErrRailRetryable", err)
	}
	if errors.Is(err, payout.ErrRailTerminal) {
		t.Fatal("a timeout also reads as terminal; a reservation would be released for a payment in flight")
	}
	if !errors.Is(err, stub.ErrRailTimedOut) {
		t.Errorf("the failure lost its cause: %v", err)
	}

	first, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	again, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("a third submission failed: %v", err)
	}
	if first.Ref() != again.Ref() {
		t.Errorf("re-sending one key gave %s and %s; that is two payments", first, again)
	}
}

// TestAPermanentFailureIsTerminal: the reservation goes back to the member's
// confirmed balance and they are told (FR-053), which only happens if the
// caller can tell this from a timeout.
func TestAPermanentFailureIsTerminal(t *testing.T) {
	t.Parallel()

	rail := stub.New(stub.WithPermanentFailure())
	_, err := rail.Submit(t.Context(), anInstruction())
	if !errors.Is(err, payout.ErrRailTerminal) {
		t.Fatalf("a refusal = %v, want one wrapping ErrRailTerminal", err)
	}
	if errors.Is(err, payout.ErrRailRetryable) {
		t.Fatal("a refusal also reads as retryable; a member's balance would be held for a payment that never happens")
	}
	if !errors.Is(err, stub.ErrRailRefused) {
		t.Errorf("the failure lost its cause: %v", err)
	}
}

// TestAKeyAlreadyAcceptedSurvivesInjectedMisbehaviour pins the order inside
// Submit. A key this rail has accepted names a payment that EXISTS, and no
// injected failure afterwards may make it look like it does not - which is
// exactly the shape of a timeout on a submission that landed.
func TestAKeyAlreadyAcceptedSurvivesInjectedMisbehaviour(t *testing.T) {
	t.Parallel()

	rail := stub.New()
	first, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("Submit(): %v", err)
	}

	// Now make THIS rail misbehave - the same one that already holds the
	// payment - and ask it again. A second rail would prove nothing: the
	// ordering under test is inside one rail's Submit.
	rail.FailNext(payout.Terminal(stub.ErrRailRefused))

	again, err := rail.Submit(t.Context(), anInstruction())
	if err != nil {
		t.Fatalf("re-submitting an accepted key failed: %v", err)
	}
	if again.Ref() != first.Ref() {
		t.Errorf("re-submitting gave %s, want the payment that already exists (%s)", again, first)
	}

	// And the failure is still pending, so it was not spent on a key that
	// had already been accepted - the lookup came first, which is the point.
	other := anInstruction()
	other.IdempotencyKey = "withdrawal:00000000-0000-0000-0000-000000000002"
	if _, err := rail.Submit(t.Context(), other); !errors.Is(err, payout.ErrRailTerminal) {
		t.Errorf("a new key = %v, want the injected failure still waiting for it", err)
	}
}

// TestConcurrentSubmissionsOfOneKeyMakeOnePayment: C-5 under the condition it
// actually has to hold in - two requests at once.
func TestConcurrentSubmissionsOfOneKeyMakeOnePayment(t *testing.T) {
	t.Parallel()

	rail := stub.New()
	const submitters = 8
	refs := make([]string, submitters)
	var wg sync.WaitGroup
	for i := range submitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := rail.Submit(context.Background(), anInstruction())
			if err != nil {
				t.Errorf("submitter %d: %v", i, err)
				return
			}
			refs[i] = ref.Ref()
		}()
	}
	wg.Wait()

	for i, ref := range refs {
		if ref != refs[0] {
			t.Fatalf("submitter %d landed on %q, submitter 0 on %q; concurrent retries made two payments", i, ref, refs[0])
		}
	}
}

// TestAReferenceThisRailNeverIssuedIsTerminal: it will not start existing, and
// a caller that retried would ask forever about a payment nobody made.
func TestAReferenceThisRailNeverIssuedIsTerminal(t *testing.T) {
	t.Parallel()

	invented, err := payout.NewRailReference("stub:not-a-payment")
	if err != nil {
		t.Fatalf("NewRailReference(): %v", err)
	}
	_, err = stub.New().Status(t.Context(), invented)
	if !errors.Is(err, payout.ErrRailTerminal) {
		t.Fatalf("Status() of an unknown reference = %v, want one wrapping ErrRailTerminal", err)
	}
	if errors.Is(err, payout.ErrRailRetryable) {
		t.Error("an unknown reference reads as retryable; a caller would ask forever")
	}
}

func TestWhatTheStubRefuses(t *testing.T) {
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
			name:    "a destination for another rail",
			spoil:   func(i *payout.Instruction) { i.Destination.Kind = payout.KindManual },
			wantErr: payout.ErrRailTerminal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			instruction := anInstruction()
			tt.spoil(&instruction)
			if _, err := stub.New().Submit(t.Context(), instruction); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Submit() = %v, want one wrapping %v", err, tt.wantErr)
			}
		})
	}
}
