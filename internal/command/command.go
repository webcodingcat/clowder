// Package command runs the host's tools (lvs, drbdadm, mkfs, nft, ip,
// systemctl, ...) so that none can hang the agent. Every host tool goes
// through it, with no exception: without quorum, DRBD suspends I/O, and a
// process touching a volume then hangs in the kernel in state D, where even
// SIGKILL does not reach it; and a tool that hangs for any other reason must
// not stop a reconciler either. Run gives up on such a process after its
// timeout and leaves it behind, instead of waiting with it.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ExitError is a command that ran and exited with a code other than 0, as
// opposed to one that did not start or did not finish. Callers that read a
// tool's exit code (blkid's 2: no file system) check for it.
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

// ExitCode tells the exit code of a command that ran and failed; ok is false
// for any other error (a timeout, a missing tool).
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

// Stream runs a command that writes for as long as it runs (logs that
// follow), passing its output on as it comes. It has no timeout: ctx ends
// it. Only for tools that touch no volume, as it waits for the process.
func Stream(ctx context.Context, out io.Writer, name string, args ...string) error {
	var stderr strings.Builder
	process := exec.CommandContext(ctx, name, args...)
	process.Stdout = out
	process.Stderr = &stderr
	if err := process.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RunWithInput is Run with input on the command's standard input.
func RunWithInput(ctx context.Context, timeout time.Duration, input, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var stderr strings.Builder
		process := exec.CommandContext(ctx, name, args...)
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
	case <-ctx.Done():
		return "", fmt.Errorf("%s %s: no answer within %s: %w", name, strings.Join(args, " "), timeout, ctx.Err())
	}
}
