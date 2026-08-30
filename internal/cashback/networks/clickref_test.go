// The tests for clickref.go: that a blank reference and an absent one stay
// two different facts, across encoding/json as well as in memory, and that a
// minted reference cannot be built in a shape the click table would refuse.

package networks_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// portTestMustIssuedRef builds a minted click reference or stops the run,
// under the same rule as portTestMustAccount.
func portTestMustIssuedRef(ref string) networks.IssuedClickRef {
	issued, err := networks.NewIssuedClickRef(ref)
	if err != nil {
		panic("networks_test: fixture click reference: " + err.Error())
	}
	return issued
}

// portTestIssuedRef is the reference Apivo minted for a click, of the shape
// the click table accepts.
var portTestIssuedRef = portTestMustIssuedRef(portTestRefValue)

// TestClickRefTellsBlankFromAbsent pins the distinction the evidence table
// depends on: a reference nobody reported and a reference reported blank are
// different values, they read differently, and only the second is refused.
// Collapsing them is what would let one unattributed transaction fingerprint
// as another and sit in the attributed index carrying nothing (migration
// 0012).
func TestClickRefTellsBlankFromAbsent(t *testing.T) {
	t.Parallel()

	absent := networks.ClickRef{}
	if _, ok := absent.Ref(); ok {
		t.Errorf("ClickRef{}.Ref() reported a reference; the zero value is the absence of one")
	}
	if err := absent.Validate(); err != nil {
		t.Errorf("ClickRef{}.Validate() = %v, want nil: an unattributed transaction is evidence too (FR-034)", err)
	}

	blank := networks.NewClickRef("")
	value, ok := blank.Ref()
	if !ok {
		t.Errorf("NewClickRef(%q).Ref() reported no reference; a blank reference is present and wrong, not absent", "")
	}
	if value != "" {
		t.Errorf("NewClickRef(%q).Ref() = %q, want the value as reported", "", value)
	}
	if err := blank.Validate(); !errors.Is(err, networks.ErrBlankClickRef) {
		t.Errorf("NewClickRef(%q).Validate() = %v, want an error wrapping %v", "", err, networks.ErrBlankClickRef)
	}

	present := networks.NewClickRef(portTestRefValue)
	if value, ok := present.Ref(); !ok || value != portTestRefValue {
		t.Errorf("NewClickRef(ref).Ref() = %q, %v, want the reference back", value, ok)
	}
	if err := present.Validate(); err != nil {
		t.Errorf("NewClickRef(ref).Validate() = %v, want nil", err)
	}

	// The rendering is what an operator reads when telling two reports
	// apart, so it must not print the blank one as if nothing was reported.
	if absent.String() == blank.String() {
		t.Errorf("an absent and a blank click reference both render as %q", absent.String())
	}
}

// TestClickRefSurvivesJSON pins the distinction above ACROSS encoding/json,
// which is where it would otherwise be lost. Both of ClickRef's fields are
// unexported, so without its own methods an attributed report marshals to
// {} and decodes back as a valid unattributed one - no error at marshal,
// none at unmarshal, none at Validate, and a member credited for nobody's
// purchase. A Reported crosses JSON here as a matter of course: the outbox
// stores an event payload as json.RawMessage and the operations endpoints
// serve reports over the wire.
func TestClickRefSurvivesJSON(t *testing.T) {
	t.Parallel()

	t.Run("an attributed report keeps its attribution", func(t *testing.T) {
		t.Parallel()

		encoded, err := json.Marshal(portTestReport())
		if err != nil {
			t.Fatalf("json.Marshal(report) = %v, want nil", err)
		}
		if !strings.Contains(string(encoded), strconv.Quote(portTestRefValue)) {
			t.Fatalf("json.Marshal(report) = %s, want it to carry the click reference", encoded)
		}
		var back networks.Reported
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("json.Unmarshal(report) = %v, want nil", err)
		}
		value, ok := back.ClickRef.Ref()
		if !ok || value != portTestRefValue {
			t.Errorf("the round-tripped report reports ref %q, present %v; attribution was lost in the encoding", value, ok)
		}
	})

	t.Run("an unattributed report stays unattributed", func(t *testing.T) {
		t.Parallel()

		report := portTestReport()
		report.ClickRef = networks.ClickRef{}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("json.Marshal(report) = %v, want nil", err)
		}
		if !strings.Contains(string(encoded), `"ClickRef":null`) {
			t.Errorf("json.Marshal(report) = %s, want the absence encoded as null, the shape the nullable column has", encoded)
		}
		var back networks.Reported
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("json.Unmarshal(report) = %v, want nil", err)
		}
		if _, ok := back.ClickRef.Ref(); ok {
			t.Errorf("the round-tripped report claims a reference; null must decode as the absence")
		}
		if err := back.Validate(); err != nil {
			t.Errorf("the round-tripped unattributed report = %v, want nil (FR-034)", err)
		}
	})

	t.Run("a blank reference never reaches the wire", func(t *testing.T) {
		t.Parallel()

		if _, err := json.Marshal(networks.NewClickRef("  ")); !errors.Is(err, networks.ErrBlankClickRef) {
			t.Errorf("json.Marshal(blank ref) = %v, want an error wrapping %v", err, networks.ErrBlankClickRef)
		}
	})

	t.Run("neither a string nor null is refused", func(t *testing.T) {
		t.Parallel()

		var ref networks.ClickRef
		if err := json.Unmarshal([]byte(`12345`), &ref); !errors.Is(err, networks.ErrMalformedClickRef) {
			t.Errorf("json.Unmarshal(number) = %v, want an error wrapping %v", err, networks.ErrMalformedClickRef)
		}
	})
}

// TestIssuedClickRefIsUnguessableByConstruction pins what makes the minted
// reference a different type from the echoed one: it cannot be built in a
// shape the click table would refuse, so a redirect can never carry a
// reference no click row will ever match (FR-020).
func TestIssuedClickRefIsUnguessableByConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		wantErr error
	}{
		{name: "a 22-character base64url reference", ref: portTestRefValue},
		{name: "a 32-character hex reference", ref: "0123456789abcdef0123456789abcdef"},
		{name: "a reference using both URL-safe punctuation marks", ref: "3zK9pQ2vX7mB4nL8sR-t_w"},
		{name: "the word a test would reach for", ref: "ref", wantErr: networks.ErrInvalidIssuedClickRef},
		{name: "one character short of the bound", ref: portTestRefValue[1:], wantErr: networks.ErrInvalidIssuedClickRef},
		{name: "the empty reference", ref: "", wantErr: networks.ErrInvalidIssuedClickRef},
		{name: "a long reference carrying a character a URL must escape", ref: "3zK9pQ2vX7mB4nL8sR1tY+", wantErr: networks.ErrInvalidIssuedClickRef},
		{name: "raw bytes that were never base64url encoded", ref: "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15", wantErr: networks.ErrInvalidIssuedClickRef},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issued, err := networks.NewIssuedClickRef(tc.ref)
			portTestAssert(t, "NewIssuedClickRef()", err, tc.wantErr, nil)
			if tc.wantErr == nil && issued.Ref() != tc.ref {
				t.Errorf("NewIssuedClickRef(%q).Ref() = %q, want the reference back", tc.ref, issued.Ref())
			}
			if tc.wantErr != nil && issued.Ref() != "" {
				t.Errorf("NewIssuedClickRef(%q) returned %q beside its refusal", tc.ref, issued.Ref())
			}
		})
	}

	t.Run("the zero value is a redirect built before its click", func(t *testing.T) {
		t.Parallel()

		var never networks.IssuedClickRef
		if err := never.Validate(); !errors.Is(err, networks.ErrInvalidIssuedClickRef) {
			t.Errorf("IssuedClickRef{}.Validate() = %v, want an error wrapping %v", err, networks.ErrInvalidIssuedClickRef)
		}
		if never.String() == portTestIssuedRef.String() {
			t.Errorf("a minted and an unminted reference both render as %q", never.String())
		}
	})

	t.Run("a reference survives JSON as itself", func(t *testing.T) {
		t.Parallel()

		encoded, err := json.Marshal(portTestIssuedRef)
		if err != nil {
			t.Fatalf("json.Marshal(issued ref) = %v, want nil", err)
		}
		var back networks.IssuedClickRef
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("json.Unmarshal(issued ref) = %v, want nil", err)
		}
		if back.Ref() != portTestRefValue {
			t.Errorf("the round-tripped reference is %q, want %q", back.Ref(), portTestRefValue)
		}
		if _, err := json.Marshal(networks.IssuedClickRef{}); !errors.Is(err, networks.ErrInvalidIssuedClickRef) {
			t.Errorf("json.Marshal(IssuedClickRef{}) = %v, want an error wrapping %v", err, networks.ErrInvalidIssuedClickRef)
		}
		if err := json.Unmarshal([]byte(`"ref"`), &back); !errors.Is(err, networks.ErrInvalidIssuedClickRef) {
			t.Errorf("json.Unmarshal(short ref) = %v, want an error wrapping %v", err, networks.ErrInvalidIssuedClickRef)
		}
	})
}
