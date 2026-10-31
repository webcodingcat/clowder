package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// waitTimeout bounds how long a read waits for its minimum index and a write
// for the leader.
const waitTimeout = 10 * time.Second

// maxRequestBytes bounds every request body: an apply with all the secrets
// a namespace may have (resource.MaxNamespaceSecretBytes) fits with room to
// spare.
const maxRequestBytes = 1 << 20

// Cluster is what the server needs from the agent's Raft member.
type Cluster interface {
	Propose(ctx context.Context, cmd state.Command) (uint64, error)
	Read(use func(current *state.State))
	WaitIndex(ctx context.Context, index uint64) error
	IsLeader() bool
	// Leader returns the leading host; ok is false while there is none.
	Leader() (id state.HostID, ok bool)
}

// ServerConfig is what an agent's API server needs.
type ServerConfig struct {
	Cluster Cluster
	CA      *x509.Certificate
	// Certificate is the host's: it serves TLS and forwards writes.
	Certificate tls.Certificate
	// APIAddress gives a host's API address (its node address and Port).
	APIAddress func(id state.HostID) string
	// Keys seals secrets for the state and opens them (the data key).
	Keys Keys
	// HostID is this agent's host; Containers answers for its stacks.
	HostID     state.HostID
	Containers Containers
	// Alerts answers for the alert channels; the agent's alerts loop is the
	// real one.
	Alerts  Alerts
	Version string
	Logger  *slog.Logger
}

// Alerts is what the API asks of the agent's alerts loop.
type Alerts interface {
	// Test sends a test alert to every channel from this agent.
	Test(ctx context.Context) ([]AlertTestResult, error)
	// Failures are the messages a channel has not taken yet; only the
	// leader has any.
	Failures() []AlertFailure
}

// Keys is the cluster's data key as the server uses it.
type Keys interface {
	Seal(plaintext []byte) (state.Sealed, error)
	Open(sealed state.Sealed) ([]byte, error)
	// Derive gives a key of its own for one purpose (the secrets' digests).
	Derive(purpose string) ([]byte, error)
}

// Server is an agent's API.
type Server struct {
	config ServerConfig
	mux    *http.ServeMux
	// forwarder carries writes to the leader; streamer relays answers of
	// other hosts that may take long (logs that follow), so it has no timeout.
	forwarder *http.Client
	streamer  *http.Client
}

// NewServer sets up the routes.
func NewServer(config ServerConfig) *Server {
	server := &Server{config: config, mux: http.NewServeMux()}
	transport := &http.Transport{TLSClientConfig: pki.ClientTLSToHosts(config.Certificate, config.CA, server.revoked)}
	server.forwarder = &http.Client{Timeout: waitTimeout, Transport: transport}
	server.streamer = &http.Client{Transport: transport}
	server.handle("GET /v1/status", pki.RoleUser, server.status)
	server.handle("GET /v1/hosts", pki.RoleUser, server.hosts)
	server.handle("POST /v1/hosts/{id}/{action}", pki.RoleUser, server.hostAction)
	server.handle("GET /v1/users", pki.RoleUser, server.users)
	server.handle("GET /v1/audit", pki.RoleUser, server.audit)
	server.handle("GET /v1/cluster", pki.RoleUser, server.cluster)
	server.handle("PUT /v1/cluster", pki.RoleUser, server.applyCluster)
	server.handle("POST /v1/apply", pki.RoleUser, server.applyResources)
	server.handle("POST /v1/delete", pki.RoleUser, server.deleteResources)
	server.handle("GET /v1/resources/{kind}", pki.RoleUser, server.listResources)
	server.handle("GET /v1/resources/{kind}/{namespace}/{name}", pki.RoleUser, server.getResource)
	server.handle("GET /v1/resources/namespace/{name}", pki.RoleUser, server.getNamespace)
	server.handle("GET /v1/stacks/{namespace}/{name}/ps", pki.RoleUser, server.stackPs)
	server.handle("GET /v1/stacks/{namespace}/{name}/logs", pki.RoleUser, server.stackLogs)
	server.handle("POST /v1/alerts/test", pki.RoleUser, server.alertTest)
	server.handle("GET /v1/alerts/failures", pki.RoleUser, server.alertFailures)
	server.handle("GET /v1/internal/alerts/failures", pki.RoleHost, server.localAlertFailures)
	server.handle("POST /v1/internal/propose", pki.RoleHost, server.internalPropose)
	server.handle("POST /v1/internal/changes", pki.RoleHost, server.internalChanges)
	server.handle("GET /v1/internal/stacks/{project}/ps", pki.RoleHost, server.localPs)
	server.handle("GET /v1/internal/stacks/{project}/logs", pki.RoleHost, server.localLogs)
	server.handleOpen("GET /v1/bootstrap", server.bootstrapOffer)
	server.handleOpen("POST /v1/bootstrap", server.bootstrap)
	return server
}

// TLSConfig is the server's side of mutual TLS. A client certificate is
// checked when given; the routes refuse calls without one. (Bootstrap and
// join will come without one and prove themselves with the bootstrap key.)
func (s *Server) TLSConfig() *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(s.config.CA)
	return &tls.Config{
		Certificates: []tls.Certificate{s.config.Certificate},
		ClientCAs:    roots,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS13,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	s.mux.ServeHTTP(w, r)
}

// identityKey is where a request's caller is kept in its context.
type identityKey struct{}

// handle registers a route that only callers of one role may use. Every
// call is checked against the CA and the revocation list in the state, so a
// revoked certificate is refused at once on every agent.
func (s *Server) handle(pattern string, role pki.Role, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, errors.New("a client certificate is required"))
			return
		}
		identity, err := pki.Verify(s.config.CA, r.TLS.PeerCertificates[0], s.revoked, time.Now())
		if err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if identity.Role != role {
			writeError(w, http.StatusForbidden, fmt.Errorf("%s may not call %s", identity.Name, r.URL.Path))
			return
		}
		if !s.checkVersion(w, r) {
			return
		}
		if err := s.waitForMinIndex(r); err != nil {
			writeError(w, http.StatusGatewayTimeout, err)
			return
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	})
}

// handleOpen registers a route callers reach without a certificate: the
// bootstrap, which proves itself with the bootstrap key instead.
func (s *Server) handleOpen(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if s.checkVersion(w, r) {
			handler(w, r)
		}
	})
}

// checkVersion refuses a caller too far from this agent's version.
func (s *Server) checkVersion(w http.ResponseWriter, r *http.Request) bool {
	if caller := r.Header.Get(VersionHeader); !buildinfo.Compatible(caller, s.config.Version) {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"version %s cannot talk to agent %s (at most one minor version apart); update the older one",
			caller, s.config.Version))
		return false
	}
	return true
}

func (s *Server) revoked(serial string) bool {
	var revoked bool
	s.config.Cluster.Read(func(current *state.State) {
		_, revoked = current.Revoked[serial]
	})
	return revoked
}

// waitForMinIndex holds a request until this agent has applied the index the
// caller asks for.
func (s *Server) waitForMinIndex(r *http.Request) error {
	text := r.Header.Get(MinIndexHeader)
	if text == "" {
		return nil
	}
	index, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return fmt.Errorf("%s %q: %w", MinIndexHeader, text, err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), waitTimeout)
	defer cancel()
	return s.config.Cluster.WaitIndex(ctx, index)
}

func identityOf(r *http.Request) pki.Identity {
	identity, _ := r.Context().Value(identityKey{}).(pki.Identity)
	return identity
}

// actorOf is how the state names a caller.
func actorOf(identity pki.Identity) string {
	if identity.Role == pki.RoleHost {
		return state.HostActor(state.HostID(identity.HostID))
	}
	return state.UserActor(identity.Name)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	var status Status
	leaderID, hasLeader := s.config.Cluster.Leader()
	s.config.Cluster.Read(func(current *state.State) {
		status = Status{
			Name: current.Config.Spec.Name, Revision: current.Config.Revision,
			Index: current.Index, Hosts: hostInfos(current),
		}
		if leader, ok := current.Hosts[leaderID]; hasLeader && ok {
			status.Leader = leader.Name
		}
		for _, alert := range current.Alerts {
			status.Alerts = append(status.Alerts, AlertInfo{
				Key: alert.Key, Severity: alert.Severity, Subject: alert.Subject, Message: alert.Message, Since: alert.Since,
			})
		}
	})
	slices.SortFunc(status.Alerts, func(a, b AlertInfo) int {
		return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.Key, b.Key))
	})
	writeJSON(w, http.StatusOK, status)
}

// alertTest sends a test alert to every channel, from this agent.
func (s *Server) alertTest(w http.ResponseWriter, r *http.Request) {
	results, err := s.config.Alerts.Test(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// alertFailures lists the messages the channels have not taken yet: the
// leader keeps them, so a follower asks it.
func (s *Server) alertFailures(w http.ResponseWriter, r *http.Request) {
	leader, ok := s.config.Cluster.Leader()
	switch {
	case !ok:
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster has no leader right now"))
	case leader != s.config.HostID:
		s.relay(w, r, s.forwarder, leader, "/v1/internal/alerts/failures")
	default:
		s.localAlertFailures(w, r)
	}
}

func (s *Server) localAlertFailures(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.config.Alerts.Failures())
}

func (s *Server) hosts(w http.ResponseWriter, _ *http.Request) {
	var hosts []HostInfo
	s.config.Cluster.Read(func(current *state.State) {
		hosts = hostInfos(current)
	})
	writeJSON(w, http.StatusOK, hosts)
}

func hostInfos(current *state.State) []HostInfo {
	hosts := []HostInfo{}
	for _, id := range slices.Sorted(maps.Keys(current.Hosts)) {
		host := current.Hosts[id]
		hosts = append(hosts, HostInfo{
			ID: int(host.ID), Name: host.Name, Phase: string(host.Phase), Voter: host.Voter,
			Underlay: host.Underlay.String(), Version: host.Version,
		})
	}
	return hosts
}

// hostAction checks the phase a user action starts from; the state allows
// more moves (the leader's), so this is the user's narrower rule.
func (s *Server) hostAction(w http.ResponseWriter, r *http.Request) {
	index := slices.IndexFunc(HostActions, func(action HostAction) bool { return action.Name == r.PathValue("action") })
	if index < 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("no host action %q", r.PathValue("action")))
		return
	}
	action := HostActions[index]
	number, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("host ID %q is not a number", r.PathValue("id")))
		return
	}
	id := state.HostID(number)
	var phase state.HostPhase
	s.config.Cluster.Read(func(current *state.State) {
		if host, ok := current.Hosts[id]; ok {
			phase = host.Phase
		}
	})
	switch {
	case phase == "":
		writeError(w, http.StatusNotFound, fmt.Errorf("no host %d", id))
		return
	case len(action.From) > 0 && !slices.Contains(action.From, phase):
		writeError(w, http.StatusConflict, fmt.Errorf("host %d is %s; %s works on a host that is %s",
			id, phase, action.Name, phaseList(action.From)))
		return
	}
	// the phase checked here must still be the host's when it commits
	s.write(w, r, state.KindSetHostPhase, state.SetHostPhase{Host: id, Phase: action.To, From: phase})
}

// phaseList reads "active", "active or draining", ...
func phaseList(phases []state.HostPhase) string {
	names := make([]string, len(phases))
	for index, phase := range phases {
		names[index] = string(phase)
	}
	return strings.Join(names, " or ")
}

func (s *Server) users(w http.ResponseWriter, _ *http.Request) {
	users := []UserInfo{}
	s.config.Cluster.Read(func(current *state.State) {
		for _, name := range slices.Sorted(maps.Keys(current.Users)) {
			users = append(users, UserInfo{Name: name, Certificates: len(current.Users[name].Serials)})
		}
	})
	writeJSON(w, http.StatusOK, users)
}

// audit returns the last entries, newest last; ?limit= says how many
// (default 50).
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if text := r.URL.Query().Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("limit %q is not a positive number", text))
			return
		}
		limit = parsed
	}
	entries := []AuditEntry{}
	s.config.Cluster.Read(func(current *state.State) {
		for _, entry := range current.Audit[max(0, len(current.Audit)-limit):] {
			entries = append(entries, AuditEntry(entry))
		}
	})
	writeJSON(w, http.StatusOK, entries)
}

// userKinds are the commands a follower may forward for a user: exactly
// those the user routes of the API and the bootstrap make; a kind joins when
// its route does.
var userKinds = map[string]bool{
	state.KindApplyConfig: true, state.KindSetHostPhase: true,
	state.KindAddUser: true, // the bootstrap's first user
}

// hostKinds are what a host reports about itself.
var hostKinds = map[string]bool{
	state.KindReportHost: true, state.KindSetHostPhase: true, state.KindAdvanceOperation: true,
	state.KindSetStackStatus: true, state.KindForgetCopy: true,
}

// internalPropose is where a follower forwards a user's write and where an
// agent proposes what it reports about itself. Nothing else comes in from
// outside: the leader's own commands (operations, placements, new hosts)
// are made on the leader. A host may name the user it forwards for, since the
// user's certificate ended at the first agent; it may never speak for another
// host or for the leader.
func (s *Server) internalPropose(w http.ResponseWriter, r *http.Request) {
	var cmd state.Command
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the command: %w", err))
		return
	}
	caller := actorOf(identityOf(r))
	allowed := (state.IsUserActor(cmd.Actor) && userKinds[cmd.Kind]) ||
		(cmd.Actor == caller && hostKinds[cmd.Kind])
	if !allowed {
		writeError(w, http.StatusForbidden, fmt.Errorf("%s may not send %s as %s", caller, cmd.Kind, cmd.Actor))
		return
	}
	// no second hop: if the lead moved meanwhile, the caller asks again
	if index, ok := s.commit(w, r, cmd, s.proposeHere); ok {
		writeJSON(w, http.StatusOK, Written{Index: index})
	}
}

// write turns a request into a command and commits it.
func (s *Server) write(w http.ResponseWriter, r *http.Request, kind string, payload any) {
	// no time here: the leader stamps its own (proposeHere)
	cmd, err := state.NewCommand(kind, time.Time{}, actorOf(identityOf(r)), payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if index, ok := s.commit(w, r, cmd, s.Propose); ok {
		writeJSON(w, http.StatusOK, Written{Index: index})
	}
}

// commit commits a command with propose. On success it sets the index header
// and leaves the answer to the caller; on failure it answers with the error.
func (s *Server) commit(w http.ResponseWriter, r *http.Request, cmd state.Command,
	propose func(context.Context, state.Command) (uint64, error),
) (uint64, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), waitTimeout)
	defer cancel()
	index, err := propose(ctx, cmd)
	var refused *state.Refused
	var statusError *StatusError
	switch {
	case errors.As(err, &refused):
		writeError(w, http.StatusConflict, refused)
	case errors.As(err, &statusError):
		writeError(w, statusError.Status, errors.New(statusError.Message))
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		w.Header().Set(IndexHeader, strconv.FormatUint(index, 10))
		return index, true
	}
	return 0, false
}

// proposeHere commits a command on this agent, which must lead. The command
// takes the leader's clock (Command.At), whichever agent took the request.
func (s *Server) proposeHere(ctx context.Context, cmd state.Command) (uint64, error) {
	if !s.config.Cluster.IsLeader() {
		return 0, errors.New("not the leader; try again")
	}
	cmd.At = time.Now().UTC()
	return s.config.Cluster.Propose(ctx, cmd)
}

// Propose commits a command: directly on the leader, else by forwarding it
// to the leader's internal endpoint. It is the one way to commit, for the
// API's routes as for the agent's own reports and the leader's controllers.
func (s *Server) Propose(ctx context.Context, cmd state.Command) (uint64, error) {
	if s.config.Cluster.IsLeader() {
		return s.proposeHere(ctx, cmd)
	}
	id, ok := s.config.Cluster.Leader()
	if !ok {
		return 0, errors.New("the cluster has no leader right now")
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return 0, err
	}
	url := "https://" + s.config.APIAddress(id) + "/v1/internal/propose"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set(VersionHeader, s.config.Version)
	var written Written
	if err := send(s.forwarder, request, &written); err != nil {
		// the leader's answer goes back as it is, a refusal as a refusal
		var statusError *StatusError
		switch {
		case errors.As(err, &statusError) && statusError.Status == http.StatusConflict:
			return 0, &state.Refused{Err: errors.New(statusError.Message)}
		case errors.As(err, &statusError):
			return 0, statusError
		default:
			return 0, fmt.Errorf("forward to the leader (host %d): %w", id, err)
		}
	}
	return written.Index, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client is gone if this fails
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, Error{Message: err.Error()})
}
