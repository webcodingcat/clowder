package docker

import (
	"context"
	"io"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// Timeouts of docker compose. up pulls images, which may take a while.
const (
	upTimeout   = 15 * time.Minute
	downTimeout = 5 * time.Minute
	readTimeout = 30 * time.Second
)

// withConfig puts docker's --config (one directory per namespace) before
// args, so a stack pulls only with its own namespace's registry logins.
// Without --config, docker uses root's own logins.
func withConfig(config string, args ...string) []string {
	if config == "" {
		return args
	}
	return append([]string{"--config", config}, args...)
}

// ComposeUp starts or updates a stack from its rendered Compose files (the
// stack, and its secret variables if it has any). Docker recreates only the
// containers whose configuration changed. The services in leftOut are scaled
// to 0, which also removes their running containers, so no running service
// may depend on them. Missing images are pulled with the logins of config
// (withConfig).
func (c *Client) ComposeUp(ctx context.Context, config, stack string, files, leftOut []string) error {
	args := withConfig(config, "compose", "--project-name", stack)
	for _, file := range files {
		args = append(args, "--file", file)
	}
	args = append(args, "up", "--detach", "--remove-orphans")
	for _, service := range leftOut {
		args = append(args, "--scale", service+"=0")
	}
	_, err := command.Run(ctx, upTimeout, "docker", args...)
	return err
}

// ComposePull pulls the images of some services of a stack again, even the
// ones that Docker already has, because an image may have changed under the
// same tag. compose up pulls only missing images. ComposePull logs in with
// the registry logins of config (withConfig).
func (c *Client) ComposePull(ctx context.Context, config, stack string, files, services []string) error {
	args := withConfig(config, "compose", "--project-name", stack)
	for _, file := range files {
		args = append(args, "--file", file)
	}
	args = append(args, "pull")
	args = append(args, services...)
	_, err := command.Run(ctx, upTimeout, "docker", args...)
	return err
}

// ComposeDown stops and removes a stack's containers, networks and Docker
// volumes. The data stays, because a Docker volume only binds a directory
// of clowder's volume (compose.Render), and only clowder removes that.
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

// ComposeLogs writes a stack's logs to out. With follow, it goes on until
// ctx ends.
func (c *Client) ComposeLogs(ctx context.Context, stack, file string, follow bool, out io.Writer) error {
	args := []string{"compose", "--project-name", stack, "--file", file, "logs", "--no-color", "--timestamps"}
	if follow {
		args = append(args, "--follow")
	}
	return command.Stream(ctx, out, "docker", args...)
}
