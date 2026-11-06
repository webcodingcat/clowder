package stacks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/webcodingcat/clowder/internal/api"
)

// Ps lists a stack's containers on this host (api.Containers).
func (r *Runner) Ps(ctx context.Context, project string) ([]api.ContainerInfo, error) {
	output, err := r.docker.ComposePs(ctx, project, r.files.stackFile(project))
	if err != nil {
		return nil, err
	}
	return parsePs(output)
}

// Logs writes a stack's logs on this host (api.Containers).
func (r *Runner) Logs(ctx context.Context, project string, follow bool, out io.Writer) error {
	return r.docker.ComposeLogs(ctx, project, r.files.stackFile(project), follow, out)
}

// parsePs reads docker compose ps --format json: one object per line.
func parsePs(output string) ([]api.ContainerInfo, error) {
	containers := []api.ContainerInfo{}
	for line := range strings.Lines(output) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var container struct {
			Service, State, Health, Status string
		}
		if err := json.Unmarshal([]byte(line), &container); err != nil {
			return nil, fmt.Errorf("read docker compose ps: %w", err)
		}
		containers = append(containers, api.ContainerInfo{
			Service: container.Service, State: container.State, Health: container.Health, Status: container.Status,
		})
	}
	return containers, nil
}
