package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// TestEachRouteTakesItsCallers holds the route table a second time, so
// that a change to who may call a route shows up here, and a new route
// cannot come without its callers.
func TestEachRouteTakesItsCallers(t *testing.T) {
	want := map[string]caller{
		"GET /v1/status":                                                  cliCaller,
		"GET /v1/hosts":                                                   cliCaller,
		"POST /v1/hosts/{id}/{action}":                                    cliCaller,
		"POST /v1/join-tokens":                                            cliCaller,
		"GET /v1/audit":                                                   cliCaller,
		"GET /v1/sealing-key":                                             cliCaller,
		"POST /v1/apply":                                                  cliCaller,
		"POST /v1/delete":                                                 cliCaller,
		"GET /v1/resources/{kind}":                                        cliCaller,
		"GET /v1/resources/{kind}/{namespace}/{name}":                     cliCaller,
		"GET /v1/resources/{kind}/{name}":                                 cliCaller,
		"GET /v1/watch":                                                   cliCaller,
		"GET /v1/stacks/{namespace}/{name}/ps":                            cliCaller,
		"GET /v1/stacks/{namespace}/{name}/logs":                          cliCaller,
		"POST /v1/stacks/{namespace}/{name}/restart":                      cliCaller,
		"POST /v1/stacks/{namespace}/{name}/move":                         cliCaller,
		"POST /v1/volumes/{namespace}/{name}/resolve-splitbrain":          cliCaller,
		"POST /v1/alerts/test":                                            cliCaller,
		"GET /v1/alerts/failures":                                         cliCaller,
		"POST /v1/internal/propose":                                       anyHost,
		"POST /v1/internal/beats":                                         anyHost,
		"GET /v1/internal/beats/{id}":                                     anyHost,
		"GET /v1/internal/commit-index":                                   anyHost,
		"GET /v1/internal/leader":                                         anyHost,
		"GET /v1/internal/hosts/self/secrets":                             anyHost,
		"GET /v1/internal/hosts/self/tls":                                 anyHost,
		"POST /v1/internal/changes":                                       controlPlaneHost,
		"POST /v1/internal/join":                                          controlPlaneHost,
		"POST /v1/internal/hand-over":                                     controlPlaneHost,
		"GET /v1/internal/alerts/failures":                                controlPlaneHost,
		"GET /v1/internal/stacks/{project}/ps":                            controlPlaneHost,
		"GET /v1/internal/stacks/{project}/logs":                          controlPlaneHost,
		"GET /v1/internal/volumes/progress":                               controlPlaneHost,
		"POST /v1/internal/volumes/{namespace}/{name}/resolve-splitbrain": controlPlaneHost,
		"POST /v1/internal/credentials":                                   thisHost,
		"GET /v1/internal/sealing-key":                                    thisHost,
	}
	routes := newTestCluster(t).apis[0].routes()
	for _, route := range routes {
		rule, ok := want[route.pattern]
		switch {
		case !ok:
			t.Errorf("%s: a route without callers in this test", route.pattern)
		case rule != route.caller:
			t.Errorf("%s: for %s, want %s", route.pattern, route.caller, rule)
		}
	}
	if len(routes) != len(want) {
		t.Errorf("%d routes, want %d", len(routes), len(want))
	}
}

func TestEachRuleAdmitsItsCallers(t *testing.T) {
	server := &Server{config: ServerConfig{HostID: 0}}
	callers := map[string]pki.Identity{
		"the CLI":              {Role: pki.RoleCLI, Name: "alice"},
		"this host":            {Role: pki.RoleHost, Name: "host-0", HostID: 0, HostRoles: hostrole.DefaultInit},
		"a control-plane host": {Role: pki.RoleHost, Name: "host-1", HostID: 1, HostRoles: hostrole.DefaultInit},
		"a worker":             {Role: pki.RoleHost, Name: "host-2", HostID: 2, HostRoles: hostrole.DefaultJoin},
	}
	admitted := map[caller][]string{
		caller(-1):       nil, // an unknown rule admits nobody
		cliCaller:        {"the CLI"},
		anyHost:          {"this host", "a control-plane host", "a worker"},
		controlPlaneHost: {"this host", "a control-plane host"},
		thisHost:         {"this host"},
	}
	for rule, names := range admitted {
		for name, identity := range callers {
			want := false
			for _, admittedName := range names {
				want = want || admittedName == name
			}
			if got := server.admits(rule, identity); got != want {
				t.Errorf("%s admits %s: %v, want %v", rule, name, got, want)
			}
		}
	}
}

func TestAControlPlaneHostRefusesCallersOutsideTheRouteTable(t *testing.T) {
	cluster := newTestCluster(t)
	ctx := context.Background()
	worker := cluster.client(cluster.hostCertificate(workerHost), testVersion, 0)
	if err := worker.Get(ctx, "/v1/internal/volumes/progress", &map[string]int64{}); statusOf(err) != http.StatusForbidden {
		t.Errorf("a worker asked a control-plane route: want 403, got %v", err)
	}
	// root on host 1 asks host 0's agent, which is not its own
	other := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	if err := other.Get(ctx, "/v1/internal/sealing-key", &SealingKeyAnswer{}); statusOf(err) != http.StatusForbidden {
		t.Errorf("another host asked for the sealing key: want 403, got %v", err)
	}
	if err := worker.Act(ctx, "/v1/internal/beats", nil, nil); err != nil {
		t.Errorf("a worker's beat: %v", err)
	}
}

func TestAWorkerTakesOnlyTheControlPlane(t *testing.T) {
	cluster := newTestCluster(t)
	address := "https://" + cluster.endpoint(int(workerHost))
	ask := func(certificate tls.Certificate, path string) (int, error) {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: pki.ClientTLSToHosts(certificate, cluster.ca.Certificate, nil),
		}}
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, address+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(VersionHeader, testVersion)
		response, err := client.Do(request)
		if err != nil {
			return 0, err
		}
		_ = response.Body.Close()
		return response.StatusCode, nil
	}
	if status, err := ask(cluster.hostCertificate(0), "/v1/internal/volumes/progress"); err != nil || status != http.StatusOK {
		t.Errorf("the control plane asked the worker: HTTP %d, %v", status, err)
	}
	for name, certificate := range map[string]tls.Certificate{
		"the CLI":                         cluster.cliCertificate(),
		"the worker itself":               cluster.hostCertificate(workerHost),
		"a joining host (no certificate)": {},
	} {
		if status, err := ask(certificate, "/v1/status"); err == nil {
			t.Errorf("%s got through the handshake: HTTP %d", name, status)
		}
	}
	// the CLI's own client does not even take the worker as an endpoint
	err := cluster.client(cluster.cliCertificate(), testVersion, int(workerHost)).Get(context.Background(), "/v1/status", &Status{})
	if err == nil || statusOf(err) != 0 {
		t.Errorf("the CLI took a worker: %v", err)
	}
}

func TestOnlyControlPlaneHostsAreTheCLIsEndpoints(t *testing.T) {
	cluster := newTestCluster(t)
	cluster.withRealCA()
	cluster.clusters[0].Read(func(current *state.State) {
		current.Hosts[workerHost] = &state.Host{
			ID: workerHost, Name: "node2", Roles: rolesOf(workerHost),
			PublicAddress: netip.MustParseAddr("203.0.113.4"), Phase: state.Active,
		}
	})
	host := cluster.client(cluster.hostCertificate(0), testVersion, 0)
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	var answer CredentialsAnswer
	if err := host.Act(context.Background(), "/v1/internal/credentials",
		CredentialsRequest{PublicKey: key.Public().(ed25519.PublicKey)}, &answer); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range answer.Endpoints {
		if endpoint == "203.0.113.4" {
			t.Errorf("the worker is among the endpoints %v", answer.Endpoints)
		}
	}
}
