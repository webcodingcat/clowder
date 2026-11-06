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

// certificateKeys gives the certificates' keys in clear while this host
// holds the Floating IP (cachedCertificateKeys).
type certificateKeys interface {
	open(ctx context.Context, certificate state.Certificate) ([]byte, error)
	// forget drops the keys kept in memory, once the host no longer holds
	// the Floating IP
	forget()
}

// chainDigest tells the certificates of one domain apart: a renewal has a
// new chain.
func chainDigest(chain []byte) string {
	sum := sha256.Sum256(chain)
	return hex.EncodeToString(sum[:])
}

// traefikRunner has its own loop next to the stack runner, so that a
// stack's long image pull does not hold up the ingress.
type traefikRunner struct {
	files   Files
	host    Host
	cluster loop.Cluster
	docker  Docker
	keys    certificateKeys
	logger  *slog.Logger
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

func (t *traefikRunner) run(ctx context.Context) {
	loop.Run(ctx, t.cluster, t.signal.Wake(), func() {
		if err := t.pass(ctx); err != nil {
			t.logger.Error("run Traefik", "error", err)
		}
	})
}

// pass runs compose up only for a new Compose file or static configuration,
// or once after the agent starts. Routes and certificates change without a
// restart, because Traefik watches the routes' directory.
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
	if !input.ingress {
		t.keys.forget()
	}
	certificates, err := t.writeCertificates(ctx, input.certificates)
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

type traefikInput struct {
	floatingIP string
	// routes are the routes of the stacks on this host, by project, then
	// by service
	routes map[string]map[string]stackspec.HTTP
	// forwarded are the routes of the stacks on other hosts, which lead to
	// those hosts' Traefik. Every host routes every stack, so the Floating
	// IP may move to any host
	forwarded []ingress.Route
	// ingress says whether the state names this host for the Floating IP;
	// certificates are empty unless it does
	ingress      bool
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
		// only the host that holds the Floating IP terminates TLS, and the
		// control plane gives the keys to it alone. It needs every
		// certificate, because it answers for the stacks of every host.
		if input.ingress = current.Ingress != nil && *current.Ingress == t.host.ID; !input.ingress {
			return
		}
		for _, domain := range slices.Sorted(maps.Keys(current.Certificates)) {
			// a copy is safe: Apply replaces a certificate, it never changes one
			input.certificates = append(input.certificates, *current.Certificates[domain])
		}
	})
	return input
}

// writeCertificates names the files after the certificate's chain, so
// that a renewed certificate gets new files. A key that cannot be
// opened, and has no files from an earlier pass, leaves only its domain on
// plain HTTP.
func (t *traefikRunner) writeCertificates(ctx context.Context, certificates []state.Certificate) (
	[]ingress.CertificateFiles, error,
) {
	// create the directory even without certificates: Traefik's Compose
	// file mounts it, and Docker would create a missing directory that
	// everyone can read. Restrict it to root each time, even if it already
	// existed with other permissions.
	dir := t.files.traefikCertificatesDir()
	if err := atomicfile.EnsureDir(dir, 0o700); err != nil {
		return nil, err
	}
	var written []ingress.CertificateFiles
	for _, certificate := range certificates {
		name := certificate.Domain + "-" + chainDigest(certificate.Chain)[:16]
		files := ingress.CertificateFiles{Domain: certificate.Domain, Chain: name + ".crt", Key: name + ".key"}
		key, err := t.keys.open(ctx, certificate)
		if err != nil {
			// log once per certificate, not at every pass: the runner runs a
			// pass at every Docker event
			if !t.unopened[string(certificate.Key)] {
				t.logger.Error("open the key of a certificate", "domain", certificate.Domain, "error", err)
				t.unopened[string(certificate.Key)] = true
			}
			// the files of this chain from an earlier pass stay, so HTTPS
			// goes on while the control plane does not answer (after the
			// agent restarted without a leader)
			if fileExists(filepath.Join(dir, files.Key)) && fileExists(filepath.Join(dir, files.Chain)) {
				written = append(written, files)
			}
			continue
		}
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeOtherCertificates runs after the routes are written, so that no
// route names a removed file.
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
