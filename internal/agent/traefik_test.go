package agent

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/state"
)

// newTestTraefik is the Traefik runner beside newTestRunner's stack runner,
// on the same host, state and Docker; the stack shop has a route to its
// service app, and Docker has four containers of it: one healthy, one
// without a health check, one unhealthy, one exited.
func newTestTraefik(t *testing.T) (*traefikRunner, *stackRunner, *fakeDocker) {
	t.Helper()
	runner, cluster, fake, _ := newTestRunner(t)
	cluster.apply(state.KindPutStack, state.UserActor("alice"), state.PutStack{
		Name: "shop", Owner: state.OwnerCLI, ConfigRevision: 1,
		Source: "name: shop\nservices:\n  app:\n    image: nginx\n    volumes: [db:/data]\n" +
			"    x-clowder: {http: {host: shop.example.com, port: 8080}}\n" +
			"volumes:\n  db:\n    x-clowder: {class: fast, size: 1G, replicas: 0}\n",
	})
	labels := map[string]string{docker.ProjectLabel: "shop", docker.ServiceLabel: "app"}
	fake.containers = []docker.Container{
		{ID: "healthy", State: "running", Labels: labels},
		{ID: "unchecked", State: "running", Labels: labels},
		{ID: "unhealthy", State: "running", Labels: labels},
		{ID: "exited", State: "exited", Labels: labels},
	}
	fake.details = map[string]docker.Details{
		"healthy": {Running: true, Health: "healthy", Addresses: map[string]netip.Addr{
			"shop_default": netip.MustParseAddr("172.18.0.2"), "shop_zzz": netip.MustParseAddr("172.19.0.2"),
		}},
		"unchecked": {Running: true, Addresses: map[string]netip.Addr{"shop_default": netip.MustParseAddr("172.18.0.3")}},
		"unhealthy": {Running: true, Health: "unhealthy", Addresses: map[string]netip.Addr{"shop_default": netip.MustParseAddr("172.18.0.4")}},
	}
	traefik := &traefikRunner{
		files: runner.files, host: runner.host, cluster: cluster, docker: fake,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), supervisor: runner.supervisor,
		wake: make(chan struct{}, 1),
	}
	return traefik, runner, fake
}

func TestTraefikComesUpWithRoutesToTheContainersThatMayTakeRequests(t *testing.T) {
	traefik, _, fake := newTestTraefik(t)
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(traefik.files.traefikDynamicDir(), "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	routes := string(content)
	for _, want := range []string{"Host(`shop.example.com`)", "http://172.18.0.2:8080", "http://172.18.0.3:8080"} {
		if !strings.Contains(routes, want) {
			t.Errorf("no %s in the routes:\n%s", want, routes)
		}
	}
	for _, unwanted := range []string{"172.18.0.4", "172.19.0.2"} {
		if strings.Contains(routes, unwanted) {
			t.Errorf("%s in the routes (unhealthy, or not the first network):\n%s", unwanted, routes)
		}
	}
	static, err := os.ReadFile(filepath.Join(traefik.files.traefikStaticDir(), "traefik.yml"))
	if err != nil || !strings.Contains(string(static), "203.0.113.10:443") || !strings.Contains(string(static), "10.77.0.1:7480") {
		t.Errorf("traefik.yml %s, %v", static, err)
	}

	up := "up " + ingress.Project
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ups := fake.count(up); ups != 1 {
		t.Errorf("Traefik came up %d times, want once while nothing changed", ups)
	}
	// a file removed by hand comes back, without a restart of Traefik
	if err := os.Remove(filepath.Join(traefik.files.traefikDynamicDir(), "routes.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(traefik.files.traefikDynamicDir(), "routes.yaml")); err != nil {
		t.Errorf("the routes did not come back: %v", err)
	}
	if ups := fake.count(up); ups != 1 {
		t.Errorf("Traefik came up %d times, want once", ups)
	}
	traefik.forget() // Docker restarted
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ups := fake.count(up); ups != 2 {
		t.Errorf("Traefik came up %d times after Docker restarted, want twice", ups)
	}
}

func TestARouteFollowsItsContainersAtOnce(t *testing.T) {
	traefik, _, fake := newTestTraefik(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go traefik.run(ctx)
	waitForRoutes(t, traefik, func(routes string) bool { return strings.Contains(routes, "172.18.0.2") })

	fake.mu.Lock()
	fake.details["healthy"] = docker.Details{Running: true, Health: "unhealthy"}
	fake.mu.Unlock()
	traefik.look() // the supervisor saw health_status: unhealthy
	waitForRoutes(t, traefik, func(routes string) bool { return !strings.Contains(routes, "172.18.0.2") })
}

// waitForRoutes fails the test unless the routes come to be what want says
// within 5 s; the loop's reconcileInterval is far longer.
func waitForRoutes(t *testing.T, traefik *traefikRunner, want func(routes string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := os.ReadFile(filepath.Join(traefik.files.traefikDynamicDir(), "routes.yaml")); err == nil &&
			want(string(content)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the routes did not follow the containers")
}

func TestTraefikIsNoStack(t *testing.T) {
	_, runner, _ := newTestTraefik(t)
	if rule, ok := runner.rules(ingress.Project, "traefik"); !ok || rule != traefikRule {
		t.Errorf("Traefik's rule %+v, %v", rule, ok)
	}
	// the stack runner keeps its secrets, though no stack of that name is
	// placed here
	dir := filepath.Join(runner.files.SecretsDir, ingress.Project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.pass(context.Background())
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Traefik's secrets went: %v", err)
	}
}

func TestNoTraefikBeforeTheFirstClusterApply(t *testing.T) {
	traefik, _, fake := newTestTraefik(t)
	cluster := traefik.cluster.(*testCluster)
	cluster.mu.Lock()
	cluster.state.Config.Spec.Ingress.FloatingIP = ""
	cluster.mu.Unlock()
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.count("up "+ingress.Project) != 0 {
		t.Error("Traefik came up without a Floating IP to bind")
	}
}
