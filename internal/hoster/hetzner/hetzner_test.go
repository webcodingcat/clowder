package hetzner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
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

func newClient(t *testing.T, api *fakeAPI) *Client {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	client := New(server.URL+"/v1/", "secret-token")
	client.poll = time.Millisecond
	return client
}

func TestTheFloatingIPIsFoundOnAnyPage(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	target, err := client.FloatingIPTarget(context.Background(), netip.MustParseAddr("203.0.113.10"))
	if err != nil || target != "42" {
		t.Errorf("target %q, %v; want 42", target, err)
	}
	target, err = client.FloatingIPTarget(context.Background(), netip.MustParseAddr("198.51.100.7"))
	if err != nil || target != "" {
		t.Errorf("an unassigned Floating IP: target %q, %v", target, err)
	}
	if _, err := client.FloatingIPTarget(context.Background(), netip.MustParseAddr("192.0.2.1")); err == nil ||
		!strings.Contains(err.Error(), "no Floating IP 192.0.2.1") {
		t.Errorf("an unknown address: %v", err)
	}
}

func TestAssigningWaitsForTheAction(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	if err := client.AssignFloatingIP(context.Background(), netip.MustParseAddr("198.51.100.7"), "43"); err != nil {
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

func TestPowerActionsNameTheServer(t *testing.T) {
	api := &fakeAPI{t: t}
	client := newClient(t, api)
	if err := client.PowerOff(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	if err := client.PowerOn(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	if api.requests[0] != "POST /v1/servers/42/actions/poweroff" || api.requests[2] != "POST /v1/servers/42/actions/poweron" {
		t.Errorf("requests %v", api.requests)
	}
}

func TestAFailedActionSaysWhy(t *testing.T) {
	api := &fakeAPI{t: t, actionFails: true}
	client := newClient(t, api)
	err := client.PowerOff(context.Background(), "42")
	if err == nil || !strings.Contains(err.Error(), "power off server 42: server not found (server_not_found)") {
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

func TestAServerRefMustBeAServerID(t *testing.T) {
	client := New("", "secret-token")
	if client.api != DefaultAPI {
		t.Errorf("api %s", client.api)
	}
	for _, ref := range []string{"", "node0", "-1", "0"} {
		if err := client.PowerOn(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "not a Hetzner server ID") {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
}
