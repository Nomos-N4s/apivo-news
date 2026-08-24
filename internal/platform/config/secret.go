package config

import "log/slog"

// RedactedPlaceholder is what a Secret renders as, everywhere. It is
// deliberately neither empty nor length-preserving: a reader must be able to
// tell "withheld" from "unset", and a length leaks the credential's shape.
const RedactedPlaceholder = "[REDACTED]"

// Secret is a configuration value that must never be printed: an API key, an
// API secret, a ledger credential. It is a string, so it costs nothing to
// carry, and it implements every interface Go reaches for when it prints
// something - fmt's Stringer and GoStringer, and slog's LogValuer - so the
// value cannot escape through %s, %q, %v, %#v, a log attribute, or a struct
// printed whole.
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
type Secret string

// Reveal returns the underlying value. This is the only way to read it, and
// naming it this loudly is the point.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset. It exists so a caller can ask
// the one question about a secret that is safe to answer without revealing
// it - "was this configured?" - and callers do not reach for Reveal to find
// out.
func (s Secret) IsZero() bool { return s == "" }

// String satisfies fmt.Stringer, covering %s, %q and %v.
func (s Secret) String() string { return RedactedPlaceholder }

// GoString satisfies fmt.GoStringer, covering %#v - the verb a debugging
// print of a whole config struct is most likely to use.
func (s Secret) GoString() string { return RedactedPlaceholder }

// LogValue satisfies slog.LogValuer, so a Secret is redacted in a log record
// whatever attribute key it is given, and whether or not the logger's own
// key-based redaction recognises that key.
func (s Secret) LogValue() slog.Value { return slog.StringValue(RedactedPlaceholder) }
