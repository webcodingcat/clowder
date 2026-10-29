package docker

import (
	"context"
	"io"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// Timeouts of docker compose; up pulls images, which may take a while.
const (
	upTimeout   = 15 * time.Minute
	downTimeout = 5 * time.Minute
	readTimeout = 30 * time.Second
)

// ComposeUp starts or updates a stack from its rendered Compose files (the
// stack, and its secret variables if it has any): Docker recreates only the
// containers whose configuration changed.
func (c *Client) ComposeUp(ctx context.Context, stack string, files []string) error {
	args := []string{"compose", "--project-name", stack}
	for _, file := range files {
		args = append(args, "--file", file)
	}
	_, err := command.Run(ctx, upTimeout, "docker", append(args, "up", "--detach", "--remove-orphans")...)
	return err
}

// ComposeDown stops and removes a stack's containers, networks and Docker
// volumes. The data stays: a Docker volume only binds a directory of
// clowder's volume (compose.Render), and that is clowder's to remove.
func (c *Client) ComposeDown(ctx context.Context, stack, file string) error {
	_, err := command.Run(ctx, downTimeout, "docker", "compose", "--project-name", stack, "--file", file,
		"down", "--remove-orphans", "--volumes")
	return err
}

// ComposePs lists a stack's containers as JSON, one object per line.
func (c *Client) ComposePs(ctx context.Context, stack, file string) (string, error) {
	return command.Run(ctx, readTimeout, "docker", "compose", "--project-name", stack, "--file", file,
		"ps", "--all", "--format", "json")
}

// ComposeLogs writes a stack's logs to out; with follow, until ctx ends.
func (c *Client) ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error {
	args := []string{"compose", "--project-name", stack, "--file", file, "logs", "--no-color", "--timestamps"}
	if follow {
		args = append(args, "--follow")
	}
	return command.Stream(ctx, out, "docker", args...)
}

// Login signs in to a private registry; the password goes on standard
// input, never on the command line.
func (c *Client) Login(ctx context.Context, registry, username, password string) error {
	_, err := command.RunWithInput(ctx, readTimeout, password, "docker", "login", "--username", username,
		"--password-stdin", registry)
	return err
}
