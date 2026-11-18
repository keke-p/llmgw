// Package logging configures the process-wide structured (JSON) logger.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON slog.Logger writing to w at the given level.
func New(w io.Writer, level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}
