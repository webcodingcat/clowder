package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
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
	// open unseals the certificates' keys (datakey.Key.Open)
	open   func(sealed state.Sealed) ([]byte, error)
	logger *slog.Logger
	// supervisor needs the stack runner's rules first, so Run sets it
	// afterwards, as the stack runner's
	supervisor *supervisor.Supervisor

	mu sync.Mutex
	// cameUp is the hash of the Compose file and traefik.yml that Traefik
	// last came up with; zero after the agent started or Docker restarted,
	// so it comes up again (restart: "no")
	cameUp [32]byte

	// wake starts a pass at once (buffered, 1)
	wake chan struct{}
	// unopened are the sealed keys that did not open, logged once; only
	// the runner's loop uses it
	unopened map[string]bool
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

// pass writes the certificates and the routes, then runs compose up when
// the Compose file or the static configuration changed (a new image pin, a
// new Floating IP) or Traefik has not come up since the agent started. The
// routes and certificates change without a restart: Traefik watches the
// routes' directory.
func (t *traefikRunner) pass(ctx context.Context) error {
	input := t.read()
	floatingIP, err := netip.ParseAddr(input.floatingIP)
	if err != nil {
		// empty before the first cluster apply, when there is nothing to
		// bind; cluster.yaml's check refuses any other value that does not
		// parse
		return nil
	}
	static, err := ingress.RenderStatic(ingress.Static{
		FloatingIP: floatingIP, NodeAddress: t.host.NodeAddress, Overlay: network.Overlay,
	})
	if err != nil {
		return err
	}
	file, err := ingress.RenderCompose(ingress.Mounts{
		Static: t.files.traefikStaticDir(), Dynamic: t.files.traefikDynamicDir(),
		Certificates: t.files.traefikCertificatesDir(),
	})
	if err != nil {
		return err
	}
	// the certificates first, then the routes that name them, so Traefik
	// finds both when it starts or reloads
	certificates, err := t.writeCertificates(input.certificates)
	if err != nil {
		return err
	}
	routes, err := t.routes(ctx, input.stacks)
	if err != nil {
		return err
	}
	rendered, err := ingress.RenderDynamic(routes, certificates)
	if err != nil {
		return err
	}
	// each file is compared with what is on disk, so one removed or changed
	// by hand comes back at the next pass
	if err := writeChanged(filepath.Join(t.files.traefikDynamicDir(), "routes.yaml"), rendered, 0o644); err != nil {
		return err
	}
	if err := t.removeOtherCertificates(certificates); err != nil {
		return err
	}
	if err := writeChanged(filepath.Join(t.files.traefikStaticDir(), "traefik.yml"), static, 0o644); err != nil {
		return err
	}
	if err := writeChanged(t.files.traefikCompose(), file, 0o600); err != nil {
		return err
	}
	sum := sha256.New()
	digest.WriteParts(sum, file, static)
	hash := [32]byte(sum.Sum(nil))
	t.mu.Lock()
	upToDate := t.cameUp == hash
	t.mu.Unlock()
	if upToDate {
		return nil
	}
	if err := composeUpHeld(ctx, t.docker, t.supervisor, ingress.Project, []string{t.files.traefikCompose()}, nil); err != nil {
		return err
	}
	t.mu.Lock()
	t.cameUp = hash
	t.mu.Unlock()
	return nil
}

// traefikInput is what Traefik's files depend on: the Floating IP, the
// sources of the stacks on this host and the certificates.
type traefikInput struct {
	floatingIP   string
	stacks       map[string]stackSource // by project
	certificates []state.Certificate
}

// stackSource is what the routes need of a stack: its name and Compose file.
type stackSource struct {
	name, source string
}

// read takes what Traefik's files depend on from the state, and nothing
// else: the runner passes at every event of Docker's, too often for a copy
// of the whole state under the lock that Raft's Apply waits for.
func (t *traefikRunner) read() traefikInput {
	input := traefikInput{stacks: map[string]stackSource{}}
	t.cluster.Read(func(current *state.State) {
		input.floatingIP = current.Config.Spec.Ingress.FloatingIP
		for _, stack := range current.Stacks {
			if stack.Host != nil && *stack.Host == t.host.ID {
				input.stacks[stack.Project()] = stackSource{name: stack.Name, source: stack.Source}
			}
		}
		// every certificate: the ingress host answers for the stacks of
		// every host (phase 2)
		for _, domain := range slices.Sorted(maps.Keys(current.Certificates)) {
			// a copy; Apply replaces a certificate, it never changes one
			input.certificates = append(input.certificates, *current.Certificates[domain])
		}
	})
	return input
}

// writeCertificates writes each certificate's chain and opened key into
// the certificates' directory, under names that change with the
// certificate (ingress.CertificateFiles). A key that does not open leaves
// its domain on plain HTTP; the others keep theirs.
func (t *traefikRunner) writeCertificates(certificates []state.Certificate) ([]ingress.CertificateFiles, error) {
	// also without certificates: Traefik's Compose file mounts it, and
	// Docker would make a missing one readable by all
	dir := t.files.traefikCertificatesDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make the certificates' directory: %w", err)
	}
	var written []ingress.CertificateFiles
	for _, certificate := range certificates {
		key, err := t.open(certificate.Key)
		if err != nil {
			// once per certificate, not at every pass: the runner passes
			// at every event of Docker's
			if !t.unopened[string(certificate.Key)] {
				t.logger.Error("open the key of a certificate", "domain", certificate.Domain, "error", err)
				t.unopened[string(certificate.Key)] = true
			}
			continue
		}
		sum := sha256.Sum256(certificate.Chain)
		name := certificate.Domain + "-" + hex.EncodeToString(sum[:8])
		files := ingress.CertificateFiles{Domain: certificate.Domain, Chain: name + ".crt", Key: name + ".key"}
		if err := writeChanged(filepath.Join(dir, files.Key), key, 0o600); err != nil {
			return nil, err
		}
		if err := writeChanged(filepath.Join(dir, files.Chain), certificate.Chain, 0o600); err != nil {
			return nil, err
		}
		written = append(written, files)
	}
	return written, nil
}

// removeOtherCertificates removes, once the routes no longer name them,
// the files of certificates that were renewed or deleted.
func (t *traefikRunner) removeOtherCertificates(certificates []ingress.CertificateFiles) error {
	keep := map[string]bool{}
	for _, certificate := range certificates {
		keep[certificate.Chain], keep[certificate.Key] = true, true
	}
	dir := t.files.traefikCertificatesDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list the certificates: %w", err)
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return fmt.Errorf("remove an old certificate: %w", err)
			}
		}
	}
	return nil
}

// writeChanged writes a file unless it holds content already.
func writeChanged(path string, content []byte, mode os.FileMode) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return nil
	}
	return atomicfile.Write(path, content, mode)
}

// routes are the HTTP routes of the stacks on this host, each with the
// containers that may take its requests: running, and healthy if they have
// a health check.
func (t *traefikRunner) routes(ctx context.Context, stacks map[string]stackSource) ([]ingress.Route, error) {
	var routes []ingress.Route
	ports := map[string]uint16{} // "<project>.<service>" -> the port requests go to
	for _, project := range slices.Sorted(maps.Keys(stacks)) {
		parsed, err := compose.Parse(ctx, stacks[project].name, []byte(stacks[project].source))
		if err != nil {
			t.logger.Warn("read the routes of a stack", "project", project, "error", err)
			continue
		}
		for _, serviceName := range slices.Sorted(maps.Keys(parsed.Services)) {
			if route := parsed.Services[serviceName].HTTP; route != nil {
				routes = append(routes, ingress.Route{
					Project: project, Service: serviceName, Host: route.Host, Path: route.Path,
				})
				ports[project+"."+serviceName] = route.Port
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
		routes[index].Targets = targets[routes[index].Project+"."+routes[index].Service]
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
