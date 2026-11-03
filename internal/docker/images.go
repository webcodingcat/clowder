package docker

import (
	"context"

	"github.com/webcodingcat/clowder/internal/command"
)

// imageMissing is docker image inspect's exit code for an image Docker
// does not have.
const imageMissing = 1

// PullMissing pulls an image unless Docker has it already, as compose up
// would: so a host that may take a stack over has its images before it
// needs them.
func (c *Client) PullMissing(ctx context.Context, image string) error {
	_, err := command.Run(ctx, readTimeout, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if code, ok := command.ExitCode(err); err == nil || !ok || code != imageMissing {
		return err
	}
	_, err = command.Run(ctx, upTimeout, "docker", "pull", "--quiet", image)
	return err
}
