package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webcodingcat/clowder/internal/state"
)

// MaxRequestBytes limits every request body. The largest body is an apply
// with every secret of the cluster (resource.MaxClusterSecretBytes in
// clear): sealed, they take about 8 MiB, and the limit leaves room for the
// rest. Text in stringData that JSON escapes (<, >, &, control characters)
// can grow to six times its size; such values belong in data or a
// SealedSecret.
const MaxRequestBytes = 16 << 20

// Cluster is what the server needs from the agent's Raft member.
type Cluster interface {
	Propose(ctx context.Context, cmd state.Command) (uint64, error)
	Read(use func(current *state.State))
	WaitIndex(ctx context.Context, index uint64) error
	// Changes returns the changes that the log made after an index, and
	// the state's index. It returns state.ErrChangesGone if the member no
	// longer keeps all of these changes.
	Changes(since uint64) ([]state.Change, uint64, error)
	// Changed closes when the next entry is applied.
	Changed() <-chan struct{}
	IsLeader() bool
	// Leader returns the leading host; ok is false while there is none.
	Leader() (id state.HostID, ok bool)
	TransferLeadership() error
	// VerifyLeader makes sure that a majority still accepts this member as
	// the leader.
	VerifyLeader(ctx context.Context) error
	// CommitIndex is the last index that this member knows to be committed.
	CommitIndex() uint64
	// WaitCommitted returns once the state holds every entry up to index.
	WaitCommitted(ctx context.Context, index uint64) error
}

// ServerConfig is what an agent's API server needs: what it knows about its
// host, the same HostConfig that its Peers have, and the parts it answers
// for.
type ServerConfig struct {
	HostConfig
	// Peers forwards what the server cannot answer itself.
	Peers *Peers
	// Containers answers for the stacks of this host.
	Containers Containers
	// Volumes is nil on an agent without a volume reconciler.
	Volumes Volumes
	// Recover answers clowderd recover on a control-plane host; it is nil
	// in tests that do not need it.
	Recover RecoverHost
	Alerts  Alerts
	Beats   Beats
	// Joins adds joining hosts while this agent leads.
	Joins Joins
	// HetznerServers lists the IDs of the servers that a Hetzner token
	// sees, at api ("" for Hetzner's own). The agent passes it in, because
	// api is portable and may not import the hetzner package. If it is nil,
	// the answer says that the token could not be checked.
	HetznerServers func(ctx context.Context, api, token string) ([]int64, error)
	// CaughtUp closes once this agent's state has caught up with the
	// leader's after its start. Until then, CLI calls wait, because the
	// state may still be empty. Nil means caught up (tests).
	CaughtUp <-chan struct{}
}

// Alerts is what the API needs from the agent's alerts loop.
type Alerts interface {
	// Test sends a test alert from this agent to every channel.
	Test(ctx context.Context) ([]AlertTestResult, error)
	// Failures returns the messages that a channel has not accepted yet.
	// Only the leader has any.
	Failures() []AlertFailure
}

// Server is an agent's API.
type Server struct {
	config ServerConfig
	mux    *http.ServeMux
	// streams ends with EndStreams. Answers that run until the client
	// stops (a watch, logs that follow) end with it, so that stopping the
	// agent does not wait for them.
	streams    context.Context
	endStreams context.CancelFunc
	// the join's routes have no certificate, so they count the tries
	joins limiter
	// warnedVersions holds the callers' versions already warned about, so
	// the log warns only once per version
	warnedVersions sync.Map
}

// NewServer sets up the routes.
func NewServer(config ServerConfig) *Server {
	server := &Server{config: config, mux: http.NewServeMux()}
	server.streams, server.endStreams = context.WithCancel(context.Background())
	for _, route := range server.routes() {
		server.handle(route.pattern, route.caller, route.handler)
	}
	server.handleOpen("GET /v1/join", limited(&server.joins, server.joinOffer))
	server.handleOpen("POST /v1/join", limited(&server.joins, server.join))
	return server
}

// route is a route that callers reach with a certificate.
type route struct {
	pattern string
	caller  caller
	handler http.HandlerFunc
}

// routes says who may call what (access.go).
func (s *Server) routes() []route {
	return []route{
		{"GET /v1/status", cliCaller, s.status},
		{"GET /v1/hosts", cliCaller, s.hosts},
		{"POST /v1/hosts/{id}/{action}", cliCaller, s.hostAction},
		{"POST /v1/join-tokens", cliCaller, s.newJoinToken},
		{"GET /v1/audit", cliCaller, s.audit},
		{"GET /v1/sealing-key", cliCaller, s.publicSealingKey},
		{"POST /v1/apply", cliCaller, s.applyResources},
		{"POST /v1/delete", cliCaller, s.deleteResources},
		{"GET /v1/resources/{kind}", cliCaller, s.listResources},
		{"GET /v1/resources/{kind}/{namespace}/{name}", cliCaller, s.getResource},
		{"GET /v1/resources/{kind}/{name}", cliCaller, s.getClusterResource},
		{"GET /v1/watch", cliCaller, s.watch},
		{"GET /v1/stacks/{namespace}/{name}/ps", cliCaller, s.stackPs},
		{"GET /v1/stacks/{namespace}/{name}/logs", cliCaller, s.stackLogs},
		{"POST /v1/stacks/{namespace}/{name}/restart", cliCaller, s.restartStack},
		{"POST /v1/stacks/{namespace}/{name}/move", cliCaller, s.moveStack},
		{"POST /v1/volumes/{namespace}/{name}/resolve-splitbrain", cliCaller, s.resolveSplitBrain},
		{"POST /v1/alerts/test", cliCaller, s.alertTest},
		{"GET /v1/alerts/failures", cliCaller, s.alertFailures},
		// a host's own reports, and the CLI's writes that a control-plane
		// host forwards (mayPropose tells them apart)
		{"POST /v1/internal/propose", anyHost, s.internalPropose},
		// what every host needs to keep up with the cluster: its beats to
		// the voters, the leader's commit index, and the known leader that
		// clowderd recover asks for
		{"POST /v1/internal/beats", anyHost, s.localBeat},
		{"GET /v1/internal/beats/{id}", anyHost, s.localSilence},
		{"GET /v1/internal/commit-index", anyHost, s.localCommitIndex},
		{"GET /v1/internal/leader", anyHost, s.knownLeader},
		// what a host needs for the stacks it runs and the Floating IP it
		// holds, each for itself (hostsecrets.go)
		{"GET /v1/internal/hosts/self/secrets", anyHost, s.hostSecrets},
		{"GET /v1/internal/hosts/self/tls", anyHost, s.hostCertificates},
		// what a control-plane host asks for the CLI, or as the leader
		{"POST /v1/internal/changes", controlPlaneHost, s.internalChanges},
		{"POST /v1/internal/join", controlPlaneHost, s.internalJoin},
		{"POST /v1/internal/hand-over", controlPlaneHost, s.internalHandOver},
		{"GET /v1/internal/alerts/failures", controlPlaneHost, s.localAlertFailures},
		{"GET /v1/internal/stacks/{project}/ps", controlPlaneHost, s.localPs},
		{"GET /v1/internal/stacks/{project}/logs", controlPlaneHost, s.localLogs},
		{"GET /v1/internal/volumes/progress", controlPlaneHost, s.localProgress},
		{"POST /v1/internal/volumes/{namespace}/{name}/resolve-splitbrain", controlPlaneHost, s.localResolve},
		// what clowderd recover on a control-plane host asks of a worker
		// that stays (recover.go)
		{"GET /v1/internal/recover/copies", controlPlaneHost, s.localCopies},
		{"POST /v1/internal/recover/rejoin", controlPlaneHost, s.localRejoin},
		// what root on this host asks for
		{"POST /v1/internal/credentials", thisHost, s.internalCredentials},
		{"GET /v1/internal/sealing-key", thisHost, s.internalSealingKey},
	}
}

// EndStreams ends the answers that run until the client stops. Otherwise
// http.Server's Shutdown would wait for them.
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	s.mux.ServeHTTP(w, r)
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

func (s *Server) alertTest(w http.ResponseWriter, r *http.Request) {
	results, err := s.config.Alerts.Test(r.Context())
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	case len(results) == 0: // 409: the configuration is the problem, not the request
		writeError(w, http.StatusConflict, errors.New("the configuration names no alert channel"))
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// alertFailures asks the leader, which keeps the failures.
func (s *Server) alertFailures(w http.ResponseWriter, r *http.Request) {
	leader, ok := s.config.Cluster.Leader()
	switch {
	case !ok:
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster has no leader right now"))
	case leader != s.config.HostID:
		s.relay(w, r, s.config.Peers.forwarder, leader, "/v1/internal/alerts/failures")
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

// hostAction checks the phase that a user's host action starts from. The
// state allows more phase changes, because the leader makes them too.
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
	// the commit fails if the host's phase changed since this check
	move := state.SetHostPhase{Host: id, Phase: action.To, From: phase}
	if action.To == state.Removed {
		if leaves := s.leaves(id); leaves != nil {
			cmd, err := state.NewCommand(state.KindSetHostPhase, time.Time{}, actorOf(identityOf(r)), *leaves)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if index, ok := s.commit(w, r, cmd, s.config.Peers.Propose); ok {
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

// handOverLeadOf makes a leader that is about to be removed hand over
// first, because the removal revokes its certificate and would end its
// leadership abruptly. If the state would refuse the removal, the
// leadership stays, and the commit then says why.
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

func (s *Server) askLeaderToHandOver(ctx context.Context, id state.HostID) error {
	url := "https://" + s.config.APIAddress(id) + "/v1/internal/hand-over"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("ask host %d to hand its lead over: %w", id, err)
	}
	request.Header.Set(VersionHeader, s.config.Version)
	if err := send(s.config.Peers.forwarder, request, nil); err != nil {
		return fmt.Errorf("ask host %d to hand its lead over: %w", id, err)
	}
	return nil
}

// internalHandOver hands over only if the state would allow this leader's
// removal.
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

func phaseList(phases []state.HostPhase) string {
	names := make([]string, len(phases))
	for index, phase := range phases {
		names[index] = string(phase)
	}
	return strings.Join(names, " or ")
}

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

func (s *Server) internalPropose(w http.ResponseWriter, r *http.Request) {
	// lenient, unlike the CLI's requests: the forwarding agent may run the
	// next release, and a command may carry fields that this one does not
	// know (State, Versions)
	var cmd state.Command
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read the command: %w", err))
		return
	}
	if identity := identityOf(r); !mayPropose(identity, cmd) {
		writeError(w, http.StatusForbidden, fmt.Errorf("%s may not send %s as %s", actorOf(identity), cmd.Kind, cmd.Actor))
		return
	}
	// no second hop: if another agent leads by now, the caller asks again
	if index, ok := s.commit(w, r, cmd, s.config.proposeHere); ok {
		writeJSON(w, http.StatusOK, Written{Index: index})
	}
}

func (s *Server) commitRequest(w http.ResponseWriter, r *http.Request, kind string, payload any) {
	// no time here: the leader stamps its own clock (proposeHere)
	cmd, err := state.NewCommand(kind, time.Time{}, actorOf(identityOf(r)), payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if index, ok := s.commit(w, r, cmd, s.config.Peers.Propose); ok {
		writeJSON(w, http.StatusOK, Written{Index: index})
	}
}

// commit leaves the answer to the caller on success, and answers with the
// error on failure.
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client is gone if this fails
}

func writeError(w http.ResponseWriter, status int, err error) {
	// a body over MaxRequestBytes fails where a handler reads it, and the
	// handler reports a bad request; this one place turns that into 413
	// for every handler
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
		err = fmt.Errorf("the request is larger than %d bytes", MaxRequestBytes)
	}
	writeJSON(w, status, Error{Message: err.Error()})
}
