package docker

import (
	"context"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// Timeouts of docker compose; up pulls images, which may take a while.
const (
	upTimeout   = 15 * time.Minute
	downTimeout = 5 * time.Minute
	readTimeout = 30 * time.Second
)

// ComposeUp starts or updates a stack from its rendered Compose file:
// Docker recreates only the containers whose configuration changed.
func ComposeUp(ctx context.Context, stack, file string) error {
	_, err := command.Run(ctx, upTimeout, "docker", "compose", "--project-name", stack, "--file", file,
		"up", "--detach", "--remove-orphans")
	return err
}

// ComposeDown stops and removes a stack's containers and networks; its
// volumes stay (they are clowder's to remove).
func ComposeDown(ctx context.Context, stack, file string) error {
	_, err := command.Run(ctx, downTimeout, "docker", "compose", "--project-name", stack, "--file", file,
		"down", "--remove-orphans")
	return err
}

// ComposePs lists a stack's containers as JSON lines.
func ComposePs(ctx context.Context, stack, file string) (string, error) {
	return command.Run(ctx, readTimeout, "docker", "compose", "--project-name", stack, "--file", file,
		"ps", "--all", "--format", "json")
}

// Login signs in to a private registry; the password goes on standard
// input, never on the command line.
func Login(ctx context.Context, registry, username, password string) error {
	_, err := command.RunWithInput(ctx, readTimeout, password, "docker", "login", "--username", username,
		"--password-stdin", registry)
	return err
}
