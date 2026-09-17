// Package logger provides a thin structured-logging wrapper around log/slog.
// Centralized so log level/format can be tuned from a single place.
package logger

import (
	"log/slog"
	"os"
)

// L is the package-level logger. Components should call logger.L directly
// rather than fmt.Println so that all output stays structured and filterable.
var L *slog.Logger

// Init configures the global logger. level "debug" enables verbose output,
// anything else sets Info level. Output goes to stderr so stdout stays clean
// for any future machine-parsed results.
func Init(level string) {
	var lvl slog.Level = slog.LevelInfo
	if level == "debug" {
		lvl = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	L = slog.New(handler)
	slog.SetDefault(L)
}
