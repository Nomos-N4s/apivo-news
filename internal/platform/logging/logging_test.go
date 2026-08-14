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
