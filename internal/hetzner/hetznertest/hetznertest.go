// Package hetznertest is a fake of Hetzner Cloud's API, whose power actions
// finish at once. A test puts its URL into the configuration's hetzner.api,
// so the real client talks to the fake.
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
	// changes is what Changes returns
	changes []string
	// failing counts, per server, how many more actions will end in an
	// error
	failing map[int64]int
	// held holds, per server, a channel that its power actions wait for
	// (HoldActions)
	held map[int64]chan struct{}
}

type server struct {
	private, public netip.Addr
	labels          map[string]string
}

// New starts the fake for the test. Each request must carry the token.
func New(t testing.TB, token string) *API {
	api := &API{
		token: token, servers: map[int64]*server{}, floatingIPs: map[netip.Addr]int64{}, failing: map[int64]int{},
		held: map[int64]chan struct{}{},
	}
	httpServer := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(httpServer.Close)
	api.URL = httpServer.URL
	return api
}

// AddServer adds a server with its address in the private network and its
// public address.
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

// FloatingIPTarget returns the server that the Floating IP is routed to, or
// 0 for none.
func (a *API) FloatingIPTarget(ip netip.Addr) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.floatingIPs[ip]
}

// FailActions makes the next count actions on the server end in an error,
// as when Hetzner cannot carry out an action. The client does not send such
// an action again.
func (a *API) FailActions(serverID int64, count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing[serverID] = count
}

// HoldActions makes the server's power actions wait, as when Hetzner does
// not answer, until release is called or the client gives up. The action
// is in Changes while it waits.
func (a *API) HoldActions(serverID int64) (release func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := make(chan struct{})
	a.held[serverID] = held
	return sync.OnceFunc(func() {
		a.mu.Lock()
		delete(a.held, serverID)
		a.mu.Unlock()
		close(held)
	})
}

// SetLabel sets a label on the server.
func (a *API) SetLabel(serverID int64, key, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.servers[serverID].labels[key] = value
}

// Labels returns the server's labels.
func (a *API) Labels(serverID int64) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.servers[serverID].labels)
}

// Changes returns what the requests changed so far, in order: "poweroff
// 41", "poweron 41", "assign 203.0.113.10 41", "label 41
// clowder-retired=s1". Failed actions are included.
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
	case r.Method == http.MethodGet && r.URL.Path == "/servers":
		a.writeServers(w)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "floating_ips" && parts[3] == "assign":
		a.assign(w, r, parts[1])
	case len(parts) >= 2 && parts[0] == "servers":
		a.serveServer(w, r, parts)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// writeFloatingIPs gives each Floating IP its position as its ID, which
// assign relies on.
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

func (a *API) writeServers(w http.ResponseWriter) {
	list := []map[string]int64{}
	for _, id := range slices.Sorted(maps.Keys(a.servers)) {
		list = append(list, map[string]int64{"id": id})
	}
	writeJSON(w, map[string]any{"servers": list, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
}

func (a *API) sortedFloatingIPs() []netip.Addr {
	return slices.SortedFunc(maps.Keys(a.floatingIPs), netip.Addr.Compare)
}

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
		if held := a.held[id]; held != nil {
			a.mu.Unlock() // serve holds the lock; other requests go on meanwhile
			select {
			case <-held:
			case <-r.Context().Done():
			}
			a.mu.Lock()
		}
		if a.actionFails(id) {
			writeFailedAction(w)
			return
		}
		writeJSON(w, map[string]any{"action": map[string]any{"id": 1, "status": "success"}})
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// actionFails also counts the failure down.
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
