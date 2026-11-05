// Package hetznertest is a fake of Hetzner Cloud's API for the tests of the
// packages that use internal/hetzner: servers with their addresses and
// labels, Floating IPs, and power actions that finish at once. A test names
// it in cluster.yaml's hetzner.api, so the real client talks to it.
package hetznertest

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// API is the fake API of one project. Its methods are safe to call while
// a client sends requests.
type API struct {
	// URL is the API's address, for hetzner.api.
	URL string

	token string

	mu          sync.Mutex
	servers     map[int64]*server
	floatingIPs map[netip.Addr]int64 // the server each is routed to, 0 for none
	// changes are what the requests changed, in order: "poweroff 41",
	// "poweron 41", "assign 203.0.113.10 41", "label 41 clowder-retired=s1"
	changes []string
	// failing counts the actions on a server that still end in an error
	failing map[int64]int
}

type server struct {
	private, public netip.Addr
	labels          map[string]string
}

// New starts the fake for the test; each request must carry the token.
func New(t testing.TB, token string) *API {
	api := &API{token: token, servers: map[int64]*server{}, floatingIPs: map[netip.Addr]int64{}, failing: map[int64]int{}}
	httpServer := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(httpServer.Close)
	api.URL = httpServer.URL
	return api
}

// AddServer adds a server with its address in the private network and its
// public one.
func (a *API) AddServer(id int64, private, public netip.Addr) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.servers[id] = &server{private: private, public: public, labels: map[string]string{}}
}

// RouteFloatingIP routes the Floating IP to the server (0 for none), as a
// user would by hand; it adds the Floating IP to the project if needed.
func (a *API) RouteFloatingIP(ip netip.Addr, serverID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.floatingIPs[ip] = serverID
}

// FloatingIPTarget is the server the Floating IP is routed to, 0 for none.
func (a *API) FloatingIPTarget(ip netip.Addr) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.floatingIPs[ip]
}

// FailActions makes the next count actions on the server end in an error,
// as an action Hetzner could not carry out does: the client does not send
// it again.
func (a *API) FailActions(serverID int64, count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing[serverID] = count
}

// SetLabel sets a label on the server.
func (a *API) SetLabel(serverID int64, key, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.servers[serverID].labels[key] = value
}

// Labels are the server's labels.
func (a *API) Labels(serverID int64) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.servers[serverID].labels)
}

// Changes are what the requests changed so far, in order: "poweroff 41",
// "poweron 41", "assign 203.0.113.10 41", "label 41 clowder-retired=s1".
// Failed actions count too.
func (a *API) Changes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.changes)
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/floating_ips":
		a.writeFloatingIPs(w)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "floating_ips" && parts[3] == "assign":
		a.assign(w, r, parts[1])
	case len(parts) >= 2 && parts[0] == "servers":
		a.serveServer(w, r, parts)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// writeFloatingIPs lists the Floating IPs, sorted by address, on one page;
// a Floating IP's ID is its place in the list.
func (a *API) writeFloatingIPs(w http.ResponseWriter) {
	type floatingIP struct {
		ID     int64  `json:"id"`
		IP     string `json:"ip"`
		Server *int64 `json:"server"`
	}
	list := []floatingIP{}
	for index, ip := range a.sortedFloatingIPs() {
		entry := floatingIP{ID: int64(index + 1), IP: ip.String()}
		if target := a.floatingIPs[ip]; target != 0 {
			entry.Server = &target
		}
		list = append(list, entry)
	}
	writeJSON(w, map[string]any{"floating_ips": list, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
}

func (a *API) sortedFloatingIPs() []netip.Addr {
	return slices.SortedFunc(maps.Keys(a.floatingIPs), netip.Addr.Compare)
}

// assign routes the Floating IP with the ID to the server in the body.
func (a *API) assign(w http.ResponseWriter, r *http.Request, idText string) {
	var body struct {
		Server int64 `json:"server"`
	}
	index, err := strconv.Atoi(idText)
	ips := a.sortedFloatingIPs()
	if err != nil || index < 1 || index > len(ips) || json.NewDecoder(r.Body).Decode(&body) != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	ip := ips[index-1]
	a.changes = append(a.changes, fmt.Sprintf("assign %s %d", ip, body.Server))
	if a.actionFails(body.Server) {
		writeFailedAction(w)
		return
	}
	a.floatingIPs[ip] = body.Server
	writeJSON(w, map[string]any{"action": map[string]any{"id": 1, "status": "success"}})
}

// serveServer answers /servers/<id> (GET, PUT of the labels) and its power
// actions.
func (a *API) serveServer(w http.ResponseWriter, r *http.Request, parts []string) {
	id, err := strconv.ParseInt(parts[1], 10, 64)
	found, ok := a.servers[id]
	if err != nil || !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		a.writeServer(w, id, found)
	case len(parts) == 2 && r.Method == http.MethodPut:
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input")
			return
		}
		for _, key := range slices.Sorted(maps.Keys(body.Labels)) {
			if found.labels[key] != body.Labels[key] {
				a.changes = append(a.changes, fmt.Sprintf("label %d %s=%s", id, key, body.Labels[key]))
			}
		}
		found.labels = body.Labels
		a.writeServer(w, id, found)
	case len(parts) == 4 && r.Method == http.MethodPost && (parts[3] == "poweroff" || parts[3] == "poweron"):
		a.changes = append(a.changes, fmt.Sprintf("%s %d", parts[3], id))
		if a.actionFails(id) {
			writeFailedAction(w)
			return
		}
		writeJSON(w, map[string]any{"action": map[string]any{"id": 1, "status": "success"}})
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// actionFails tells whether the next action on the server fails, and
// counts it.
func (a *API) actionFails(serverID int64) bool {
	if a.failing[serverID] <= 0 {
		return false
	}
	a.failing[serverID]--
	return true
}

func (a *API) writeServer(w http.ResponseWriter, id int64, found *server) {
	answer := map[string]any{"id": id, "labels": found.labels, "public_net": map[string]any{"ipv4": nil}}
	if found.public.IsValid() {
		answer["public_net"] = map[string]any{"ipv4": map[string]any{"ip": found.public.String()}}
	}
	private := []map[string]any{}
	if found.private.IsValid() {
		private = append(private, map[string]any{"network": 1, "ip": found.private.String()})
	}
	answer["private_net"] = private
	writeJSON(w, map[string]any{"server": answer})
}

func writeFailedAction(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"action": map[string]any{
		"id": 1, "status": "error", "error": map[string]any{"code": "action_failed", "message": "the fake failed it"},
	}})
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error": {"code": "`+code+`", "message": "`+code+`"}}`)
}

func writeJSON(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(value) // the client reports a broken answer
}
