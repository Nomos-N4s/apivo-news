package config_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// leaked is the value every assertion below looks for in the output. If it
// appears anywhere, a print path is not covered.
const leaked = "sk_live_do_not_print_me"

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()

	secret := config.Secret(leaked)
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
		{name: "verb s", got: fmt.Sprintf("%s", secret)},
		{name: "verb q", got: fmt.Sprintf("%q", secret)},
		{name: "verb v", got: fmt.Sprintf("%v", secret)},
		{name: "verb plus v", got: fmt.Sprintf("%+v", secret)},
		{name: "verb sharp v", got: fmt.Sprintf("%#v", secret)},
		{name: "inside a struct, verb v", got: fmt.Sprintf("%v", holder)},
		{name: "inside a struct, verb plus v", got: fmt.Sprintf("%+v", holder)},
		{name: "inside a struct, verb sharp v", got: fmt.Sprintf("%#v", holder)},
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

func TestSecretLogValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	// A bare slog handler with no redaction of its own: the redaction under
	// test must be the Secret's, not the logger's.
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("call", "harmless_name", config.Secret(leaked))

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

	if got := config.Secret(leaked).Reveal(); got != leaked {
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
		{name: "unset", secret: "", want: true},
		{name: "set", secret: config.Secret(leaked), want: false},
		// A secret that is whitespace was set to something; only the
		// empty string means "not configured".
		{name: "whitespace is set", secret: " ", want: false},
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
