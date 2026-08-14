// Package logging builds the process logger. Production output is JSON for
// machine ingestion; development output is human-readable text.
package logging

import (
	"io"
	"log/slog"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// New returns a logger writing to w, emitting records at or above level.
// In config.EnvProd the handler is JSON; in any other environment, text.
func New(w io.Writer, level slog.Level, env string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if env == config.EnvProd {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
