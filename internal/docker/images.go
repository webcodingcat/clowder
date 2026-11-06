package docker

import (
	"context"

	"github.com/webcodingcat/clowder/internal/command"
)

// imageMissing is docker image inspect's exit code for an image that Docker
// does not have.
const imageMissing = 1

// PullMissing pulls an image unless Docker already has it, as compose up
// would. So a host that may take over a stack has its images before it
// needs them. The pull logs in with the registry logins of config
// (withConfig).
func (c *Client) PullMissing(ctx context.Context, config, image string) error {
	_, err := command.Run(ctx, readTimeout, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if code, ok := command.ExitCode(err); err == nil || !ok || code != imageMissing {
		return err
	}
	_, err = command.Run(ctx, upTimeout, "docker", withConfig(config, "pull", "--quiet", image)...)
	return err
}
