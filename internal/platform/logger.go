package platform

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process-wide structured logger from Config. It writes to
// stdout (the twelve-factor convention: the runtime, not the app, decides where
// logs go) using either the JSON or text slog handler.
//
// An unrecognized level falls back to info rather than failing — logging setup
// should never be the reason a server refuses to start.
func NewLogger(cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}

	var handler slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// parseLevel maps a config string to an slog.Level, defaulting to info.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
