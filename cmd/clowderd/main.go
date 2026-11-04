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
		os.Exit(exitStatus(err))
	}
}

// exitStatus is the status clowderd ends with after err. The errors after
// which systemd must not restart the agent get a status of their own; any
// other error gets 1.
func exitStatus(err error) int {
	switch {
	case errors.Is(err, state.ErrUnreadable):
		return exitUnreadable
	case errors.Is(err, agent.ErrRetired):
		return exitRetired
	}
	return 1
}
