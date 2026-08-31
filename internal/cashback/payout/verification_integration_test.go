// The tests for verification.go. Verification is one-way and final in the
// schema itself, so what is checked here is that the code agrees with the
// guard rather than discovering it as an exception in a member's request.

package payout_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

func TestVerifyingRecordsWhenAndHow(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	recorded, err := d.Record(ctx, payout.NewDestination{
		AccountID: member, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if err := payout.RequireVerified(recorded); !errors.Is(err, payout.ErrDestinationNotVerified) {
		t.Fatalf("RequireVerified() on a new destination = %v, want one wrapping ErrDestinationNotVerified", err)
	}

	verified, err := d.Verify(ctx, member, recorded.ID, "micro_deposit")
	if err != nil {
		t.Fatalf("Verify(): %v", err)
	}
	if !verified.Verified() {
		t.Error("the destination is not verified after Verify()")
	}
	if verified.VerifiedMethod != "micro_deposit" {
		t.Errorf("VerifiedMethod = %q, want the method that was used", verified.VerifiedMethod)
	}
	if err := payout.RequireVerified(verified); err != nil {
		t.Errorf("RequireVerified() on a verified destination: %v", err)
	}

	// And it is what a later read sees, not just what this call returned.
	back, err := d.Get(ctx, member, recorded.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if !back.Verified() || back.VerifiedMethod != "micro_deposit" {
		t.Errorf("a later read says verified=%v method=%q", back.Verified(), back.VerifiedMethod)
	}
}

// TestVerifyingTwiceKeepsTheFirstProof is the one-way rule. The table's guard
// raises on any attempt to change or re-date a verification, because it is
// the evidence a withdrawal was allowed to name the destination - so a repeat
// must return what stands rather than becoming a database error in the middle
// of a member's request.
func TestVerifyingTwiceKeepsTheFirstProof(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	recorded, err := d.Record(ctx, payout.NewDestination{
		AccountID: member, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	first, err := d.Verify(ctx, member, recorded.ID, "micro_deposit")
	if err != nil {
		t.Fatalf("the first Verify(): %v", err)
	}

	again, err := d.Verify(ctx, member, recorded.ID, "a_different_method")
	if err != nil {
		t.Fatalf("verifying twice failed: %v", err)
	}
	if !again.VerifiedAt.Equal(first.VerifiedAt) {
		t.Errorf("the second Verify() re-dated the proof to %s, want %s", again.VerifiedAt, first.VerifiedAt)
	}
	if again.VerifiedMethod != "micro_deposit" {
		t.Errorf("the second Verify() replaced the method with %q, want the first one", again.VerifiedMethod)
	}
}

// TestAnotherMembersDestinationCannotBeVerified: the ownership rule holds on
// the write as well as the reads, and answers the same way, so nothing tells
// a caller that another member's destination id is real.
func TestAnotherMembersDestinationCannotBeVerified(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	owner := aMember(ctx, t, tx)
	stranger := aMember(ctx, t, tx)
	d := destinations(t, tx)

	theirs, err := d.Record(ctx, payout.NewDestination{
		AccountID: owner, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	if _, err := d.Verify(ctx, stranger, theirs.ID, "micro_deposit"); !errors.Is(err, payout.ErrDestinationNotFound) {
		t.Fatalf("Verify() as a stranger = %v, want one wrapping ErrDestinationNotFound", err)
	}
	if _, err := d.Verify(ctx, stranger, uuid.New(), "micro_deposit"); !errors.Is(err, payout.ErrDestinationNotFound) {
		t.Fatalf("Verify() for an id that names nothing = %v, want one wrapping ErrDestinationNotFound", err)
	}

	// And it really was not verified.
	back, err := d.Get(ctx, owner, theirs.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if back.Verified() {
		t.Error("a stranger's attempt verified the destination")
	}
}

// TestAVerificationSaysHowItWasDone mirrors
// payout_destination_verification_all_or_none, refused here so a member's
// request fails with a sentence rather than a constraint name.
func TestAVerificationSaysHowItWasDone(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	recorded, err := d.Record(ctx, payout.NewDestination{
		AccountID: member, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	for _, method := range []string{"", "   "} {
		if _, err := d.Verify(ctx, member, recorded.ID, method); !errors.Is(err, payout.ErrNoVerificationMethod) {
			t.Errorf("Verify() with method %q = %v, want one wrapping ErrNoVerificationMethod", method, err)
		}
	}
	back, err := d.Get(ctx, member, recorded.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if back.Verified() {
		t.Error("a verification with no method was recorded anyway")
	}
}
