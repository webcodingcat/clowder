package stacks

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// Puller pulls an image unless Docker has it (docker.Client.PullMissing),
// with the logins of the Docker config directory config.
type Puller interface {
	PullMissing(ctx context.Context, config, image string) error
}

// Prefetcher pulls the images of the stacks that this host may take over,
// before it needs them, so that a failover or a move starts the containers
// immediately. It runs its own loop, one pull at a time, so a long pull
// holds up neither the runner nor any other loop's pass. Like the runner, it
// pulls with the registry logins of each stack's namespace.
type Prefetcher struct {
	host       state.HostID
	cluster    loop.Cluster
	docker     Puller
	registries *RegistryConfigs
	logger     *slog.Logger

	mu sync.Mutex
	// pulled holds the images that Docker had at the last pass
	pulled map[string]bool
}

// NewPrefetcher creates the prefetcher, which shares the runner's
// registries. It starts nothing.
func NewPrefetcher(host state.HostID, cluster loop.Cluster, docker Puller, registries *RegistryConfigs,
	logger *slog.Logger,
) *Prefetcher {
	return &Prefetcher{
		host: host, cluster: cluster, docker: docker, registries: registries, logger: logger,
		pulled: map[string]bool{},
	}
}

// Run pulls at every change of the state and every loop.Interval, until
// ctx ends (loop.Run).
func (p *Prefetcher) Run(ctx context.Context) {
	loop.Run(ctx, p.cluster, nil, func() { p.pass(ctx) })
}

// Ready reports whether Docker has every image of a stack, by its ID. A
// move to this host waits until it does.
func (p *Prefetcher) Ready(stackID string) bool {
	copied := p.cluster.SharedCopy()
	stack, ok := copied.Stacks[stackID]
	if !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, image := range images(stack) {
		if !p.pulled[image] {
			return false
		}
	}
	return true
}

// pass makes sure that Docker has the images of the stacks this host may
// take. PullMissing pulls only what is missing, so an image removed by hand
// comes back. If a tag now points to a new image, that image is pulled only
// when a user restarts the stack (Runner.pullRestarted).
func (p *Prefetcher) pass(ctx context.Context) {
	copied := p.cluster.SharedCopy()
	// the stacks that move here come first, because a move waits for them
	var moving, others []*state.Stack
	for _, id := range slices.Sorted(maps.Keys(copied.Stacks)) {
		stack := copied.Stacks[id]
		switch move := copied.MoveOf(id); {
		case move != nil && move.Move.To == p.host:
			moving = append(moving, stack)
		case mayTake(copied, stack, p.host):
			others = append(others, stack)
		}
	}
	present := map[string]bool{}
	for _, stack := range slices.Concat(moving, others) {
		config, missing, err := p.registries.Write(copied, stack.Namespace)
		if err != nil {
			p.logger.Warn("pull the images of a stack this host may take", "stack", stack.ID(), "error", err)
			continue
		}
		p.pullMissing(ctx, config, images(stack), missing, present)
	}
	p.mu.Lock()
	p.pulled = present
	p.mu.Unlock()
}

// pullMissing pulls the images that Docker lacks and records the ones it
// has in present. The warning for a failed pull names the image pull
// secrets that were left out (missing).
func (p *Prefetcher) pullMissing(ctx context.Context, config string, stackImages []string, missing error,
	present map[string]bool,
) {
	for _, image := range stackImages {
		if present[image] {
			continue
		}
		if err := p.docker.PullMissing(ctx, config, image); err != nil {
			if ctx.Err() == nil {
				p.logger.Warn("pull the image of a stack this host may take", "image", image,
					"error", errors.Join(err, missing))
			}
			continue
		}
		present[image] = true
	}
}

// mayTake reports whether a host may take over a stack that runs elsewhere,
// in a failover or a move. That is the case when the host has a copy with a
// disk of each of the stack's volumes that hold data (replicas 1 and more).
// A stack without such volumes may go to any host that takes stacks. A
// stack that already runs here gets its images from compose up.
func mayTake(current *state.State, stack *state.Stack, host state.HostID) bool {
	if stack.Host != nil && *stack.Host == host {
		return false
	}
	if move := current.MoveOf(stack.ID()); move != nil && move.Move.To == host {
		return true
	}
	for _, name := range stack.Spec.VolumeNames() {
		volume, ok := current.Volumes[names.ID(stack.Namespace, name)]
		if !ok || volume.Spec.Replicas == 0 || volume.Status == state.VolumePending {
			continue
		}
		if !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == host && !c.Tiebreaker }) {
			return false
		}
	}
	self, ok := current.Hosts[host]
	return ok && state.InService(self.Phase)
}

// images returns the images of a stack's services.
func images(stack *state.Stack) []string {
	var all []string
	for _, name := range slices.Sorted(maps.Keys(stack.Spec.Services)) {
		all = append(all, stack.Spec.Services[name].Image)
	}
	return slices.Compact(all)
}
