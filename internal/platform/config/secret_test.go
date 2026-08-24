package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// leaked is the value every assertion below looks for in the output. If it
// appears anywhere, a print path is not covered.
const leaked = "sk_live_do_not_print_me"

// sprintf is fmt.Sprintf behind an any-typed argument. The point of the
// table below is to exercise the verbs a careless caller would reach for,
// including "%s" on a Stringer - which staticcheck's S1025 would otherwise
// insist be rewritten as the very call the test is proving redundant.
func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()

	secret := config.NewSecret(leaked)
	// A struct holding one, printed whole, is the accident this type
	// exists to survive.
	holder := struct {
		Name  string
		Token config.Secret
	}{Name: "network", Token: secret}

	tests := []struct {
		name string
		got  string
	}{
		{name: "String", got: secret.String()},
		{name: "GoString", got: secret.GoString()},
		{name: "verb s", got: sprintf("%s", secret)},
		{name: "verb q", got: sprintf("%q", secret)},
		{name: "verb v", got: sprintf("%v", secret)},
		{name: "verb plus v", got: sprintf("%+v", secret)},
		{name: "verb sharp v", got: sprintf("%#v", secret)},
		{name: "inside a struct, verb v", got: sprintf("%v", holder)},
		{name: "inside a struct, verb plus v", got: sprintf("%+v", holder)},
		{name: "inside a struct, verb sharp v", got: sprintf("%#v", holder)},
		{name: "error wrapping it", got: fmt.Errorf("calling with %s", secret).Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tt.got, leaked) {
				t.Fatalf("secret leaked: %s", tt.got)
			}
			if !strings.Contains(tt.got, config.RedactedPlaceholder) {
				t.Fatalf("expected %q in %q", config.RedactedPlaceholder, tt.got)
			}
		})
	}
}

// TestSecretSurvivesTheEncoderPaths covers the two ways a Secret escaped
// before: a format verb it has no answer for, and an encoder that reads the
// underlying string kind by reflection rather than asking the type.
//
// Both are the same shape of accident - nobody writes %d on a credential or
// marshals a config struct on purpose - which is exactly why the type has to
// be airtight rather than merely well-behaved on the paths somebody
// remembered.
func TestSecretSurvivesTheEncoderPaths(t *testing.T) {
	t.Parallel()

	secret := config.NewSecret(leaked)
	holder := struct {
		Name  string        `json:"name"`
		Token config.Secret `json:"token"`
	}{Name: "network", Token: secret}

	marshalled := func(v any) string {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		return string(encoded)
	}

	tests := []struct {
		name string
		got  string
	}{
		// fmt.badVerb sets p.erroring BEFORE printing the operand, and
		// handleMethods returns immediately when that flag is set - so
		// String() is never consulted and the raw value lands in the error
		// text. Only fmt.Formatter runs early enough to stop it.
		{name: "wrong verb d", got: sprintf("%d", secret)},
		{name: "wrong verb f", got: sprintf("%f", secret)},
		{name: "wrong verb with a flag", got: sprintf("%+d", secret)},
		{name: "wrong verb inside a struct", got: sprintf("%d", holder)},
		// encoding/json encodes a string KIND through reflection unless the
		// type implements json.Marshaler or encoding.TextMarshaler.
		{name: "json of the secret alone", got: marshalled(secret)},
		{name: "json of a struct carrying one", got: marshalled(holder)},
		{name: "json of a map valued by one", got: marshalled(map[string]config.Secret{"token": secret})},
		{name: "json of a slice of them", got: marshalled([]config.Secret{secret})},
		// A Secret used as a map KEY goes through encoding.TextMarshaler,
		// which is a third path again.
		{name: "json of a map keyed by one", got: marshalled(map[config.Secret]string{secret: "value"})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tt.got, leaked) {
				t.Fatalf("secret leaked: %s", tt.got)
			}
			if !strings.Contains(tt.got, config.RedactedPlaceholder) {
				t.Fatalf("expected %q in %q", config.RedactedPlaceholder, tt.got)
			}
		})
	}
}

func TestSecretLogValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	// A bare slog handler with no redaction of its own: the redaction under
	// test must be the Secret's, not the logger's.
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("call", "harmless_name", config.NewSecret(leaked))

	out := buf.String()
	if strings.Contains(out, leaked) {
		t.Fatalf("secret reached the log: %s", out)
	}
	if !strings.Contains(out, config.RedactedPlaceholder) {
		t.Fatalf("expected %q in %q", config.RedactedPlaceholder, out)
	}
}

func TestSecretReveal(t *testing.T) {
	t.Parallel()

	if got := config.NewSecret(leaked).Reveal(); got != leaked {
		t.Fatalf("Reveal() = %q, want %q", got, leaked)
	}
}

func TestSecretIsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret config.Secret
		want   bool
	}{
		{name: "unset", secret: config.Secret{}, want: true},
		{name: "set", secret: config.NewSecret(leaked), want: false},
		// A secret that is whitespace was set to something; only the
		// empty string means "not configured".
		{name: "whitespace is set", secret: config.NewSecret(" "), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.secret.IsZero(); got != tt.want {
				t.Fatalf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}
