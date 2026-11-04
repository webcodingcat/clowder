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

const (
	// exitUnreadable ends an agent that met a Raft entry or snapshot it
	// cannot read. clowderd.service does not restart it
	// (RestartPreventExitStatus): it would stop at the same entry again, and
	// a member that comes and goes would keep the other hosts from seeing it
	// fail.
	exitUnreadable = 3
	// exitRetired ends the agent of a server clowderd recover retired;
	// clowderd.service does not restart it either.
	exitRetired = 4
)

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.JSON, slog.LevelInfo))
	if err := agent.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		switch {
		case errors.Is(err, state.ErrUnreadable):
			os.Exit(exitUnreadable)
		case errors.Is(err, agent.ErrRetired):
			os.Exit(exitRetired)
		}
		os.Exit(1)
	}
}
