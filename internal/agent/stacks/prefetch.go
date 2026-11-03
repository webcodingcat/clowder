package stacks

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// Puller pulls an image unless Docker has it (docker.Client.PullMissing).
type Puller interface {
	PullMissing(ctx context.Context, image string) error
}

// Prefetcher pulls the images of the stacks this host may take over, before
// it needs them: a failover or a move then starts the containers at once.
// It runs a loop of its own, one pull at a time, so a long pull holds up
// neither the runner nor a pass of anything else. The runner logs Docker
// in to the registries; a pull before that fails and is tried again at the
// next pass.
type Prefetcher struct {
	host    state.HostID
	cluster loop.Cluster
	docker  Puller
	logger  *slog.Logger

	mu sync.Mutex
	// pulled are the images Docker had at the last pass
	pulled map[string]bool
}

// NewPrefetcher makes the prefetcher; it starts nothing.
func NewPrefetcher(host state.HostID, cluster loop.Cluster, docker Puller, logger *slog.Logger) *Prefetcher {
	return &Prefetcher{host: host, cluster: cluster, docker: docker, logger: logger, pulled: map[string]bool{}}
}

// Run pulls at every change of the state and every interval, until ctx
// ends (loop.Run).
func (p *Prefetcher) Run(ctx context.Context) {
	loop.Run(ctx, p.cluster, nil, func() { p.pass(ctx) })
}

// Ready tells whether Docker has every image of a stack, by its ID: a move
// to this host waits for it.
func (p *Prefetcher) Ready(stackID string) bool {
	copied, err := p.cluster.SharedCopy()
	if err != nil {
		return false
	}
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

// pass makes sure Docker has the images of the stacks this host may take:
// PullMissing pulls only what is missing, so an image removed by hand comes
// back. A tag that changed under the same name is pulled again only when
// a user restarts the stack (Runner.pullRestarted).
func (p *Prefetcher) pass(ctx context.Context) {
	copied, err := p.cluster.SharedCopy()
	if err != nil {
		p.logger.Error("read the state", "error", err)
		return
	}
	// the stacks that move here first: a move waits for them
	var moving, others []string
	for _, id := range slices.Sorted(maps.Keys(copied.Stacks)) {
		stack := copied.Stacks[id]
		switch move := copied.MoveOf(id); {
		case move != nil && move.Move.To == p.host:
			moving = append(moving, images(stack)...)
		case mayTake(copied, stack, p.host):
			others = append(others, images(stack)...)
		}
	}
	present := map[string]bool{}
	for _, image := range slices.Concat(moving, others) {
		if present[image] {
			continue
		}
		if err := p.docker.PullMissing(ctx, image); err != nil {
			if ctx.Err() == nil {
				p.logger.Warn("pull the image of a stack this host may take", "image", image, "error", err)
			}
			continue
		}
		present[image] = true
	}
	p.mu.Lock()
	p.pulled = present
	p.mu.Unlock()
}

// mayTake tells whether a host may take a stack that runs elsewhere, in a
// failover or a move: it has a copy with a disk of each of the stack's
// volumes that holds data (replicas 1 and more). A stack without such
// volumes may go to any host that takes stacks. A stack that runs here
// already gets its images from compose up.
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
	return ok && (self.Phase == state.Active || self.Phase == state.Draining)
}

// images are the images of a stack's services.
func images(stack *state.Stack) []string {
	var all []string
	for _, name := range slices.Sorted(maps.Keys(stack.Spec.Services)) {
		all = append(all, stack.Spec.Services[name].Image)
	}
	return slices.Compact(all)
}
