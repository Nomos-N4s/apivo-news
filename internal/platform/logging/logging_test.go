package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	"github.com/Nomos-N4s/apivo-news/internal/platform/logging"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      string
		level    slog.Level
		logAt    func(l *slog.Logger)
		wantLine bool
		wantJSON bool
	}{
		{
			name:     "prod emits JSON",
			env:      config.EnvProd,
			level:    slog.LevelInfo,
			logAt:    func(l *slog.Logger) { l.Info("hello", "k", "v") },
			wantLine: true,
			wantJSON: true,
		},
		{
			name:     "dev emits text",
			env:      config.EnvDev,
			level:    slog.LevelInfo,
			logAt:    func(l *slog.Logger) { l.Info("hello", "k", "v") },
			wantLine: true,
			wantJSON: false,
		},
		{
			name:     "records below level are dropped",
			env:      config.EnvDev,
			level:    slog.LevelWarn,
			logAt:    func(l *slog.Logger) { l.Info("too quiet") },
			wantLine: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := logging.New(&buf, tt.level, tt.env)
			tt.logAt(log)

			out := buf.String()
			if !tt.wantLine {
				if out != "" {
					t.Fatalf("expected no output, got %q", out)
				}
				return
			}
			if out == "" {
				t.Fatal("expected a log line, got none")
			}
			isJSON := json.Valid([]byte(strings.TrimSpace(out)))
			if isJSON != tt.wantJSON {
				t.Fatalf("output JSON = %v, want %v; line: %q", isJSON, tt.wantJSON, out)
			}
		})
	}
}

func TestIsSecretKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "empty key is not a secret", key: "", want: false},
		{name: "punctuation-only key is not a secret", key: "__", want: false},
		{name: "snake case api key", key: "api_key", want: true},
		{name: "camel case api key", key: "apiKey", want: true},
		{name: "kebab case api key", key: "API-KEY", want: true},
		{name: "dotted api key", key: "network.api.key", want: true},
		{name: "api secret", key: "api_secret", want: true},
		{name: "bare secret", key: "secret", want: true},
		{name: "blnk secret key", key: "blnk_secret_key", want: true},
		{name: "password", key: "password", want: true},
		{name: "passwd", key: "passwd", want: true},
		{name: "bearer token", key: "access_token", want: true},
		{name: "authorization header", key: "Authorization", want: true},
		{name: "credential", key: "network_credential", want: true},
		{name: "private key", key: "private_key", want: true},
		{name: "session key", key: "session_key", want: true},
		{name: "access key", key: "aws_access_key_id", want: true},
		{name: "connection string", key: "connection_string", want: true},
		{name: "dsn", key: "blnk_dsn", want: true},
		{name: "database url", key: "database_url", want: true},
		// The ingestion module logs feed URLs under this name on every
		// poll cycle. Redacting them would blind the operator to which
		// source failed, and the ones that carry credentials are already
		// redacted where they are read.
		{name: "plain url is not a secret", key: "url", want: false},
		{name: "source id is not a secret", key: "source_id", want: false},
		{name: "error is not a secret", key: "error", want: false},
		{name: "addr is not a secret", key: "addr", want: false},
		{name: "version is not a secret", key: "version", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := logging.IsSecretKey(tt.key); got != tt.want {
				t.Fatalf("IsSecretKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestNewRedactsSecretAttributes proves the redaction is in force in the
// logger the process actually builds, in both handlers, and that it survives
// nesting inside a group and being carried on a With logger.
func TestNewRedactsSecretAttributes(t *testing.T) {
	t.Parallel()

	const leaked = "hunter2-do-not-print"

	tests := []struct {
		name  string
		env   string
		logAt func(l *slog.Logger)
	}{
		{
			name:  "prod redacts a top-level attribute",
			env:   config.EnvProd,
			logAt: func(l *slog.Logger) { l.Info("call", "api_key", leaked) },
		},
		{
			name:  "dev redacts a top-level attribute",
			env:   config.EnvDev,
			logAt: func(l *slog.Logger) { l.Info("call", "api_key", leaked) },
		},
		{
			name:  "redacts inside a group",
			env:   config.EnvProd,
			logAt: func(l *slog.Logger) { l.Info("call", slog.Group("network", "apiSecret", leaked)) },
		},
		{
			name:  "redacts an attribute carried on a With logger",
			env:   config.EnvProd,
			logAt: func(l *slog.Logger) { l.With("password", leaked).Info("call") },
		},
		{
			name:  "redacts a config.Secret whatever the key",
			env:   config.EnvProd,
			logAt: func(l *slog.Logger) { l.Info("call", "harmless_name", config.NewSecret(leaked)) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := logging.New(&buf, slog.LevelInfo, tt.env)
			tt.logAt(log)

			out := buf.String()
			if strings.Contains(out, leaked) {
				t.Fatalf("secret reached the log: %q", out)
			}
			if !strings.Contains(out, logging.Redacted) {
				t.Fatalf("expected %q in the output, got %q", logging.Redacted, out)
			}
		})
	}
}

// TestNewKeepsNonSecretAttributes guards the other half: the redaction must
// not swallow the attributes an operator reads a log for.
func TestNewKeepsNonSecretAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo, config.EnvDev)
	log.Info("poll failed", "url", "https://example.test/feed.xml", "source_id", "abc-123")

	out := buf.String()
	for _, want := range []string{"https://example.test/feed.xml", "abc-123", "poll failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the output, got %q", want, out)
		}
	}
}
