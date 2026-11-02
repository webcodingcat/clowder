//go:build linux

// Command clowderd is the agent that runs on every clowder host (Linux only).
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/webcodingcat/clowder/internal/agent"
	"github.com/webcodingcat/clowder/internal/logging"
	"github.com/webcodingcat/clowder/internal/state"
)

// exitUnreadable ends an agent that met a Raft entry or snapshot it cannot
// read. clowderd.service does not restart it (RestartPreventExitStatus):
// it would stop at the same entry again, and a member that comes and goes
// would keep the other hosts from seeing it fail.
const exitUnreadable = 3

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.JSON, slog.LevelInfo))
	if err := agent.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		if errors.Is(err, state.ErrUnreadable) {
			os.Exit(exitUnreadable)
		}
		os.Exit(1)
	}
}
