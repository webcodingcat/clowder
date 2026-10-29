// Package command runs the host's tools (lvs, drbdadm, mkfs, ...) so that
// none can hang the agent. Without quorum, DRBD suspends I/O, and a process
// touching a volume then hangs in the kernel in state D, where even SIGKILL
// does not reach it. Run gives up on such a process after its timeout and
// leaves it behind, instead of waiting with it.
package command

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Run runs a command and returns its standard output. If it does not finish
// within timeout, Run returns an error and leaves the process (and the
// goroutine waiting for it) behind.
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
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
		process.Stderr = &stderr
		output, err := process.Output()
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
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
