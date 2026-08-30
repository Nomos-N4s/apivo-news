// The two click references and the difference between them: [ClickRef], what
// a network echoed back, and [IssuedClickRef], what Apivo minted before the
// redirect. One file, because the whole point is that they are told apart,
// and the opposite rules they obey only read as deliberate side by side.

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The sentinels a click reference is refused with - blank where a value was
// present, and bytes that are neither a JSON string nor null. Absence is not
// among them: a network reporting no reference is ordinary.
var (
	// ErrBlankClickRef reports a [ClickRef] that says a reference is present
	// and then carries nothing but space. Blank and absent are different
	// facts and the schema refuses to let them collapse
	// (network_transaction_click_ref_not_blank): the content digest folds a
	// null reference to the empty string, so a blank one would fingerprint
	// identically to no reference at all, and `click_ref is not null` would
	// count a blank as attribution present. A report that cannot be told
	// apart from another report is refused rather than normalised, because
	// quietly rewriting what a network said is not this port's job.
	ErrBlankClickRef = errors.New("networks: click reference is present but blank")
	// ErrMalformedClickRef reports JSON that is neither a string nor null
	// where a [ClickRef] was expected. The encoding has exactly two shapes
	// because the fact has exactly two shapes, and a number or an object
	// arriving in that position is a producer that does not know which of
	// them it meant.
	ErrMalformedClickRef = errors.New("networks: click reference is not a JSON string or null")
	// ErrInvalidIssuedClickRef reports a reference Apivo is supposed to have
	// minted that is not one: shorter than 128 bits of entropy needs once
	// encoded, or carrying characters a URL would have to escape. It mirrors
	// click_ref_url_safe_and_long_enough (migration 0012), which is the
	// constraint FR-020's unguessability actually rests on. Refused at
	// construction rather than at the redirect, because a short reference
	// that reached a network is one an attacker can enumerate and the
	// database will refuse the click row it was supposed to match.
	ErrInvalidIssuedClickRef = errors.New("networks: click reference is not the unguessable reference a redirect needs")
)

// reportedJSONNull is the literal a payload must not be, and the literal an
// absent [ClickRef] is. It is spelled once, here, because "absent" and "the
// four bytes that mean absent" are the same fact and should be recognised by
// the same comparison.
var reportedJSONNull = []byte("null")

// ClickRef is the attribution reference a network ECHOED back against a
// transaction, or the definite absence of one. It is what the network said,
// with all the unreliability that implies - which is why it is a different
// type from [IssuedClickRef], the reference Apivo minted before the redirect.
// The two are constrained differently, arrive from opposite directions, and
// confusing them costs a member their cashback (see [IssuedClickRef]).
//
// Absence is a first-class state rather than an empty string, because the
// two are different facts and the evidence table refuses to let them
// collapse. click_ref is nullable and constrained not to be blank (migration
// 0012): the content digest folds a null reference to the empty string, so a
// blank reference would fingerprint identically to no reference at all and
// the schema would call two different reports the same report; and
// `click_ref is not null` counts a blank as attribution present, so an
// unattributed transaction would sit in the attributed index carrying
// nothing. Both failures are silent, and both end in a member being credited
// for someone else's purchase or for nobody's.
//
// The fields are unexported and the zero value is the absence, so a report
// that never set a reference says "the network reported none" rather than
// "the network reported a blank one". That absence is ordinary, not an
// error: a transaction with no matching click is recorded as unattributed
// and never auto-credited (FR-034), which requires it to be storable in the
// first place.
type ClickRef struct {
	ref     string
	present bool
}

// NewClickRef builds a reference the network actually reported.
//
// It takes the value as-is rather than trimming or rejecting it, and
// [ClickRef.Validate] is what refuses a blank one. That split is
// deliberate: this port's job at translation time is to carry what the
// network said, and a constructor that quietly cleaned the value would make
// the very confusion the schema forbids unobservable - a blank reference
// would arrive looking like a real one, and nobody would ever see the
// adapter bug that produced it.
func NewClickRef(ref string) ClickRef {
	return ClickRef{ref: ref, present: true}
}

// Ref returns the reference and whether the network reported one at all.
// Callers that match a report back to a click read it this way, so "no
// reference" is a branch they must handle rather than a string comparison
// they might forget.
func (c ClickRef) Ref() (string, bool) {
	if !c.present {
		return "", false
	}
	return c.ref, true
}

// String renders the reference for logs, errors and test failures, printing
// an absent reference as "(none)" so a blank one cannot read as no reference
// in the very output someone is using to tell them apart.
func (c ClickRef) String() string {
	if !c.present {
		return "(none)"
	}
	return strconv.Quote(c.ref)
}

// Validate reports whether the reference is one the evidence table would
// accept, returning an error wrapping [ErrBlankClickRef] for a reference
// that claims to be present and carries nothing but space. An absent
// reference is valid: an unattributed transaction is evidence too (FR-034).
func (c ClickRef) Validate() error {
	if c.present && strings.TrimSpace(c.ref) == "" {
		return fmt.Errorf("%w: %s", ErrBlankClickRef, strconv.Quote(c.ref))
	}
	return nil
}

// MarshalJSON encodes a reported reference as a JSON string and an absent one
// as null - the same two shapes the nullable click_ref column has.
//
// The methods exist because the distinction this type is built to carry does
// not survive the default encoding. Both fields are unexported, so an
// attributed report would marshal to {} and unmarshal back as a perfectly
// valid UNATTRIBUTED report: no error at marshal, none at unmarshal, and none
// at Validate. A [Reported] crosses encoding/json in this repository as a
// matter of course - the outbox stores an event payload as json.RawMessage,
// and the operations endpoints serve reports over JSON - so without these
// methods attribution would disappear silently in exactly the places built to
// preserve it.
//
// A present-but-blank reference does not encode at all, for the same reason
// [money.Amount] refuses to encode without a currency: putting one on the
// wire would produce a document that decodes as attribution present and
// carrying nothing.
func (c ClickRef) MarshalJSON() ([]byte, error) {
	if !c.present {
		return []byte("null"), nil
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(c.ref)
}

// UnmarshalJSON decodes null as the absence of a reference and a JSON string
// as one the network reported, refusing everything else wrapping
// [ErrMalformedClickRef]. A blank string decodes rather than failing here:
// blank is a fact a network can state, and [ClickRef.Validate] is where the
// port refuses it, so a stored report that is already wrong stays readable
// instead of becoming undecodable evidence.
func (c *ClickRef) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), reportedJSONNull) {
		*c = ClickRef{}
		return nil
	}
	var ref string
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedClickRef, err)
	}
	*c = NewClickRef(ref)
	return nil
}

// IssuedClickRef is the reference APIVO minted for one click and passed to
// the network in its own click-reference parameter (FR-020, FR-021). It is
// the value cashback.click.click_ref holds and the value every reported
// transaction is matched back on.
//
// It is a separate type from [ClickRef] because the two obey opposite rules
// and only one of them is ours. A ClickRef is whatever a network echoed:
// arbitrary text, legitimately absent, and constrained only not to be blank.
// An IssuedClickRef must satisfy click_ref_url_safe_and_long_enough - at
// least 22 URL-safe characters, which is what 128 bits of entropy needs once
// encoded - and must exist before the redirect is issued at all. One type for
// both would compile the confusion: reconciliation code holding a [Reported]
// could re-issue a redirect with the reference the NETWORK echoed, or a
// handler could pass the raw bytes before base64url encoding, and the member
// would be redirected, buy, and have the network echo back a reference no
// click row can match. That transaction lands unattributed - the failure that
// costs a member real money while looking to them exactly like success.
//
// The field is unexported and [NewIssuedClickRef] is the only way to fill it,
// so a reference that would be refused by the click table cannot reach a
// redirect. The zero value is the absence of a minted reference, which
// [ValidateDeeplinkInputs] refuses: FR-020 requires the click record and its
// reference to exist BEFORE the redirect, so a redirect built without one is
// being built out of order.
type IssuedClickRef struct {
	ref string
}

// issuedClickRefMinLen is the shortest reference the click table accepts: 22
// base64url characters carry 132 bits, which is the first length at or above
// the 128 bits FR-020's unguessability is specified in. It mirrors
// click_ref_url_safe_and_long_enough rather than restating a policy.
const issuedClickRefMinLen = 22

// NewIssuedClickRef takes the reference minted for a click and refuses one the
// click table would refuse, wrapping [ErrInvalidIssuedClickRef].
//
// Refusing at construction rather than at the redirect is the point. A short
// or punctuated reference reaching a network is one an attacker can enumerate
// and one the click row it was supposed to match will not accept, so the
// redirect would succeed, the purchase would happen, and the transaction
// would come back attributable to nothing.
func NewIssuedClickRef(ref string) (IssuedClickRef, error) {
	if len(ref) < issuedClickRefMinLen {
		return IssuedClickRef{}, fmt.Errorf("%w: %s is %d characters, and the click table requires at least %d",
			ErrInvalidIssuedClickRef, strconv.Quote(ref), len(ref), issuedClickRefMinLen)
	}
	for i := range len(ref) {
		c := ref[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return IssuedClickRef{}, fmt.Errorf("%w: %s is not URL-safe", ErrInvalidIssuedClickRef, strconv.Quote(ref))
		}
	}
	return IssuedClickRef{ref: ref}, nil
}

// Ref returns the reference itself, for the adapter that places it in the
// network's own parameter and for the click row that stores it.
func (r IssuedClickRef) Ref() string { return r.ref }

// Validate reports whether a reference was minted at all, returning an error
// wrapping [ErrInvalidIssuedClickRef] for the zero value. Nothing else can be
// wrong with a constructed one, which is the point of the constructor.
func (r IssuedClickRef) Validate() error {
	if r.ref == "" {
		return fmt.Errorf("%w: no reference was minted", ErrInvalidIssuedClickRef)
	}
	return nil
}

// String renders the reference for logs, errors and test failures, printing
// the zero value as "(none)" so a redirect built out of order reads as one.
func (r IssuedClickRef) String() string {
	if r.ref == "" {
		return "(none)"
	}
	return strconv.Quote(r.ref)
}

// MarshalJSON encodes the reference as a JSON string - the shape POST
// /clickouts returns it in. The zero value does not encode: a document
// carrying click_ref for a click that minted none would be read as a click
// nobody can match a transaction to.
func (r IssuedClickRef) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r.ref)
}

// UnmarshalJSON decodes a JSON string through [NewIssuedClickRef], so a
// reference that crossed a wire is held to the same rule as one just minted.
// The unexported field would otherwise let the default encoding hand back a
// zero value from any document at all.
func (r *IssuedClickRef) UnmarshalJSON(data []byte) error {
	var ref string
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidIssuedClickRef, err)
	}
	issued, err := NewIssuedClickRef(ref)
	if err != nil {
		return err
	}
	*r = issued
	return nil
}
