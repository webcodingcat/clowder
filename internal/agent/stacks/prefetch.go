package stacks

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/state"
)

// Puller pulls an image unless Docker has it (docker.Client.PullMissing),
// with the logins of the Docker config directory config.
type Puller interface {
	PullMissing(ctx context.Context, config, image string) error
}

// Prefetcher pulls the images of the stacks that this host may take over,
// so that a failover or a move starts the containers at once. It pulls one
// image at a time in its own loop, so a long pull holds up no other loop.
// It pulls without logins: a host gets a namespace's logins only once one
// of its stacks runs there, so an image that needs a login is pulled when
// its stack starts here.
type Prefetcher struct {
	host       state.HostID
	cluster    loop.Cluster
	docker     Puller
	registries *RegistryConfigs
	logger     *slog.Logger

	mu sync.Mutex
	// pulled holds the images that Docker had at the last pass. needLogin
	// holds the images of namespaces with image pull secrets that did not
	// pull without a login; a move does not wait for them.
	pulled, needLogin map[string]bool
}

// NewPrefetcher creates the prefetcher, which shares the runner's
// registries. It starts nothing.
func NewPrefetcher(host state.HostID, cluster loop.Cluster, docker Puller, registries *RegistryConfigs,
	logger *slog.Logger,
) *Prefetcher {
	return &Prefetcher{
		host: host, cluster: cluster, docker: docker, registries: registries, logger: logger,
		pulled: map[string]bool{}, needLogin: map[string]bool{},
	}
}

// Run pulls until ctx ends.
func (p *Prefetcher) Run(ctx context.Context) {
	loop.Run(ctx, p.cluster, nil, func() { p.pass(ctx) })
}

// Ready reports whether Docker has every image of a stack, by its ID, that
// pulls without a login. A move to this host waits until it does.
func (p *Prefetcher) Ready(stackID string) bool {
	copied := p.cluster.SharedCopy()
	stack, ok := copied.Stacks[stackID]
	if !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, image := range images(stack) {
		if !p.pulled[image] && !p.needLogin[image] {
			return false
		}
	}
	return true
}

// pass brings back an image removed by hand. If a tag now points to a new
// image, that image is pulled only when a user restarts the stack
// (Runner.pullRestarted).
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
	config, err := p.registries.WithoutLogins()
	if err != nil {
		p.logger.Warn("pull the images of the stacks this host may take", "error", err)
		return
	}
	present, needLogin := map[string]bool{}, map[string]bool{}
	for _, stack := range slices.Concat(moving, others) {
		withLogins := hasPullSecrets(copied, stack.Namespace)
		for _, image := range images(stack) {
			if present[image] {
				continue
			}
			err := p.docker.PullMissing(ctx, config, image)
			switch {
			case err == nil:
				present[image] = true
			case ctx.Err() != nil:
			case withLogins:
				// expected for a private image: it pulls when the stack
				// starts here, with the namespace's logins. Debug, as it
				// repeats at every pass; a failure at the start shows in
				// get stacks.
				p.logger.Debug("an image of a namespace with logins did not pull without them", "image", image,
					"error", err)
				needLogin[image] = true
			default:
				p.logger.Warn("pull the image of a stack this host may take", "image", image, "error", err)
			}
		}
	}
	p.mu.Lock()
	p.pulled, p.needLogin = present, needLogin
	p.mu.Unlock()
}

func hasPullSecrets(current *state.State, namespace string) bool {
	held, ok := current.Namespaces[namespace]
	return ok && len(held.Spec.ImagePullSecrets) > 0
}

// mayTake reports whether a host may take over a stack in a failover or a
// move: it is a worker in service, and it has a copy with a disk of each
// volume that holds data (replicas 1 and more). A stack that already runs
// here gets its images from compose up.
func mayTake(current *state.State, stack *state.Stack, host state.HostID) bool {
	self, ok := current.Hosts[host]
	if !ok || !state.InService(self.Phase) || !self.Roles.Has(hostrole.Worker) {
		return false
	}
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
	return true
}

func images(stack *state.Stack) []string {
	var all []string
	for _, name := range slices.Sorted(maps.Keys(stack.Spec.Services)) {
		all = append(all, stack.Spec.Services[name].Image)
	}
	return slices.Compact(all)
}
