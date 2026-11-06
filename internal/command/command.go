// Package command runs the host's tools so that none of them can hang the
// agent: Run gives up on a process after its timeout and leaves it behind.
// Without quorum, DRBD suspends I/O, and a process that touches a volume
// then hangs in the kernel in state D, where not even SIGKILL ends it.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ExitError is the error of a command that ran and exited with a code other
// than 0, as opposed to one that did not start or did not finish. Callers
// that read a tool's exit code (blkid's 2 means no file system) check for
// it.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr)
}

// ExitCode returns the exit code of a command that ran and failed. ok is
// false for any other error (a timeout, a missing tool).
func ExitCode(err error) (code int, ok bool) {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code, true
	}
	return 0, false
}

// Run runs a command and returns its standard output. If it does not finish
// within timeout, Run returns an error and leaves the process (and the
// goroutine waiting for it) behind.
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return RunWithInput(ctx, timeout, "", name, args...)
}

// hostTool returns a tool's process in the C locale. clowder parses what
// tools print (lvs's sizes and percentages, findmnt) and matches their
// messages. A host's locale, such as one with a decimal comma, must not
// change either.
func hostTool(ctx context.Context, name string, args ...string) *exec.Cmd {
	process := exec.CommandContext(ctx, name, args...)
	process.Env = append(os.Environ(), "LC_ALL=C")
	// a child that the tool leaves running (a daemon it forks) may keep the
	// output open after the tool is killed. After waitDelay, Go closes the
	// output, so only a process in state D can block the waiting goroutine.
	process.WaitDelay = waitDelay
	return process
}

// waitDelay is how long a killed tool's output may stay open.
const waitDelay = 5 * time.Second

// Stream runs a command that writes for as long as it runs (logs that
// follow) and passes its output on as it comes. It has no timeout, and only
// ctx ends it. Use it only for tools that touch no volume, because it waits
// for the process.
func Stream(ctx context.Context, out io.Writer, name string, args ...string) error {
	var stderr strings.Builder
	process := hostTool(ctx, name, args...)
	process.Stdout = out
	process.Stderr = &stderr
	if err := process.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RunWithInput is Run with input on the command's standard input.
func RunWithInput(ctx context.Context, timeout time.Duration, input, name string, args ...string) (string, error) {
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var stderr strings.Builder
		process := hostTool(toolCtx, name, args...)
		process.Stdin = strings.NewReader(input)
		process.Stderr = &stderr
		output, err := process.Output()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			err = &ExitError{Code: exitErr.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
		}
		done <- result{output, err}
	}()
	select {
	case finished := <-done:
		if finished.err != nil {
			return string(finished.output), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), finished.err)
		}
		return string(finished.output), nil
	case <-toolCtx.Done():
		if ctx.Err() != nil {
			// the caller gave up (the agent stops) or ran out of its own
			// time. The tool did not time out.
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctx.Err())
		}
		return "", fmt.Errorf("%s %s: no answer within %s: %w", name, strings.Join(args, " "), timeout, toolCtx.Err())
	}
}
