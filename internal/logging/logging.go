// Package logging sets up log/slog the same way for both binaries.
package logging

import (
	"io"
	"log/slog"
)

// Format is how log lines are written.
type Format int

const (
	// Text is for people at a terminal (the CLI).
	Text Format = iota
	// JSON is for journald and machines (the agent).
	JSON
)

// New returns a logger that writes to out in the given format and drops
// records below level.
func New(out io.Writer, format Format, level slog.Level) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	if format == JSON {
		return slog.New(slog.NewJSONHandler(out, options))
	}
	return slog.New(slog.NewTextHandler(out, options))
}
