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
	// exitUnreadable ends an agent that found a Raft entry or snapshot it
	// cannot read. clowderd.service does not restart it
	// (RestartPreventExitStatus): it would stop at the same entry again, and
	// a member that keeps coming and going would hide its failure from the
	// other hosts.
	exitUnreadable = 3
	// exitRetired ends the agent of a server that clowderd recover retired;
	// clowderd.service does not restart it either.
	exitRetired = 4
)

func main() {
	slog.SetDefault(logging.New(os.Stderr, logging.JSON, slog.LevelInfo))
	if err := agent.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clowderd:", err)
		os.Exit(exitStatus(err))
	}
}

// exitStatus returns the status that clowderd exits with after err. Each
// error after which systemd must not restart the agent gets its own status;
// any other error gets 1.
func exitStatus(err error) int {
	switch {
	case errors.Is(err, state.ErrUnreadable):
		return exitUnreadable
	case errors.Is(err, agent.ErrRetired):
		return exitRetired
	}
	return 1
}
