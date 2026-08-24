package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// RedactedPlaceholder is what a Secret renders as, everywhere. It is
// deliberately neither empty nor length-preserving: a reader must be able to
// tell "withheld" from "unset", and a length leaks the credential's shape.
const RedactedPlaceholder = "[REDACTED]"

// Secret is a configuration value that must never be printed: an API key, an
// API secret, a ledger credential. It is a string, so it costs nothing to
// carry, and it implements every interface Go reaches for when it renders
// something:
//
//	fmt.Formatter            every verb, including the ones that are a
//	                         mistake - see Format
//	fmt.Stringer             %s, %q, %v, and anything taking a Stringer
//	fmt.GoStringer           %#v
//	slog.LogValuer           log attributes, under any key
//	json.Marshaler           encoding/json, as a value
//	encoding.TextMarshaler   encoding/json as a map KEY, and every other
//	                         encoder that asks for text
//
// Between them there is no path from a Secret to its own contents that does
// not go through Reveal. Stringer alone was not enough, and the two gaps it
// left are worth remembering because both were silent:
//
//   - A verb the type has no answer for, such as %d. fmt.badVerb sets
//     p.erroring BEFORE printing the operand, and handleMethods returns
//     immediately when that flag is set - so String() is never consulted and
//     the raw value lands in the error text. Only Formatter runs early
//     enough.
//   - encoding/json, which encodes a string KIND by reflection and never
//     asks a type that implements nothing it recognises.
//
// A Secret is deliberately WRITE-ONLY for encoders: it marshals to the
// placeholder and there is no matching unmarshaller, so a Secret must never
// be a field of a type that is decoded from JSON - the decode would produce
// the literal placeholder as if it were the credential. Secrets in this
// repository come from the environment and from nowhere else.
//
// Reading the real value is deliberately a verb you have to type: Reveal.
// Every call site that reveals a secret is therefore greppable, and the
// default thing to do with one - pass it around, log it, print it - is the
// safe thing.
//
// A Secret is NOT protection against a caller who reveals it and then logs
// the result. It is protection against the accident: the config struct
// dumped at startup, the %+v in an error, the attribute named something the
// logger's own redaction did not recognise.
// It is a STRUCT rather than a named string type, and that is load-bearing
// rather than a matter of taste. A named string type is a string KIND, and
// reflection-based encoders read a string kind directly instead of asking
// the type - encoding/json's map-key path does exactly that, checking
// reflect.String BEFORE encoding.TextMarshaler, so a string-kind Secret used
// as a map key leaks its contents however many marshallers it implements. A
// struct kind has no readable contents without a method, so the guarantee
// stops being a list of paths somebody remembered and becomes a property of
// the type. That is the same argument the constitution makes one layer down
// for database-enforced invariants: structural, not disciplined.
//
// Construct one with NewSecret. It stays comparable, so configuration
// structs carrying one still compare with ==.
type Secret struct {
	// value is unexported, so nothing outside this file can read it
	// without going through Reveal.
	value string
}

// NewSecret wraps a credential. Secrets in this repository are read from the
// environment and constructed here; there is no other way in.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the underlying value. This is the only way to read it, and
// naming it this loudly is the point.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is unset. It exists so a caller can ask
// the one question about a secret that is safe to answer without revealing
// it - "was this configured?" - and callers do not reach for Reveal to find
// out.
func (s Secret) IsZero() bool { return s.value == "" }

// String satisfies fmt.Stringer, covering %s, %q and %v.
func (s Secret) String() string { return RedactedPlaceholder }

// GoString satisfies fmt.GoStringer, covering %#v - the verb a debugging
// print of a whole config struct is most likely to use.
func (s Secret) GoString() string { return RedactedPlaceholder }

// LogValue satisfies slog.LogValuer, so a Secret is redacted in a log record
// whatever attribute key it is given, and whether or not the logger's own
// key-based redaction recognises that key.
func (s Secret) LogValue() slog.Value { return slog.StringValue(RedactedPlaceholder) }

// Format satisfies fmt.Formatter, which is the only interface fmt consults
// before deciding a verb is wrong. It answers EVERY verb with the
// placeholder - %d, %f, %x and every flag, width and precision alike -
// because there is no verb for which printing a credential is the right
// answer, and because the whole hazard here is the verb nobody intended.
//
// Width and precision are deliberately ignored rather than applied to the
// placeholder: a precision could truncate it to something that no longer
// reads as a redaction.
func (s Secret) Format(state fmt.State, _ rune) {
	// The error is unrecoverable and unactionable - fmt.State's writer is
	// the print buffer - and returning it is not an option the interface
	// offers.
	_, _ = io.WriteString(state, RedactedPlaceholder)
}

// MarshalJSON satisfies json.Marshaler. Without it, encoding/json sees a
// string kind and encodes the underlying value by reflection, so any JSON
// dump of a struct carrying a Secret would exfiltrate it.
func (s Secret) MarshalJSON() ([]byte, error) {
	// Marshalling the placeholder rather than splicing quotes by hand, so
	// the escaping is the encoder's problem and stays correct if the
	// placeholder ever changes.
	return json.Marshal(RedactedPlaceholder)
}

// MarshalText satisfies encoding.TextMarshaler. It covers the paths
// MarshalJSON does not - a Secret used as a map key, and every other encoder
// that asks a type for its text form.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(RedactedPlaceholder), nil
}
