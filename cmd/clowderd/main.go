//go:build linux

// Command clowderd is the agent that runs on every clowder host (Linux only).
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/webcodingcat/clowder/internal/agent"
	"github.com/webcodingcat/clowder/internal/logging"
)

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.JSON, slog.LevelInfo))
	if err := agent.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		os.Exit(1)
	}
}
