package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		files: runner.files, host: runner.host, cluster: cluster, docker: fake, open: runner.open,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), supervisor: runner.supervisor,
		wake: make(chan struct{}, 1), unopened: map[string]bool{},
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

func TestACertificateBringsHTTPSAndARenewalComesUnderNewNames(t *testing.T) {
	traefik, _, _ := newTestTraefik(t)
	cluster := traefik.cluster.(*testCluster)
	put := func(chain string) {
		cluster.apply(state.KindPutCertificate, state.LeaderActor, state.PutCertificate{Certificate: state.Certificate{
			Domain: "shop.example.com", Key: state.Sealed("key of " + chain), Chain: []byte(chain),
			NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		}})
	}
	put("first chain")
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := certificateFiles(t, traefik)
	routes, err := os.ReadFile(filepath.Join(traefik.files.traefikDynamicDir(), "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !strings.HasPrefix(first[0], "shop.example.com-") || !strings.HasSuffix(first[0], ".crt") {
		t.Fatalf("certificate files %v", first)
	}
	for _, want := range []string{"websecure.shop.app", "redirect-to-https", first[0], first[1]} {
		if !strings.Contains(string(routes), want) {
			t.Errorf("no %s in the routes:\n%s", want, routes)
		}
	}
	key, err := os.ReadFile(filepath.Join(traefik.files.traefikCertificatesDir(), first[1]))
	if err != nil || string(key) != "key of first chain" {
		t.Errorf("the key: %q, %v", key, err)
	}

	// Traefik watches only the routes: a renewed certificate comes under
	// new names, and the old files go
	put("second chain")
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second := certificateFiles(t, traefik); len(second) != 2 || second[0] == first[0] {
		t.Errorf("certificate files after the renewal %v", second)
	}

	cluster.apply(state.KindDeleteCertificate, state.LeaderActor, state.DeleteCertificate{Domain: "shop.example.com"})
	if err := traefik.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if files := certificateFiles(t, traefik); len(files) != 0 {
		t.Errorf("files of a deleted certificate stayed: %v", files)
	}
}

// certificateFiles lists the certificates' directory, sorted.
func certificateFiles(t *testing.T, traefik *traefikRunner) []string {
	t.Helper()
	entries, err := os.ReadDir(traefik.files.traefikCertificatesDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestEveryAgentAnswersTheChallengesOfTheState(t *testing.T) {
	_, cluster, _, _ := newTestRunner(t)
	cluster.apply(state.KindPutACMEChallenge, state.LeaderActor, state.PutACMEChallenge{
		Token: "abc", KeyAuthorization: "abc.thumb",
	})
	handler := challengeHandler(cluster)
	for path, want := range map[string]int{
		ingress.ChallengePath + "abc":   http.StatusOK,
		ingress.ChallengePath + "other": http.StatusNotFound,
		"/other/abc":                    http.StatusNotFound,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if recorder.Code != want || (want == http.StatusOK && recorder.Body.String() != "abc.thumb") {
			t.Errorf("%s: %d %q", path, recorder.Code, recorder.Body)
		}
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
