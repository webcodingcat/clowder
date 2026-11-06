// Package service starts and stops clowderd.service through systemctl, for
// init, join and recover.
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// Unit is the agent's systemd unit.
const Unit = "clowderd.service"

// Timeout limits each systemctl call. systemctl waits until the agent has
// started or stopped, and a stop waits for the agent's loops to end
// (well within this).
const Timeout = time.Minute

// Start starts the agent.
func Start(ctx context.Context) error { return systemctl(ctx, "start") }

// Stop stops the agent and waits until it has ended.
func Stop(ctx context.Context) error { return systemctl(ctx, "stop") }

// EnableAndStart starts the agent now and at every boot.
func EnableAndStart(ctx context.Context) error { return systemctl(ctx, "enable", "--now") }

func systemctl(ctx context.Context, action ...string) error {
	if _, err := command.Run(ctx, Timeout, "systemctl", append(action, Unit)...); err != nil {
		return fmt.Errorf("systemctl %s %s: %w", strings.Join(action, " "), Unit, err)
	}
	return nil
}
