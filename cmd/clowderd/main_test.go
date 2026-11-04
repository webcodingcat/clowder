//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/agent"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestExitStatusTellsTheErrorsSystemdMustNotRestartAfter(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"an unreadable Raft entry, wrapped", fmt.Errorf("apply: %w", state.ErrUnreadable), exitUnreadable},
		{"a retired server, wrapped", fmt.Errorf("start: %w", agent.ErrRetired), exitRetired},
		{"any other error", errors.New("no such file"), 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitStatus(test.err); got != test.want {
				t.Errorf("exitStatus(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

// The unit must list the agent's own statuses, or systemd restarts an agent
// that would stop at the same place again.
func TestTheUnitDoesNotRestartAfterTheAgentsOwnStatuses(t *testing.T) {
	unit, err := os.ReadFile("../../packaging/systemd/clowderd.service")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^RestartPreventExitStatus=(.*)$`).FindSubmatch(unit)
	if match == nil {
		t.Fatal("clowderd.service has no RestartPreventExitStatus")
	}
	statuses := strings.Fields(string(match[1]))
	for _, status := range []int{exitUnreadable, exitRetired} {
		if !slices.Contains(statuses, fmt.Sprint(status)) {
			t.Errorf("RestartPreventExitStatus=%s lacks %d", match[1], status)
		}
	}
}
