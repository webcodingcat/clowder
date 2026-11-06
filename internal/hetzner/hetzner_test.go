package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI answers like Hetzner's API, with answers shaped as its
// documentation shows them: two pages of Floating IPs, and actions that run
// for one poll before they finish.
type fakeAPI struct {
	t *testing.T

	mu       sync.Mutex
	requests []string // "METHOD path" in order
	bodies   []string
	// actionFails makes every action end in an error.
	actionFails bool
	// missingServers makes GET /v1/servers/404 the API's not found
	missingServers bool
	// manyServers makes the list of servers longer than one page
	manyServers bool
	// labels are server 42's, which a PUT replaces
	labels map[string]string
	// failures are answered first, one per request, before the real ones:
	// an HTTP status and the API's error code
	failures []failure
}

type failure struct {
	status int
	code   string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer secret-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error": {"code": "unauthorized", "message": "unable to authenticate"}}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.bodies = append(f.bodies, string(body))
	w.Header().Set("Content-Type", "application/json")
	if len(f.failures) > 0 {
		next := f.failures[0]
		f.failures = f.failures[1:]
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(time.Now().Unix(), 10))
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, `{"error": {"code": "`+next.code+`", "message": "try later"}}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/floating_ips" && r.URL.Query().Get("page") == "1":
		_, _ = io.WriteString(w, `{"floating_ips": [
			{"id": 4711, "ip": "2001:db8::/64", "server": null},
			{"id": 4712, "ip": "198.51.100.7", "server": null}
		], "meta": {"pagination": {"page": 1, "per_page": 50, "next_page": 2}}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/floating_ips" && r.URL.Query().Get("page") == "2":
		_, _ = io.WriteString(w, `{"floating_ips": [
			{"id": 4713, "ip": "203.0.113.10", "server": 42}
		], "meta": {"pagination": {"page": 2, "per_page": 50, "next_page": null}}}`)
	case f.missingServers && r.Method == http.MethodGet && r.URL.Path == "/v1/servers/404":
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error": {"code": "not_found", "message": "server not found"}}`)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/servers/42":
		var update struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(body, &update); err != nil {
			f.t.Errorf("PUT body %s: %v", body, err)
		}
		f.labels = update.Labels
		f.writeServer(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/servers/42":
		f.writeServer(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/servers" && f.manyServers:
		_, _ = io.WriteString(w, `{"servers": [{"id": 42}], "meta": {"pagination": {"page": 1, "next_page": 2}}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/servers":
		_, _ = io.WriteString(w, `{"servers": [{"id": 42, "name": "node0"}, {"id": 43, "name": "node1"}],
			"meta": {"pagination": {"page": 1, "per_page": 50, "next_page": null}}}`)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/actions/"):
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"action": {"id": 13, "command": "assign_floating_ip", "status": "running", "error": null}}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/actions/13":
		if f.actionFails {
			_, _ = io.WriteString(w, `{"action": {"id": 13, "status": "error",
				"error": {"code": "server_not_found", "message": "server not found"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"action": {"id": 13, "status": "success", "error": null}}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error": {"code": "not_found", "message": "not found"}}`)
	}
}

// writeServer answers with server 42 and its labels.
func (f *fakeAPI) writeServer(w http.ResponseWriter) {
	labels, err := json.Marshal(f.labels)
	if err != nil || f.labels == nil {
		labels = []byte("{}")
	}
	_, _ = io.WriteString(w, `{"server": {"id": 42, "name": "node0", "labels": `+string(labels)+`,
		"public_net": {"ipv4": {"id": 7, "ip": "198.51.100.42", "blocked": false}, "ipv6": null},
		"private_net": [{"network": 4711, "ip": "10.0.0.2", "alias_ips": [], "mac_address": "86:00:00:2a:7d:e0"}]}}`)
}

func newClient(t *testing.T, api *fakeAPI) *Client {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	client := New(server.URL+"/v1/", "secret-token")
	client.poll = time.Millisecond
	client.firstWait, client.lastWait = time.Millisecond, 4*time.Millisecond
	return client
}

func TestARequestIsSentAgainWhileTheAPIIsBusy(t *testing.T) {
	api := &fakeAPI{t: t, failures: []failure{
		{http.StatusTooManyRequests, "rate_limit_exceeded"},
		{http.StatusServiceUnavailable, "unavailable"},
		{http.StatusLocked, "locked"},
		{http.StatusConflict, "locked"},
	}}
	client := newClient(t, api)
	if err := client.PowerOff(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /v1/servers/42/actions/poweroff",
		"POST /v1/servers/42/actions/poweroff",
		"POST /v1/servers/42/actions/poweroff",
		"POST /v1/servers/42/actions/poweroff",
		"POST /v1/servers/42/actions/poweroff",
		"GET /v1/actions/13",
	}
	if strings.Join(api.requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(api.requests, "\n"), strings.Join(want, "\n"))
	}
}

func TestARequestGivesUpAfterItsAttempts(t *testing.T) {
	var failures []failure
	for range attempts {
		failures = append(failures, failure{http.StatusBadGateway, "unavailable"})
	}
	api := &fakeAPI{t: t, failures: failures}
	client := newClient(t, api)
	err := client.PowerOn(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "power server 42 on: try later (unavailable, HTTP 502)") {
		t.Errorf("got %v", err)
	}
	if len(api.requests) != attempts {
		t.Errorf("%d requests, want %d", len(api.requests), attempts)
	}
}

func TestARefusedRequestIsNotSentAgain(t *testing.T) {
	api := &fakeAPI{t: t, failures: []failure{{http.StatusForbidden, "forbidden"}}}
	client := newClient(t, api)
	if err := client.PowerOn(context.Background(), 42); err == nil {
		t.Error("a refusal passed")
	}
	if len(api.requests) != 1 {
		t.Errorf("requests %v", api.requests)
	}
}

func TestTheFloatingIPIsFoundOnAnyPage(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	target, err := client.FloatingIPTarget(context.Background(), netip.MustParseAddr("203.0.113.10"))
	if err != nil || target != 42 {
		t.Errorf("target %d, %v; want 42", target, err)
	}
	target, err = client.FloatingIPTarget(context.Background(), netip.MustParseAddr("198.51.100.7"))
	if err != nil || target != 0 {
		t.Errorf("an unassigned Floating IP: target %d, %v", target, err)
	}
	if _, err := client.FloatingIPTarget(context.Background(), netip.MustParseAddr("192.0.2.1")); err == nil ||
		!strings.Contains(err.Error(), "192.0.2.1") || !errors.Is(err, ErrNoSuchFloatingIP) {
		t.Errorf("an unknown address: %v", err)
	}
}

func TestTheServersAreListedInOneRequest(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	ids, err := client.ServerIDs(context.Background())
	if err != nil || !slices.Equal(ids, []int64{42, 43}) || len(api.requests) != 1 {
		t.Errorf("ids %v, %v, requests %v", ids, err, api.requests)
	}
	api.manyServers = true
	if _, err := client.ServerIDs(context.Background()); err == nil || !strings.Contains(err.Error(), "more than 50") {
		t.Errorf("a second page: %v", err)
	}
}

func TestATokenIsRefusedWhenUnknownOrOfAnotherProject(t *testing.T) {
	api := &fakeAPI{t: t, missingServers: true}
	wrong := New(newClient(t, api).api, "another-token")
	_, err := wrong.ServerIDs(context.Background())
	if !TokenRefused(err) {
		t.Errorf("an unknown token: %v", err)
	}
	client := newClient(t, api)
	_, err = client.RetiredSerial(context.Background(), 404)
	if !TokenRefused(err) {
		t.Errorf("a server of another project: %v", err)
	}
	if TokenRefused(errors.New("connection refused")) || TokenRefused(nil) {
		t.Error("a network error refuses no token")
	}
}

func TestAssigningWaitsForTheAction(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	if err := client.AssignFloatingIP(context.Background(), netip.MustParseAddr("198.51.100.7"), 43); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/floating_ips?per_page=50&page=1",
		"POST /v1/floating_ips/4712/actions/assign",
		"GET /v1/actions/13",
	}
	if strings.Join(api.requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(api.requests, "\n"), strings.Join(want, "\n"))
	}
	if api.bodies[1] != `{"server":43}` {
		t.Errorf("assign body %s", api.bodies[1])
	}
}

func TestAServerIsTheHostsWithItsPrivateAndPublicAddress(t *testing.T) {
	api := &fakeAPI{t: t, missingServers: true}
	client := newClient(t, api)
	for _, test := range []struct {
		what               string
		serverID           int64
		underlay, public   string
		wantOtherServerErr bool
	}{
		{"the host's server", 42, "10.0.0.2", "198.51.100.42", false},
		{"another underlay", 42, "10.0.0.3", "198.51.100.42", true},
		{"the public address as the underlay", 42, "198.51.100.42", "198.51.100.42", true},
		{"another public address", 42, "10.0.0.2", "198.51.100.43", true},
		{"a server the project does not have", 404, "10.0.0.2", "198.51.100.42", true},
	} {
		err := client.CheckServer(context.Background(), test.serverID,
			netip.MustParseAddr(test.underlay), netip.MustParseAddr(test.public))
		if errors.Is(err, ErrOtherServer) != test.wantOtherServerErr || (err != nil && !test.wantOtherServerErr) {
			t.Errorf("%s: %v", test.what, err)
		}
	}
}

func TestARetiredServerKeepsItsOtherLabels(t *testing.T) {
	api := &fakeAPI{t: t, labels: map[string]string{"role": "web"}}
	client := newClient(t, api)
	ctx := context.Background()
	if serial, err := client.RetiredSerial(ctx, 42); err != nil || serial != "" {
		t.Fatalf("retired before: %q, %v", serial, err)
	}
	for range 2 {
		if err := client.SetRetired(ctx, 42, "a1b2"); err != nil {
			t.Fatal(err)
		}
	}
	if serial, err := client.RetiredSerial(ctx, 42); err != nil || serial != "a1b2" {
		t.Errorf("retired after: %q, %v", serial, err)
	}
	if want := map[string]string{"role": "web", RetiredLabel: "a1b2"}; !maps.Equal(api.labels, want) {
		t.Errorf("labels %v, want %v", api.labels, want)
	}
	// the second SetRetired found the label and changed nothing
	if puts := slices.DeleteFunc(slices.Clone(api.requests), func(r string) bool { return !strings.HasPrefix(r, "PUT") }); len(puts) != 1 {
		t.Errorf("PUT requests %v, want one", puts)
	}
}

func TestAServerTheProjectDoesNotHaveIsNoSuchServer(t *testing.T) {
	api := &fakeAPI{t: t, missingServers: true}
	client := newClient(t, api)
	for _, serverID := range []int64{404, 0} {
		if _, err := client.RetiredSerial(context.Background(), serverID); !errors.Is(err, ErrNoSuchServer) {
			t.Errorf("server %d: %v", serverID, err)
		}
	}
}

func TestAFailedActionSaysWhy(t *testing.T) {
	api := &fakeAPI{t: t, actionFails: true}
	client := newClient(t, api)
	err := client.AssignFloatingIP(context.Background(), netip.MustParseAddr("198.51.100.7"), 42)
	if err == nil || !strings.Contains(err.Error(), "assign the Floating IP 198.51.100.7 to server 42: server not found (server_not_found)") {
		t.Errorf("got %v", err)
	}
}

func TestAnAPIErrorCarriesItsMessageNotTheToken(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	client.token = "wrong-token"
	_, err := client.FloatingIPTarget(context.Background(), netip.MustParseAddr("203.0.113.10"))
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate (unauthorized, HTTP 401)") {
		t.Errorf("got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Errorf("the error carries the token: %v", err)
	}
}

func TestNoServerHasAnIDBelowOne(t *testing.T) {
	client := New("", "secret-token")
	if client.api != DefaultAPI {
		t.Errorf("api %s", client.api)
	}
	for _, serverID := range []int64{0, -1} {
		if err := client.AssignFloatingIP(context.Background(), netip.MustParseAddr("198.51.100.7"), serverID); !errors.Is(err, ErrNoSuchServer) {
			t.Errorf("server %d: %v", serverID, err)
		}
	}
}

func TestAnUnreadAnswerIsReadToTheEnd(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"server": {"id": 42, "labels": {}}, "padding": "`+strings.Repeat("x", 8<<10)+`"}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	client := New(server.URL, "secret-token")
	for range 3 {
		if err := client.SetRetired(context.Background(), 42, "a1b2"); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Errorf("%d connections, want one for all requests", got)
	}
}
