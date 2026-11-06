package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

func TestTheCLIReadsTheStatusFromAnyAgent(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	var status Status
	if err := client.Get(context.Background(), "/v1/status", &status); err != nil {
		t.Fatal(err)
	}
	if status.Leader != "node0" || len(status.Hosts) != 2 || status.Hosts[1].Phase != "joining" {
		t.Errorf("status %+v", status)
	}
}

func TestAFollowerForwardsWritesToTheLeader(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	ctx := context.Background()

	// node0 is active: draining it is allowed
	written, err := client.Post(ctx, "/v1/hosts/0/drain", nil)
	if err != nil {
		t.Fatal(err)
	}
	var phase state.HostPhase
	var actor string
	cluster.clusters[0].Read(func(current *state.State) {
		phase = current.Hosts[0].Phase
		actor = current.Audit[len(current.Audit)-1].Actor
	})
	// the actor names the CLI certificate, also on the leader it was
	// forwarded to
	if phase != state.Draining || actor != "cli/alice" {
		t.Errorf("leader's host 0 is %s, last actor %s", phase, actor)
	}

	// the follower has not caught up: the next read waits for the write
	go func() {
		time.Sleep(100 * time.Millisecond)
		cluster.clusters[1].setIndex(written.Index)
	}()
	var hosts []HostInfo
	if err := client.Get(ctx, "/v1/hosts", &hosts); err != nil {
		t.Fatal(err)
	}
	cluster.clusters[1].Read(func(current *state.State) {
		if current.Index < written.Index {
			t.Errorf("the read did not wait for index %d", written.Index)
		}
	})

	// a refusal of the state comes back as such
	// the API checks the phase a user action starts from
	_, err = client.Post(ctx, "/v1/hosts/1/undrain", nil)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "undrain works on a host that is draining") {
		t.Errorf("want 409 for undraining a joining host, got %v", err)
	}
	// the leader's state refuses what the API lets through: removing twice
	if _, err := client.Post(ctx, "/v1/hosts/1/remove", nil); err != nil {
		t.Fatal(err)
	}
	_, err = client.Post(ctx, "/v1/hosts/1/remove", nil)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "node1 is removed") {
		t.Errorf("want 409 with the state's reason, got %v", err)
	}
	if _, err := client.Post(ctx, "/v1/hosts/1/explode", nil); statusOf(err) != http.StatusNotFound {
		t.Errorf("want 404 for an unknown action, got %v", err)
	}
}

func TestARemovedLeaderHandsItsLeadOverFirst(t *testing.T) {
	cluster := newTestCluster(t)
	leader := cluster.clusters[0]
	leader.mu.Lock()
	leader.state.Hosts[1].Phase = state.Active
	leader.mu.Unlock()
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	// host 1 is not the leader: no hand-over
	if _, err := client.Post(context.Background(), "/v1/hosts/1/drain", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Post(context.Background(), "/v1/hosts/0/remove", nil); err != nil {
		t.Fatal(err)
	}
	leader.Read(func(current *state.State) {
		if current.Hosts[0].Phase != state.Removed || leader.handOvers != 1 {
			t.Errorf("host 0 is %s after %d hand-overs", current.Hosts[0].Phase, leader.handOvers)
		}
	})
}

func TestARefusedRemovalMovesNoLead(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	if _, err := client.Post(context.Background(), "/v1/hosts/0/remove", nil); statusOf(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "the last control-plane host in service") {
		t.Errorf("removing the last host: %v", err)
	}
	cluster.clusters[0].Read(func(*state.State) {
		if cluster.clusters[0].handOvers != 0 {
			t.Error("the lead moved for a removal that was refused")
		}
	})
}

// A follower that takes the removal of the leader asks the leader to hand
// over (askLeaderToHandOver).
func TestALeaderHandsOverWhenAskedOnlyIfItsRemovalWouldGo(t *testing.T) {
	cluster := newTestCluster(t)
	leader := cluster.clusters[0]
	ctx := context.Background()
	err := cluster.apis[1].askLeaderToHandOver(ctx, 0)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "the last control-plane host in service") {
		t.Errorf("the last control-plane host was asked: %v", err)
	}
	leader.mu.Lock()
	leader.state.Hosts[1].Phase = state.Active
	leader.mu.Unlock()
	if err := cluster.apis[1].askLeaderToHandOver(ctx, 0); err != nil {
		t.Errorf("a leader that may go was asked: %v", err)
	}
	// a member that does not lead has no lead to hand over
	if err := cluster.apis[0].askLeaderToHandOver(ctx, 1); err != nil {
		t.Errorf("a follower was asked: %v", err)
	}
	leader.Read(func(*state.State) {
		if leader.handOvers != 1 {
			t.Errorf("%d hand-overs, want 1", leader.handOvers)
		}
	})
}

func TestTheAuditShowsTheLastEntries(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	if _, err := client.Post(ctx, "/v1/hosts/0/drain", nil); err != nil {
		t.Fatal(err)
	}
	var entries []AuditEntry
	if err := client.Get(ctx, "/v1/audit?limit=1", &entries); err != nil || len(entries) != 1 ||
		entries[0].Actor != "cli/alice" || entries[0].Kind != state.KindSetHostPhase {
		t.Errorf("entries %+v, %v", entries, err)
	}
	for _, limit := range []string{"0", "many"} {
		if err := client.Get(ctx, "/v1/audit?limit="+limit, &entries); statusOf(err) != http.StatusBadRequest {
			t.Errorf("limit %s: want HTTP 400, got %v", limit, err)
		}
	}
}

// fakeAlerts answers a test with results and keeps failures.
type fakeAlerts struct {
	results  []AlertTestResult
	failures []AlertFailure
}

func (f fakeAlerts) Test(context.Context) ([]AlertTestResult, error) { return f.results, nil }

func (f fakeAlerts) Failures() []AlertFailure { return f.failures }

func TestAnAlertTestNeedsAChannel(t *testing.T) {
	for _, test := range []struct {
		results []AlertTestResult
		want    int
	}{
		{nil, http.StatusConflict},
		{[]AlertTestResult{{Channel: "discord"}}, http.StatusOK},
	} {
		server := &Server{config: ServerConfig{Alerts: fakeAlerts{results: test.results}}}
		recorder := httptest.NewRecorder()
		server.alertTest(recorder, httptest.NewRequest(http.MethodPost, "/v1/alerts/test", nil))
		if recorder.Code != test.want {
			t.Errorf("%d results: HTTP %d, want %d: %s", len(test.results), recorder.Code, test.want, recorder.Body)
		}
	}
}

func TestTheLeaderAnswersWithTheAlertsFailures(t *testing.T) {
	cluster := newFakeCluster(t, true)
	failures := []AlertFailure{{Channel: "discord", Key: "pool-use/node0", Attempts: 3}}
	server := &Server{config: ServerConfig{HostConfig: HostConfig{Cluster: cluster}, Alerts: fakeAlerts{failures: failures}}}
	recorder := httptest.NewRecorder()
	server.alertFailures(recorder, httptest.NewRequest(http.MethodGet, "/v1/alerts/failures", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"attempts":3`) {
		t.Errorf("the leader's failures: HTTP %d, %s", recorder.Code, recorder.Body)
	}
	cluster.mu.Lock()
	cluster.noLeader = true
	cluster.mu.Unlock()
	recorder = httptest.NewRecorder()
	server.alertFailures(recorder, httptest.NewRequest(http.MethodGet, "/v1/alerts/failures", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("without a leader: HTTP %d, %s", recorder.Code, recorder.Body)
	}
}

func TestTheHostsShowTheirAddresses(t *testing.T) {
	current := &state.State{Hosts: map[state.HostID]*state.Host{
		0: {ID: 0, Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"), PublicAddress: netip.MustParseAddr("203.0.113.10")},
	}}
	hosts := hostInfos(current)
	if hosts[0].Underlay != "10.0.0.2" || hosts[0].PublicAddress != "203.0.113.10" {
		t.Errorf("hosts %+v", hosts)
	}
}

func TestARequestOverTheLimitIsNamedAsSuch(t *testing.T) {
	// a small limit in place of MaxRequestBytes (ServeHTTP's): reading 16 MiB
	// takes seconds with the race detector
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(`{"resources": []}`))
	request.Body = http.MaxBytesReader(recorder, request.Body, 4)
	var decoded ApplyRequest
	writeError(recorder, http.StatusBadRequest, decodeStrict(request.Body, &decoded))
	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), "larger than 16777216 bytes") {
		t.Errorf("answer %d: %s", recorder.Code, recorder.Body)
	}
}
