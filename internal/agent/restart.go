package agent

import (
	"slices"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// restartRules tell the supervisor the restart rules of this host's
// services (restartRules.of is a supervisor.Rules).
type restartRules struct {
	cluster loop.Cluster
	host    state.HostID
}

// of is a service's restart rule, taken from the stack as the state has it.
// A pushed-out service never restarts. Clowder's own Traefik has its own
// rule.
func (r restartRules) of(project, service string) (supervisor.Rule, bool) {
	if names.ClowdersOwn(project) {
		return traefikRule, true
	}
	namespace, stackName, ok := names.SplitProject(project)
	if !ok {
		return supervisor.Rule{}, false
	}
	var rule supervisor.Rule
	var known bool
	r.cluster.Read(func(current *state.State) {
		stack, ok := current.Stacks[names.ID(namespace, stackName)]
		if !ok || stack.Host == nil || *stack.Host != r.host {
			return
		}
		found, ok := stack.Spec.Services[service]
		if !ok {
			return
		}
		rule, known = supervisor.Rule{Restart: found.Restart, RestartUnhealthy: found.RestartsUnhealthy()}, true
		if slices.Contains(stack.PushedOut, service) {
			rule = supervisor.Rule{Restart: "no"}
		}
	})
	return rule, known
}
