package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// clowdersOwn tells whether a Compose project is one clowder runs itself
// (its Traefik) and no stack: names refuses stack names with this prefix.
// Every place that treats such a project apart asks it.
func clowdersOwn(project string) bool {
	return strings.HasPrefix(project, names.ReservedPrefix)
}

// traefikRule is how the supervisor restarts Traefik: always, and when it
// turns unhealthy, as the host's ingress depends on it.
var traefikRule = supervisor.Rule{Restart: "always", RestartUnhealthy: true}

// traefikRunner runs clowder's Traefik on this host and writes its routes.
// Traefik is no stack, so it has a loop of its own beside the stack runner:
// a long image pull of a stack does not hold up the ingress.
type traefikRunner struct {
	files   Files
	host    Host
	cluster stateReader
	docker  Docker
	logger  *slog.Logger
	// supervisor needs the stack runner's rules first, so Run sets it
	// afterwards, as the stack runner's
	supervisor *supervisor.Supervisor

	mu sync.Mutex
	// cameUp is the hash of the Compose file and traefik.yml that Traefik
	// last came up with; zero after the agent started or Docker restarted,
	// so it comes up again (restart: "no")
	cameUp [32]byte

	// writtenRoutes are the routes as the last pass wrote them; only the
	// loop's goroutine uses them
	writtenRoutes []byte
	// wake starts a pass at once (buffered, 1)
	wake chan struct{}
}

// run keeps Traefik and its routes as this host needs them, until ctx ends:
// at every change of the state, at once when a container changed (look),
// and every reconcileInterval.
func (t *traefikRunner) run(ctx context.Context) {
	for {
		// take the channel before reading, so no change between goes unseen
		changed := t.cluster.Changed()
		if err := t.pass(ctx); err != nil {
			t.logger.Error("run Traefik", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-t.wake:
		case <-time.After(reconcileInterval):
		}
	}
}

// look starts a pass at once; the supervisor calls it after each of
// Docker's events, so a container that turns unhealthy leaves its route
// within seconds.
func (t *traefikRunner) look() {
	select {
	case t.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// forget makes Traefik come up again in a pass it starts at once; the
// supervisor calls it when Docker restarted, which stopped every container.
func (t *traefikRunner) forget() {
	t.mu.Lock()
	t.cameUp = [32]byte{}
	t.mu.Unlock()
	t.look()
}

// pass writes the routes, then runs compose up when the Compose file or
// the static configuration changed (a new image pin, a new Floating IP) or
// Traefik has not come up since the agent started. The routes change
// without a restart: Traefik watches their directory.
func (t *traefikRunner) pass(ctx context.Context) error {
	desired, err := readDesired(t.cluster, t.host.ID)
	if err != nil {
		return err
	}
	floatingIP, err := netip.ParseAddr(desired.config.Ingress.FloatingIP)
	if err != nil {
		return nil // before the first cluster apply there is nothing to bind
	}
	static, err := ingress.RenderStatic(ingress.Static{
		FloatingIP: floatingIP, NodeAddress: t.host.NodeAddress, Overlay: network.Overlay,
	})
	if err != nil {
		return err
	}
	file, err := ingress.RenderCompose(ingress.Mounts{Static: t.files.traefikStaticDir(), Dynamic: t.files.traefikDynamicDir()})
	if err != nil {
		return err
	}
	// the routes first, so Traefik finds them when it starts
	if err := t.writeRoutes(ctx, desired); err != nil {
		return err
	}
	digest := sha256.New()
	writeParts(digest, file, static)
	hash := [32]byte(digest.Sum(nil))
	t.mu.Lock()
	upToDate := t.cameUp == hash
	t.mu.Unlock()
	if upToDate {
		return nil
	}
	if err := atomicfile.Write(filepath.Join(t.files.traefikStaticDir(), "traefik.yml"), static, 0o644); err != nil {
		return err
	}
	if err := atomicfile.Write(t.files.traefikCompose(), file, 0o600); err != nil {
		return err
	}
	if err := composeUpHeld(ctx, t.docker, t.supervisor, ingress.Project, []string{t.files.traefikCompose()}); err != nil {
		return err
	}
	t.mu.Lock()
	t.cameUp = hash
	t.mu.Unlock()
	return nil
}

// writeRoutes renders the routes of the stacks on this host and writes them
// for Traefik's file provider when they changed.
func (t *traefikRunner) writeRoutes(ctx context.Context, desired desiredState) error {
	routes, err := t.routes(ctx, desired)
	if err != nil {
		return err
	}
	rendered, err := ingress.RenderDynamic(routes)
	if err != nil {
		return err
	}
	if bytes.Equal(rendered, t.writtenRoutes) {
		return nil
	}
	if err := atomicfile.Write(filepath.Join(t.files.traefikDynamicDir(), "routes.yaml"), rendered, 0o644); err != nil {
		return err
	}
	t.writtenRoutes = rendered
	return nil
}

// routes are the HTTP routes of the stacks on this host, each with the
// containers that may take its requests: running, and healthy if they have
// a health check.
func (t *traefikRunner) routes(ctx context.Context, desired desiredState) ([]ingress.Route, error) {
	var routes []ingress.Route
	ports := map[string]uint16{} // "<stack>.<service>" -> the port requests go to
	for _, name := range slices.Sorted(maps.Keys(desired.stacks)) {
		parsed, err := compose.Parse(ctx, name, []byte(desired.stacks[name].Source))
		if err != nil {
			t.logger.Warn("read the routes of a stack", "stack", name, "error", err)
			continue
		}
		for _, serviceName := range slices.Sorted(maps.Keys(parsed.Services)) {
			if route := parsed.Services[serviceName].HTTP; route != nil {
				routes = append(routes, ingress.Route{Stack: name, Service: serviceName, Host: route.Host, Path: route.Path})
				ports[name+"."+serviceName] = route.Port
			}
		}
	}
	if len(routes) == 0 {
		return routes, nil
	}
	containers, err := t.docker.Containers(ctx, compose.LabelStack)
	if err != nil {
		return nil, err
	}
	targets := map[string][]string{}
	for _, container := range containers {
		key := container.Labels[docker.ProjectLabel] + "." + container.Labels[docker.ServiceLabel]
		port, routed := ports[key]
		if !routed || container.State != "running" {
			continue
		}
		details, err := t.docker.Inspect(ctx, container.ID)
		if errors.Is(err, docker.ErrNoContainer) {
			continue // gone since the list
		}
		if err != nil {
			return nil, err
		}
		address, ok := firstAddress(details.Addresses)
		if !details.Running || (details.Health != "" && details.Health != "healthy") || !ok {
			continue
		}
		targets[key] = append(targets[key], ingress.Target(address, port))
	}
	for index := range routes {
		routes[index].Targets = targets[routes[index].Stack+"."+routes[index].Service]
	}
	return routes, nil
}

// firstAddress is the container's address in the network whose name sorts
// first, so the target stays the same from pass to pass; Traefik in the host
// network reaches every bridge.
func firstAddress(addresses map[string]netip.Addr) (netip.Addr, bool) {
	for _, network := range slices.Sorted(maps.Keys(addresses)) {
		return addresses[network], true
	}
	return netip.Addr{}, false
}
