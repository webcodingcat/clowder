package agent

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
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// logIn logs Docker in to each private registry whose login changed.
func (r *stackRunner) logIn(ctx context.Context, desired desiredState) {
	for _, registry := range slices.Sorted(maps.Keys(desired.registries)) {
		opened, err := r.open(desired.registries[registry])
		if err != nil {
			r.logger.Error("open a registry login", "registry", registry, "error", err)
			continue
		}
		hash := sha256.Sum256(opened)
		if r.logins[registry] == hash {
			continue
		}
		var login clusterdir.RegistryLogin
		if err := json.Unmarshal(opened, &login); err != nil {
			r.logger.Error("read a registry login", "registry", registry, "error", err)
			continue
		}
		if err := r.docker.Login(ctx, registry, login.Username, login.Password); err != nil {
			r.logger.Error("log in to a registry", "registry", registry, "error", err)
			continue
		}
		r.logins[registry] = hash
	}
}

// rules tells the supervisor each service's restart rule, from the stack as
// the state has it: a pushed-out service never restarts. Clowder's own
// Traefik has a rule of its own.
func (r *stackRunner) rules(project, service string) (supervisor.Rule, bool) {
	if clowdersOwn(project) {
		return traefikRule, true
	}
	namespace, stackName, ok := names.SplitProject(project)
	if !ok {
		return supervisor.Rule{}, false
	}
	var source string
	var pushedOut bool
	r.cluster.Read(func(current *state.State) {
		stack, ok := current.Stacks[names.ID(namespace, stackName)]
		if ok && stack.Host != nil && *stack.Host == r.host.ID {
			source, pushedOut = stack.Source, slices.Contains(stack.PushedOut, service)
		}
	})
	if source == "" {
		return supervisor.Rule{}, false
	}
	if pushedOut {
		return supervisor.Rule{Restart: "no"}, true
	}
	parsed, err := compose.Parse(context.Background(), stackName, []byte(source))
	if err != nil {
		return supervisor.Rule{}, false
	}
	found, ok := parsed.Services[service]
	if !ok {
		return supervisor.Rule{}, false
	}
	return supervisor.Rule{Restart: found.Restart, RestartUnhealthy: found.RestartUnhealthy}, true
}

// Ps lists a stack's containers on this host (api.Containers).
func (r *stackRunner) Ps(ctx context.Context, project string) ([]api.ContainerInfo, error) {
	output, err := r.docker.ComposePs(ctx, project, r.files.stackFile(project))
	if err != nil {
		return nil, err
	}
	return parsePs(output)
}

// Logs writes a stack's logs on this host (api.Containers).
func (r *stackRunner) Logs(ctx context.Context, project string, follow bool, out io.Writer) error {
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
