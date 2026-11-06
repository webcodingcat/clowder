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

	"github.com/webcodingcat/clowder/internal/clowderd/daemon"
	"github.com/webcodingcat/clowder/internal/state"
)

func TestExitStatusTellsTheErrorsSystemdTreatsApart(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"an unreadable Raft entry, wrapped", fmt.Errorf("apply: %w", state.ErrUnreadable), exitUnreadable},
		{"a retired server, wrapped", fmt.Errorf("start: %w", daemon.ErrRetired), exitRetired},
		{"a rejoin", daemon.ErrRejoining, exitRejoining},
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
// that would stop at the same place again, and counts a rejoin as a
// failure.
func TestTheUnitKnowsTheAgentsOwnStatuses(t *testing.T) {
	unit, err := os.ReadFile("../../packaging/systemd/clowderd.service")
	if err != nil {
		t.Fatal(err)
	}
	for setting, want := range map[string][]int{
		"RestartPreventExitStatus": {exitUnreadable, exitRetired},
		"SuccessExitStatus":        {exitRejoining},
	} {
		match := regexp.MustCompile(`(?m)^` + setting + `=(.*)$`).FindSubmatch(unit)
		if match == nil {
			t.Errorf("clowderd.service has no %s", setting)
			continue
		}
		statuses := strings.Fields(string(match[1]))
		for _, status := range want {
			if !slices.Contains(statuses, fmt.Sprint(status)) {
				t.Errorf("%s=%s lacks %d", setting, match[1], status)
			}
		}
	}
}
