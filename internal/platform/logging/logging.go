// Package logging builds the process logger. Production output is JSON for
// machine ingestion; development output is human-readable text. Every logger
// it builds redacts attributes whose key names a secret, so a credential
// cannot reach a log line by being passed to the wrong helper.
package logging

import (
	"io"
	"log/slog"
	"strings"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// Redacted is written in place of a secret value. It is deliberately not
// empty and not the value's length: a reader must be able to tell "this was
// withheld" from "this was unset", and a length leaks the key's shape.
const Redacted = "[REDACTED]"

// secretKeyMarkers are the substrings that make an attribute key a secret.
// Matching is on the key with every non-alphanumeric character removed and
// lowercased, so api_key, apiKey, API-KEY and api.key are one marker.
//
// "url" is deliberately absent: feed and request URLs are logged by name all
// over the ingestion module and are not credentials. The URLs that DO carry
// one - a connection string, a DSN - are named individually below, and a URL
// with userinfo in it is redacted at its source (ingestion.redactedFeedURL).
var secretKeyMarkers = []string{
	"accesskey",
	"apikey",
	"apisecret",
	"authorization",
	"connectionstring",
	"credential",
	"databaseurl",
	"dsn",
	"password",
	"passwd",
	"privatekey",
	"secret",
	"sessionkey",
	"token",
}

// IsSecretKey reports whether an attribute key names a value that must never
// reach a log line. It is exported so callers naming a new attribute can
// assert in a test that the name they chose is covered, rather than
// discovering in production that it was not.
func IsSecretKey(key string) bool {
	normalised := normaliseKey(key)
	if normalised == "" {
		return false
	}
	for _, marker := range secretKeyMarkers {
		if strings.Contains(normalised, marker) {
			return true
		}
	}
	return false
}

// normaliseKey lowercases a key and drops everything that is not a letter or
// a digit, so every separator convention collapses to one form.
func normaliseKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// redact replaces the value of a secret-named attribute with Redacted.
//
// It is the last line of defence, not the first: a value that IS a secret
// should be carried in a config.Secret, which renders as Redacted whatever
// key it is logged under and whether or not it passes through here. This
// catches the plain string handed to slog under a name that gives it away.
func redact(_ []string, a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// New returns a logger writing to w, emitting records at or above level.
// In config.EnvProd the handler is JSON; in any other environment, text.
// Both redact secret-named attributes - see IsSecretKey.
func New(w io.Writer, level slog.Level, env string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redact}
	if env == config.EnvProd {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
