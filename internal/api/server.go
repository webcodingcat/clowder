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
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// waitTimeout bounds how long a read waits for its minimum index and a write
// for the leader.
const waitTimeout = 10 * time.Second

// MaxRequestBytes bounds every request body. The largest body is an apply
// of a directory: every resource of every namespace in one request, and
// with them every secret of the cluster, at most
// resource.MaxClusterSecretBytes (4 MiB) in clear. Sealed for the cluster
// (internal/secretvalue), a value grows to 1.8 times its size plus less
// than 1 KiB; in a Secret's data, to 1.34 times. So all secrets together
// take about 4 MiB × 1.8 + resource.MaxClusterSecrets × 1 KiB, about
// 8.2 MiB, and the rest is for stacks and volumes. Text in a Secret's
// stringData that JSON escapes (<, >, &, control characters) takes up to
// six times its size; a request of such values may hit the limit, and
// data or a SealedSecret carries them instead.
const MaxRequestBytes = 16 << 20

// Cluster is what the server needs from the agent's Raft member.
type Cluster interface {
	Propose(ctx context.Context, cmd state.Command) (uint64, error)
	Read(use func(current *state.State))
	WaitIndex(ctx context.Context, index uint64) error
	// Changes returns what the log changed after an index, and the state's
	// index. It returns state.ErrChangesGone if the member no longer keeps
	// all of these changes.
	Changes(since uint64) ([]state.Change, uint64, error)
	// Changed closes when the next entry is applied.
	Changed() <-chan struct{}
	IsLeader() bool
	// Leader returns the leading host; ok is false while there is none.
	Leader() (id state.HostID, ok bool)
	// TransferLeadership hands this member's lead to another voter.
	TransferLeadership() error
	// VerifyLeader makes sure that a majority still accepts this member as
	// leader.
	VerifyLeader(ctx context.Context) error
	// CommitIndex is the last index that this member knows to be committed.
	CommitIndex() uint64
	// WaitCommitted returns once the state holds every entry up to index.
	WaitCommitted(ctx context.Context, index uint64) error
}

// ServerConfig is what an agent's API server needs.
type ServerConfig struct {
	Cluster Cluster
	CA      *x509.Certificate
	// Certificate is the host's certificate. It serves TLS and forwards
	// writes.
	Certificate tls.Certificate
	// APIAddress gives a host's API address (its node address and Port).
	APIAddress func(id state.HostID) string
	// Keys encrypts secrets for the state and decrypts them (the data key).
	Keys Keys
	// HostID is this agent's host; Containers answers for its stacks.
	HostID     state.HostID
	Containers Containers
	// Volumes tells how far this host's copies have synced. The volume
	// reconciler implements it; it is nil on an agent without one.
	Volumes Volumes
	// Alerts answers for the alert channels. The agent's alerts loop
	// implements it.
	Alerts Alerts
	// Beats keeps the beats of the hosts (controller.Heard).
	Beats Beats
	// Joins adds joining hosts while this agent leads.
	Joins Joins
	// HetznerServers lists the IDs of the servers that a Hetzner token
	// sees, in one request to the API at api ("" for Hetzner's own). The
	// dry run of an apply that changes the token reports what it finds. The
	// agent passes in the function from hetzner, which the API, a portable
	// package, may not import. If it is nil, the answer says that the token
	// could not be checked.
	HetznerServers func(ctx context.Context, api, token string) ([]int64, error)
	// CaughtUp closes once this agent's state has caught up with the
	// leader's after its start. Until then, the CLI's calls wait for it,
	// because the state may still be empty. Nil means caught up (tests).
	CaughtUp <-chan struct{}
	Version  string
	Logger   *slog.Logger
}

// Alerts is what the API asks of the agent's alerts loop.
type Alerts interface {
	// Test sends a test alert to every channel from this agent.
	Test(ctx context.Context) ([]AlertTestResult, error)
	// Failures are the messages that a channel has not accepted yet. Only
	// the leader has any.
	Failures() []AlertFailure
}

// Keys is the cluster's data key as the server uses it.
type Keys interface {
	Encrypt(plaintext []byte) (state.Encrypted, error)
	Decrypt(encrypted state.Encrypted) ([]byte, error)
	// Derive gives a key of its own for one purpose (the secrets' digests).
	Derive(purpose string) ([]byte, error)
}

// Server is an agent's API.
type Server struct {
	config ServerConfig
	mux    *http.ServeMux
	// forwarder carries writes to the leader. streamer relays answers of
	// other hosts that may take long (logs that follow), so it has no
	// timeout.
	forwarder *http.Client
	streamer  *http.Client
	// streams ends with EndStreams: answers that run until the client
	// stops (a watch, logs that follow) end with it, so that stopping the
	// agent does not wait for them
	streams    context.Context
	endStreams context.CancelFunc
	// the join's routes come without a certificate, so they count their
	// tries
	joins limiter
	// warnedVersions are the callers' versions that checkVersion has
	// warned about, so the log warns only once
	warnedVersions sync.Map
}

// NewServer sets up the routes.
func NewServer(config ServerConfig) *Server {
	server := &Server{config: config, mux: http.NewServeMux()}
	server.streams, server.endStreams = context.WithCancel(context.Background())
	transport := &http.Transport{TLSClientConfig: pki.ClientTLSToHosts(config.Certificate, config.CA, server.revoked)}
	server.forwarder = &http.Client{Timeout: waitTimeout, Transport: transport}
	server.streamer = &http.Client{Transport: transport}
	server.handle("GET /v1/status", pki.RoleCLI, server.status)
	server.handle("GET /v1/hosts", pki.RoleCLI, server.hosts)
	server.handle("POST /v1/hosts/{id}/{action}", pki.RoleCLI, server.hostAction)
	server.handle("POST /v1/join-tokens", pki.RoleCLI, server.newJoinToken)
	server.handle("GET /v1/audit", pki.RoleCLI, server.audit)
	server.handle("GET /v1/sealing-key", pki.RoleCLI, server.publicSealingKey)
	server.handle("POST /v1/apply", pki.RoleCLI, server.applyResources)
	server.handle("POST /v1/delete", pki.RoleCLI, server.deleteResources)
	server.handle("GET /v1/resources/{kind}", pki.RoleCLI, server.listResources)
	server.handle("GET /v1/resources/{kind}/{namespace}/{name}", pki.RoleCLI, server.getResource)
	server.handle("GET /v1/resources/{kind}/{name}", pki.RoleCLI, server.getClusterResource)
	server.handle("GET /v1/watch", pki.RoleCLI, server.watch)
	server.handle("GET /v1/stacks/{namespace}/{name}/ps", pki.RoleCLI, server.stackPs)
	server.handle("GET /v1/stacks/{namespace}/{name}/logs", pki.RoleCLI, server.stackLogs)
	server.handle("POST /v1/stacks/{namespace}/{name}/restart", pki.RoleCLI, server.restartStack)
	server.handle("POST /v1/stacks/{namespace}/{name}/move", pki.RoleCLI, server.moveStack)
	server.handle("POST /v1/volumes/{namespace}/{name}/resolve-splitbrain", pki.RoleCLI, server.resolveSplitBrain)
	server.handle("POST /v1/alerts/test", pki.RoleCLI, server.alertTest)
	server.handle("GET /v1/alerts/failures", pki.RoleCLI, server.alertFailures)
	server.handle("GET /v1/internal/alerts/failures", pki.RoleHost, server.localAlertFailures)
	server.handle("POST /v1/internal/propose", pki.RoleHost, server.internalPropose)
	server.handle("POST /v1/internal/changes", pki.RoleHost, server.internalChanges)
	server.handle("POST /v1/internal/join", pki.RoleHost, server.internalJoin)
	server.handle("POST /v1/internal/hand-over", pki.RoleHost, server.internalHandOver)
	server.handle("POST /v1/internal/credentials", pki.RoleHost, server.internalCredentials)
	server.handle("GET /v1/internal/sealing-key", pki.RoleHost, server.internalSealingKey)
	server.handle("GET /v1/internal/stacks/{project}/ps", pki.RoleHost, server.localPs)
	server.handle("GET /v1/internal/stacks/{project}/logs", pki.RoleHost, server.localLogs)
	server.handle("GET /v1/internal/volumes/progress", pki.RoleHost, server.localProgress)
	server.handle("POST /v1/internal/volumes/{namespace}/{name}/resolve-splitbrain", pki.RoleHost, server.localResolve)
	server.handle("GET /v1/internal/commit-index", pki.RoleHost, server.localCommitIndex)
	server.handle("GET /v1/internal/leader", pki.RoleHost, server.knownLeader)
	server.handle("POST /v1/internal/beats", pki.RoleHost, server.localBeat)
	server.handle("GET /v1/internal/beats/{id}", pki.RoleHost, server.localSilence)
	server.handleOpen("GET /v1/join", limited(&server.joins, server.joinOffer))
	server.handleOpen("POST /v1/join", limited(&server.joins, server.join))
	return server
}

// EndStreams ends the answers that run until the client stops; otherwise
// http.Server's Shutdown would wait for them. The agent calls it as it
// stops.
func (s *Server) EndStreams() {
	s.endStreams()
}

// streamContext is a request's context that also ends with EndStreams.
func (s *Server) streamContext(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.streams, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// TLSConfig is the server's side of mutual TLS. A client certificate is
// checked when given. The routes refuse calls without one, except the
// routes of handleOpen.
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
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	s.mux.ServeHTTP(w, r)
}

// identityKey is where a request's caller is kept in its context.
type identityKey struct{}

// handle registers a route that only callers of one role may use. Every
// call is checked against the CA, the revocation list and the CLI's cutoff
// in the state, so a revoked certificate is refused at once on every agent.
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
		if identity.Role == pki.RoleCLI && !s.cliCutoffPassed(identity.NotBefore) {
			writeError(w, http.StatusUnauthorized, errors.New("this CLI certificate was revoked with all issued "+
				"before a newer one (clowderd credentials --revoke-older); get new credentials from a host"))
			return
		}
		if identity.Role != role {
			writeError(w, http.StatusForbidden, fmt.Errorf("%s may not call %s", identity.Name, r.URL.Path))
			return
		}
		if !s.checkVersion(w, r) {
			return
		}
		if role == pki.RoleCLI && !s.waitForCatchUp(r) {
			writeError(w, http.StatusServiceUnavailable,
				errors.New("this agent has not caught up with the cluster's state since it started; try again"))
			return
		}
		s.warnAboutDevelopment(r.Header.Get(VersionHeader))
		if err := s.waitForMinIndex(r); err != nil {
			writeError(w, http.StatusGatewayTimeout, err)
			return
		}
		// the audit: which CLI certificate asked for a change, on the agent it
		// reached; the Raft command carries the same actor
		if role == pki.RoleCLI && r.Method != http.MethodGet {
			s.config.Logger.Info("a CLI request", "actor", actorOf(identity), "method", r.Method, "path", r.URL.Path)
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	})
}

// handleOpen registers a route that callers reach without a certificate:
// the join, which proves itself with a join token instead.
func (s *Server) handleOpen(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if s.checkVersion(w, r) {
			handler(w, r)
		}
	})
}

// checkVersion refuses a caller too far from this agent's version
// (buildinfo.Compatible).
func (s *Server) checkVersion(w http.ResponseWriter, r *http.Request) bool {
	caller := r.Header.Get(VersionHeader)
	if !buildinfo.Compatible(caller, s.config.Version) {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"version %s cannot talk to agent %s (before 1.0.0 the same minor version, then at most one "+
				"minor version apart); update the older one", caller, s.config.Version))
		return false
	}
	return true
}

// warnAboutDevelopment logs, once per version, that a development build
// talks to another build, which Compatible lets through. Only callers with
// a certificate reach it, because on the open routes anyone could fill the
// log and the map with versions.
func (s *Server) warnAboutDevelopment(caller string) {
	development := buildinfo.IsDevelopment(caller) || buildinfo.IsDevelopment(s.config.Version)
	if caller == "" || caller == s.config.Version || !development {
		return
	}
	if _, warned := s.warnedVersions.LoadOrStore(caller, true); !warned {
		s.config.Logger.Warn("a development build talks to another build; their formats may differ",
			"caller", caller, "agent", s.config.Version)
	}
}

func (s *Server) revoked(serial string) bool {
	var revoked bool
	s.config.Cluster.Read(func(current *state.State) {
		_, revoked = current.Revoked[serial]
	})
	return revoked
}

// waitForCatchUp holds the CLI's request until this agent has caught up
// after its start (ServerConfig.CaughtUp), for at most waitTimeout. Host
// routes do not wait, because the catch-up itself, the beats and the joins
// use them.
func (s *Server) waitForCatchUp(r *http.Request) bool {
	if s.config.CaughtUp == nil {
		return true
	}
	select {
	case <-s.config.CaughtUp:
		return true
	case <-r.Context().Done():
		return false
	case <-time.After(waitTimeout):
		return false
	}
}

// waitForMinIndex holds a request until this agent has applied the index
// that the caller asks for.
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

// actorOf returns the name that the state uses for a caller.
func actorOf(identity pki.Identity) string {
	if identity.Role == pki.RoleHost {
		return state.HostActor(state.HostID(identity.HostID))
	}
	return state.CLIActor(identity.Name)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	var status Status
	leaderID, hasLeader := s.config.Cluster.Leader()
	s.config.Cluster.Read(func(current *state.State) {
		status = Status{
			Name: current.ClusterName, Generation: current.Config.Generation,
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
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	case len(results) == 0: // the cluster's config is in the way, not the request
		writeError(w, http.StatusConflict, errors.New("the configuration names no alert channel"))
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// alertFailures lists the messages that the channels have not accepted
// yet. The leader keeps them, so a follower asks the leader.
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
			ID: int(host.ID), Name: host.Name, Roles: host.Roles, Phase: string(host.Phase), Voter: host.Voter,
			Underlay: host.Underlay.String(), PublicAddress: host.PublicAddress.String(), Version: host.Version,
		})
	}
	return hosts
}

// hostAction checks the phase that a user action starts from. The state
// allows more moves (the leader's), so this is the narrower rule for
// users.
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
	move := state.SetHostPhase{Host: id, Phase: action.To, From: phase}
	if action.To == state.Removed {
		if leaves := s.leaves(id); leaves != nil {
			cmd, err := state.NewCommand(state.KindSetHostPhase, time.Time{}, actorOf(identityOf(r)), *leaves)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if index, ok := s.commit(w, r, cmd, s.Propose); ok {
				writeJSON(w, http.StatusOK, HostActionAnswer{Written: Written{Index: index}, Leaving: true})
			}
			return
		}
		if err := s.handOverLeadOf(r.Context(), id); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	s.commitRequest(w, r, state.KindSetHostPhase, move)
}

// leaderPoll is how often handOverLeadOf checks whether the lead has moved.
const leaderPoll = 50 * time.Millisecond

// handOverLeadOf has a host that is to be removed hand over its lead
// first, if it leads. Its certificate is revoked with the removal, and a
// leader that the others no longer accept would end its lead abruptly. A
// removal that the state would refuse moves no lead; its commit then says
// why.
func (s *Server) handOverLeadOf(ctx context.Context, id state.HostID) error {
	leader, ok := s.config.Cluster.Leader()
	if !ok || leader != id {
		return nil
	}
	var refused error
	s.config.Cluster.Read(func(current *state.State) { refused = current.CheckRemoval(id) })
	if refused != nil {
		return nil
	}
	var err error
	if s.config.Cluster.IsLeader() {
		err = s.config.Cluster.TransferLeadership()
	} else {
		err = s.askLeaderToHandOver(ctx, id)
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		if leader, ok := s.config.Cluster.Leader(); ok && leader != id {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host %d still leads; try again", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(leaderPoll):
		}
	}
}

// askLeaderToHandOver asks the leading host to hand its lead over.
func (s *Server) askLeaderToHandOver(ctx context.Context, id state.HostID) error {
	url := "https://" + s.config.APIAddress(id) + "/v1/internal/hand-over"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("ask host %d to hand its lead over: %w", id, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	if err := send(s.forwarder, request, nil); err != nil {
		return fmt.Errorf("ask host %d to hand its lead over: %w", id, err)
	}
	return nil
}

// internalHandOver is where a host asks the leader to hand over its lead
// before a user removes it. Only a leader that the state would remove
// hands over.
func (s *Server) internalHandOver(w http.ResponseWriter, _ *http.Request) {
	if !s.config.Cluster.IsLeader() {
		writeJSON(w, http.StatusOK, Written{})
		return
	}
	var refused error
	s.config.Cluster.Read(func(current *state.State) { refused = current.CheckRemoval(s.config.HostID) })
	if refused != nil {
		writeError(w, http.StatusConflict, refused)
		return
	}
	if err := s.config.Cluster.TransferLeadership(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, Written{})
}

// phaseList formats phases as "active", "active or draining", and so on.
func phaseList(phases []state.HostPhase) string {
	names := make([]string, len(phases))
	for index, phase := range phases {
		names[index] = string(phase)
	}
	return strings.Join(names, " or ")
}

// audit returns the last entries, newest last. ?limit= says how many
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

// cliKinds are the commands that a follower may forward for the CLI:
// exactly those that the CLI routes of the API make. A kind is added here
// when its route is added.
var cliKinds = map[string]bool{
	state.KindSetHostPhase: true, state.KindRestartStack: true, state.KindRequestMove: true,
	state.KindAddJoinToken: true,
}

// hostKinds are the commands that a host commits as itself: what it
// reports about itself, and the cutoff that root on the host sets
// (clowderd credentials).
var hostKinds = map[string]bool{
	state.KindReportHost:     true,
	state.KindSetStackStatus: true, state.KindForgetCopy: true, state.KindSetVolumeInitialized: true,
	state.KindReportPeerForgotten: true, state.KindSetCopySynced: true,
	state.KindAdvanceOperation: true, // the step of an operation a host owns
	state.KindSetHostPhase:     true, // its own: recovering, then active (the state checks who)
	state.KindSetCLICutoff:     true,
}

// internalPropose is where a follower forwards the CLI's writes, and where
// an agent proposes what it commits as itself. Nothing else comes in from
// outside: the leader makes its own commands (placements, the ingress)
// itself. A host may forward for the CLI, whose certificate ended at the
// first agent, but it may never speak for another host or for the leader.
// The host names the CLI's actor (cli/<name>) and could name another. The
// name grants nothing, and root on any host holds every key anyway.
func (s *Server) internalPropose(w http.ResponseWriter, r *http.Request) {
	// not strict, unlike the CLI's requests: the agent that forwards may be
	// of the next release, and a command may carry fields this one does not
	// know (State, Versions)
	var cmd state.Command
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the command: %w", err))
		return
	}
	caller := actorOf(identityOf(r))
	allowed := (state.IsCLIActor(cmd.Actor) && cliKinds[cmd.Kind]) ||
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

// commitRequest turns a request into a command and commits it.
func (s *Server) commitRequest(w http.ResponseWriter, r *http.Request, kind string, payload any) {
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

// commit commits a command with propose. On success, it sets the index
// header and leaves the answer to the caller. On failure, it answers with
// the error.
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
// gets the leader's clock (Command.At), whichever agent took the request.
func (s *Server) proposeHere(ctx context.Context, cmd state.Command) (uint64, error) {
	if !s.config.Cluster.IsLeader() {
		return 0, errors.New("not the leader; try again")
	}
	cmd.At = time.Now().UTC()
	return s.config.Cluster.Propose(ctx, cmd)
}

// Propose commits a command: directly on the leader, or else by forwarding
// it to the leader's internal endpoint. It is the one way to commit, for
// the API's routes as well as for the agent's own reports and the leader's
// controllers.
func (s *Server) Propose(ctx context.Context, cmd state.Command) (uint64, error) {
	id, here, err := s.waitForLeader(ctx)
	switch {
	case err != nil:
		return 0, err
	case here:
		return s.proposeHere(ctx, cmd)
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("encode the command: %w", err)
	}
	url := "https://" + s.config.APIAddress(id) + "/v1/internal/propose"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("forward to the leader (host %d): %w", id, err)
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
	// a body over MaxRequestBytes fails where it is read, wrapped as a bad
	// request; this is the one place that reports it for every handler
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
		err = fmt.Errorf("the request is larger than %d bytes", MaxRequestBytes)
	}
	writeJSON(w, status, Error{Message: err.Error()})
}

// leaderWait is how long a write waits for a leader. An election takes
// seconds, and a peer that restarts may lose its leader for a moment.
const leaderWait = 10 * time.Second

// waitForLeader returns where a write goes, as soon as it can go somewhere
// and for at most leaderWait: here (this agent leads), or the leading
// host. During an election, or while this agent has just become leader and
// applies its log (raftnode.Node.IsLeader), a write waits a moment instead
// of failing.
func (s *Server) waitForLeader(ctx context.Context) (leader state.HostID, here bool, err error) {
	deadline := time.Now().Add(leaderWait)
	for {
		if s.config.Cluster.IsLeader() {
			return s.config.HostID, true, nil
		}
		if id, ok := s.config.Cluster.Leader(); ok && id != s.config.HostID {
			return id, false, nil
		}
		if time.Now().After(deadline) {
			return 0, false, errors.New("the cluster has no leader right now")
		}
		select {
		case <-ctx.Done():
			return 0, false, fmt.Errorf("wait for a leader: %w", ctx.Err())
		case <-time.After(leaderPoll):
		}
	}
}
