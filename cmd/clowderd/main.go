//go:build linux

// Command clowderd is the agent that runs on every clowder host (Linux only).
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/webcodingcat/clowder/internal/clowderd"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon"
	"github.com/webcodingcat/clowder/internal/logging"
	"github.com/webcodingcat/clowder/internal/state"
)

const (
	// exitUnreadable ends an agent that found a Raft entry or snapshot that
	// it cannot read. clowderd.service does not restart it
	// (RestartPreventExitStatus). The agent would stop at the same entry
	// again, and a member that keeps coming and going would hide its
	// failure from the other hosts.
	exitUnreadable = 3
	// exitRetired ends the agent of a server that clowderd recover retired.
	// clowderd.service does not restart it either.
	exitRetired = 4
	// exitRejoining ends the agent of a worker that clowderd recover kept,
	// so that it starts again as a fresh Raft member. clowderd.service
	// counts it as a success (SuccessExitStatus), so the unit does not show
	// a failure, and restarts the agent.
	exitRejoining = 5
)

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.JSON, slog.LevelInfo))
	if err := clowderd.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		os.Exit(exitStatus(err))
	}
}

// exitStatus returns the status that clowderd exits with after err. Each
// error that systemd must treat apart from a failure gets its own status.
// Any other error gets 1.
func exitStatus(err error) int {
	switch {
	case errors.Is(err, state.ErrUnreadable):
		return exitUnreadable
	case errors.Is(err, daemon.ErrRetired):
		return exitRetired
	case errors.Is(err, daemon.ErrRejoining):
		return exitRejoining
	}
	return 1
}
