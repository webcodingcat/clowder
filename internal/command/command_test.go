package command

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunReturnsTheOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	output, err := Run(context.Background(), time.Second, "sh", "-c", "echo hello")
	if err != nil || output != "hello\n" {
		t.Errorf("%q, %v", output, err)
	}
	_, err = Run(context.Background(), time.Second, "sh", "-c", "echo broken >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("want the error output in the error, got %v", err)
	}
	if code, ok := ExitCode(err); !ok || code != 3 {
		t.Errorf("exit code %d, %v", code, ok)
	}
	output, err = RunWithInput(context.Background(), time.Second, "from stdin", "cat")
	if err != nil || output != "from stdin" {
		t.Errorf("input: %q, %v", output, err)
	}
	if _, ok := ExitCode(errors.New("not a command")); ok {
		t.Error("an error that is no exit has an exit code")
	}
}

func TestRunGivesUpOnAHangingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	start := time.Now()
	_, err := Run(context.Background(), 100*time.Millisecond, "sleep", "10")
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Errorf("want a timeout, got %v", err)
	}
	if _, ok := ExitCode(err); ok {
		t.Error("a timeout counts as an exit")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("Run waited %s for a hanging command", waited)
	}
}
