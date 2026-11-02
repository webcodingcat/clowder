package stacks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/webcodingcat/clowder/internal/api"
)

// logIn logs Docker in to each private registry whose login changed.
func (r *Runner) logIn(ctx context.Context, desired desiredState) {
	for _, registry := range slices.Sorted(maps.Keys(desired.registries)) {
		login := desired.registries[registry]
		password, err := r.open(login.Password)
		if err != nil {
			r.logger.Error("open a registry's password", "registry", registry, "error", err)
			continue
		}
		hash := sha256.Sum256([]byte(login.Username + "\n" + string(password)))
		if r.logins[registry] == hash {
			continue
		}
		if err := r.docker.Login(ctx, registry, login.Username, string(password)); err != nil {
			r.logger.Error("log in to a registry", "registry", registry, "error", err)
			continue
		}
		r.logins[registry] = hash
	}
}

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
