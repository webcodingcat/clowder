package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
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
		"GET /v1/internal/recover/copies":                                 controlPlaneHost,
		"POST /v1/internal/recover/rejoin":                                controlPlaneHost,
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

// TestCLIKindsAreTheKindsOfTheCLIsWrites ties the CLI's senders in
// proposers to the routes: a follower forwards a CLI write to the leader
// through internal/propose, which takes only the kinds that proposers
// marks cli. A new write route must name its kind here, and proposers must
// then mark it, or the route works on the leader only.
func TestCLIKindsAreTheKindsOfTheCLIsWrites(t *testing.T) {
	// the kind that each of the CLI's write routes proposes; "" for a route
	// that proposes nothing as the CLI
	writes := map[string]string{
		"POST /v1/hosts/{id}/{action}":                           state.KindSetHostPhase,
		"POST /v1/join-tokens":                                   state.KindAddJoinToken,
		"POST /v1/stacks/{namespace}/{name}/restart":             state.KindRestartStack,
		"POST /v1/stacks/{namespace}/{name}/move":                state.KindRequestMove,
		"POST /v1/apply":                                         "", // internal/changes
		"POST /v1/delete":                                        "", // internal/changes
		"POST /v1/volumes/{namespace}/{name}/resolve-splitbrain": "", // the volume's host commits as itself
		"POST /v1/alerts/test":                                   "", // commits nothing
	}
	kinds := map[string]bool{}
	for _, route := range newTestCluster(t).apis[0].routes() {
		if route.caller != cliCaller || !strings.HasPrefix(route.pattern, "POST ") {
			continue
		}
		kind, ok := writes[route.pattern]
		if !ok {
			t.Errorf("%s: name the kind that this write route proposes", route.pattern)
		}
		if kind != "" {
			kinds[kind] = true
		}
	}
	cliKinds := map[string]bool{}
	for kind, allowed := range proposers {
		if allowed.cli {
			cliKinds[kind] = true
		}
	}
	if !maps.Equal(kinds, cliKinds) {
		t.Errorf("proposers takes %v from the CLI, want the kinds of the CLI's write routes %v",
			slices.Sorted(maps.Keys(cliKinds)), slices.Sorted(maps.Keys(kinds)))
	}
}

// TestEachSenderMayProposeOnlyItsKinds checks mayPropose by the actor: the
// CLI's actor only through a control-plane host, a host only as itself, and
// the cluster's commands only from a control-plane host.
func TestEachSenderMayProposeOnlyItsKinds(t *testing.T) {
	controlPlane := pki.Identity{Role: pki.RoleHost, Name: "host-0", HostID: 0, HostRoles: hostrole.DefaultInit}
	worker := pki.Identity{Role: pki.RoleHost, Name: "host-2", HostID: 2, HostRoles: hostrole.Set{hostrole.Worker}}
	tests := []struct {
		name     string
		identity pki.Identity
		actor    string
		kind     string
		want     bool
	}{
		{"the CLI's host action through the control plane", controlPlane, state.CLIActor("alice"), state.KindSetHostPhase, true},
		{"the CLI's host action through a worker", worker, state.CLIActor("alice"), state.KindSetHostPhase, false},
		{"a host's own phase", worker, state.HostActor(2), state.KindSetHostPhase, true},
		{"another host's phase", worker, state.HostActor(0), state.KindSetHostPhase, false},
		{"a report by the CLI", controlPlane, state.CLIActor("alice"), state.KindReportHost, false},
		{"a restart by a host", controlPlane, state.HostActor(0), state.KindRestartStack, false},
		{"the cutoff by the control plane", controlPlane, state.HostActor(0), state.KindSetCLICutoff, true},
		{"the cutoff by a worker", worker, state.HostActor(2), state.KindSetCLICutoff, false},
		{"a kind the leader writes", controlPlane, state.HostActor(0), state.KindPutVolume, false},
		{"as the leader", controlPlane, state.LeaderActor, state.KindReportHost, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mayPropose(test.identity, state.Command{Kind: test.kind, Actor: test.actor}); got != test.want {
				t.Errorf("mayPropose = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEachRuleAdmitsItsCallers(t *testing.T) {
	server := &Server{config: ServerConfig{HostConfig: HostConfig{HostID: 0}}}
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
