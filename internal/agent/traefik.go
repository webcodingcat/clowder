package agent

import (
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

	"github.com/webcodingcat/clowder/internal/agent/loop"
	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

// traefikRule is how the supervisor restarts Traefik: always, and also when
// it turns unhealthy, because the host's ingress depends on it.
var traefikRule = supervisor.Rule{Restart: "always", RestartUnhealthy: true}

// traefikRunner runs clowder's Traefik on this host and writes its routes.
// Traefik is not a stack, so it has its own loop next to the stack runner.
// That way a stack's long image pull does not hold up the ingress.
type traefikRunner struct {
	files   Files
	host    Host
	cluster loop.Cluster
	docker  Docker
	// open decrypts the certificates' keys (datakey.Key.Decrypt)
	open   func(encrypted state.Encrypted) ([]byte, error)
	logger *slog.Logger
	// supervisor is paused while compose up recreates Traefik
	supervisor loop.Pauser
	// signal is how the supervisor starts a pass: after each Docker event,
	// so that a container that turns unhealthy leaves its route within
	// seconds, and after a restart of Docker
	signal *loop.Signal

	// Only the runner's loop uses the fields below, so they need no lock.

	// cameUp records what Traefik last came up with. It is zero after the
	// agent starts, so that Traefik comes up again (restart: "no")
	cameUp loop.BroughtUp
	// unopened holds the encrypted keys that could not be decrypted, so
	// that each is logged only once
	unopened map[string]bool
}

// run keeps Traefik and its routes the way this host needs them, until ctx
// ends (loop.Run). Each pass compares every file with what is on disk, so it
// also repairs what someone changed by hand.
func (t *traefikRunner) run(ctx context.Context) {
	loop.Run(ctx, t.cluster, t.signal.Wake(), func() {
		if err := t.pass(ctx); err != nil {
			t.logger.Error("run Traefik", "error", err)
		}
	})
}

// pass writes the certificates and the routes. Then it runs compose up if
// the Compose file or the static configuration changed (a new image pin, a
// new Floating IP), or if Traefik has not come up since the agent started.
// The routes and certificates change without a restart, because Traefik
// watches the routes' directory.
func (t *traefikRunner) pass(ctx context.Context) error {
	input := t.read()
	floatingIP, err := netip.ParseAddr(input.floatingIP)
	if err != nil {
		// empty before the first configuration, when there is nothing to
		// bind. The configuration's check refuses any other value that
		// does not parse.
		return nil
	}
	static, err := ingress.RenderStatic(ingress.Static{
		FloatingIP: floatingIP, NodeAddress: t.host.NodeAddress, Overlay: network.Overlay,
	})
	if err != nil {
		return fmt.Errorf("render traefik.yml: %w", err)
	}
	file, err := ingress.RenderCompose(ingress.Mounts{
		Static: t.files.traefikStaticDir(), Dynamic: t.files.traefikDynamicDir(),
		Certificates: t.files.traefikCertificatesDir(),
	})
	if err != nil {
		return fmt.Errorf("render Traefik's Compose file: %w", err)
	}
	// the certificates first, then the routes that name them, so Traefik
	// finds both when it starts or reloads
	certificates, err := t.writeCertificates(input.certificates)
	if err != nil {
		return err
	}
	routes, err := t.routes(ctx, input.routes)
	if err != nil {
		return err
	}
	routes = append(routes, input.forwarded...)
	rendered, err := ingress.RenderDynamic(routes, certificates)
	if err != nil {
		return fmt.Errorf("render Traefik's routes: %w", err)
	}
	// each file is compared with what is on disk, so a file removed or
	// changed by hand comes back at the next pass
	if err := atomicfile.WriteChanged(filepath.Join(t.files.traefikDynamicDir(), "routes.yaml"), rendered, 0o644); err != nil {
		return err
	}
	if err := t.removeOtherCertificates(certificates); err != nil {
		return err
	}
	if err := atomicfile.WriteChanged(filepath.Join(t.files.traefikStaticDir(), "traefik.yml"), static, 0o644); err != nil {
		return err
	}
	if err := atomicfile.WriteChanged(t.files.traefikCompose(), file, 0o600); err != nil {
		return err
	}
	sum := sha256.New()
	digest.WriteParts(sum, file, static)
	hash := [32]byte(sum.Sum(nil))
	// read before compose up, so a restart of Docker during compose up makes
	// Traefik come up again at the next pass
	upNow := loop.BroughtUp{Hash: hash, DockerRestarts: t.signal.Restarts()}
	if t.cameUp == upNow {
		return nil
	}
	err = loop.WhilePaused(t.supervisor, ingress.Project, func() error {
		// Traefik's image is public, so root's own Docker config pulls it
		return t.docker.ComposeUp(ctx, "", ingress.Project, []string{t.files.traefikCompose()}, nil)
	})
	if err != nil {
		return fmt.Errorf("compose up Traefik: %w", err)
	}
	t.cameUp = upNow
	return nil
}

// traefikInput is what Traefik's files depend on: the Floating IP, the
// HTTP routes of the stacks and the certificates.
type traefikInput struct {
	floatingIP string
	// routes are the routes of the stacks on this host, by project, then
	// by service
	routes map[string]map[string]stackspec.HTTP
	// forwarded are the routes of the stacks on other hosts, which lead to
	// those hosts' Traefik. Every host routes every stack, so the Floating
	// IP may move to any host
	forwarded    []ingress.Route
	certificates []state.Certificate
}

// read takes only what Traefik's files depend on from the state. The runner
// runs a pass at every Docker event. That is too often to copy the whole
// state under the lock that Raft's Apply waits for.
func (t *traefikRunner) read() traefikInput {
	input := traefikInput{routes: map[string]map[string]stackspec.HTTP{}}
	t.cluster.Read(func(current *state.State) {
		input.floatingIP = current.Config.Spec.Ingress.FloatingIP
		for _, stack := range current.Stacks {
			if stack.Host == nil {
				continue
			}
			for name, service := range stack.Spec.Services {
				switch {
				case service.HTTP == nil:
				case *stack.Host != t.host.ID:
					input.forwarded = append(input.forwarded, ingress.Route{
						Project: stack.Project(), Service: name, Host: service.HTTP.Host, Path: service.HTTP.Path,
						Targets:   []string{ingress.Target(network.NodeAddress(*stack.Host), ingress.InternalPort)},
						Forwarded: true,
					})
				default:
					if input.routes[stack.Project()] == nil {
						input.routes[stack.Project()] = map[string]stackspec.HTTP{}
					}
					input.routes[stack.Project()][name] = *service.HTTP
				}
			}
		}
		// every certificate, because the ingress host answers for the
		// stacks of every host
		for _, domain := range slices.Sorted(maps.Keys(current.Certificates)) {
			// a copy is safe: Apply replaces a certificate, it never changes one
			input.certificates = append(input.certificates, *current.Certificates[domain])
		}
	})
	return input
}

// writeCertificates writes each certificate's chain and decrypted key into
// the certificates' directory, under names that change with the
// certificate (ingress.CertificateFiles). A key that cannot be decrypted
// leaves its domain on plain HTTP. The other domains keep their
// certificates.
func (t *traefikRunner) writeCertificates(certificates []state.Certificate) ([]ingress.CertificateFiles, error) {
	// create the directory even without certificates: Traefik's Compose
	// file mounts it, and Docker would create a missing directory that
	// everyone can read. Restrict it to root each time, even if it already
	// existed with other permissions. It lies in SecretsDir, which Run
	// secures at the agent's start.
	dir := t.files.traefikCertificatesDir()
	if err := atomicfile.EnsureDir(dir, 0o700); err != nil {
		return nil, err
	}
	var written []ingress.CertificateFiles
	for _, certificate := range certificates {
		key, err := t.open(certificate.Key)
		if err != nil {
			// log once per certificate, not at every pass: the runner runs a
			// pass at every Docker event
			if !t.unopened[string(certificate.Key)] {
				t.logger.Error("open the key of a certificate", "domain", certificate.Domain, "error", err)
				t.unopened[string(certificate.Key)] = true
			}
			continue
		}
		sum := sha256.Sum256(certificate.Chain)
		name := certificate.Domain + "-" + hex.EncodeToString(sum[:8])
		files := ingress.CertificateFiles{Domain: certificate.Domain, Chain: name + ".crt", Key: name + ".key"}
		if err := atomicfile.WriteChanged(filepath.Join(dir, files.Key), key, 0o600); err != nil {
			return nil, err
		}
		if err := atomicfile.WriteChanged(filepath.Join(dir, files.Chain), certificate.Chain, 0o600); err != nil {
			return nil, err
		}
		written = append(written, files)
	}
	return written, nil
}

// removeOtherCertificates removes the files of certificates that were
// renewed or deleted, once the routes no longer name them.
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

// routes returns the HTTP routes of the stacks on this host, each with the
// containers that may take its requests: those that run and, if they have
// a health check, are healthy.
func (t *traefikRunner) routes(ctx context.Context, byProject map[string]map[string]stackspec.HTTP) ([]ingress.Route, error) {
	var routes []ingress.Route
	ports := map[string]uint16{} // "<project>.<service>" -> the port requests go to
	for _, project := range slices.Sorted(maps.Keys(byProject)) {
		for _, serviceName := range slices.Sorted(maps.Keys(byProject[project])) {
			route := byProject[project][serviceName]
			routes = append(routes, ingress.Route{
				Project: project, Service: serviceName, Host: route.Host, Path: route.Path,
			})
			ports[project+"."+serviceName] = route.Port
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
		// one call per container: Docker's list (API 1.45, Debian's docker.io)
		// has no health field, only the text "Up 3 minutes (healthy)", and
		// parsing that text is not worth the calls it would save
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

// firstAddress returns the container's address in the network whose name
// sorts first, so that the target stays the same from pass to pass.
// Traefik runs in the host network and reaches every bridge.
func firstAddress(addresses map[string]netip.Addr) (netip.Addr, bool) {
	for _, network := range slices.Sorted(maps.Keys(addresses)) {
		return addresses[network], true
	}
	return netip.Addr{}, false
}
