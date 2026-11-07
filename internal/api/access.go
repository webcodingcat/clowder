package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// docs/developing/api.md (Who may call what) explains the access rules.

// caller says who may call a route.
type caller int

const (
	cliCaller caller = iota
	// anyHost is a host of the cluster with any roles.
	anyHost
	controlPlaneHost
	// thisHost is this agent's own host certificate. Root on the host uses
	// it to ask its own agent (clowderd credentials, clowderd sealing-key).
	thisHost
)

func (c caller) String() string {
	switch c {
	case cliCaller:
		return "the CLI"
	case anyHost:
		return "hosts"
	case controlPlaneHost:
		return string(hostrole.ControlPlane) + " hosts"
	case thisHost:
		return "this host itself"
	default:
		return "nobody"
	}
}

func (s *Server) admits(rule caller, identity pki.Identity) bool {
	switch rule {
	case cliCaller:
		return identity.Role == pki.RoleCLI
	case anyHost:
		return identity.Role == pki.RoleHost
	case controlPlaneHost:
		return identity.Role == pki.RoleHost && identity.HostRoles.Has(hostrole.ControlPlane)
	case thisHost:
		return identity.Role == pki.RoleHost && state.HostID(identity.HostID) == s.config.HostID
	default:
		return false
	}
}

// senders says who may send a kind of command through internal/propose.
// Every sender is a host: its certificate is the caller's.
type senders struct {
	// cli: a control-plane host that forwards the CLI's command, with the
	// CLI's actor. The host could name another CLI actor, which is
	// accepted, because the name grants nothing.
	cli bool
	// anyHost: any host about itself, as itself. The state checks that the
	// command is about the actor's own host.
	anyHost bool
	// controlPlane: a control-plane host for the cluster, as itself.
	controlPlane bool
}

// proposers holds every kind that may come in from outside, with who may
// send it; the leader writes every other kind itself. The CLI's kinds are
// exactly those of its write routes (TestCLIKindsAreTheKindsOfTheCLIsWrites).
var proposers = map[string]senders{
	// the CLI's host actions, and a host's own way back from a fence
	// (recovering, then active)
	state.KindSetHostPhase:         {cli: true, anyHost: true},
	state.KindRestartStack:         {cli: true},
	state.KindRequestMove:          {cli: true},
	state.KindAddJoinToken:         {cli: true},
	state.KindReportHost:           {anyHost: true},
	state.KindSetStackStatus:       {anyHost: true},
	state.KindForgetCopy:           {anyHost: true},
	state.KindSetVolumeInitialized: {anyHost: true},
	state.KindReportPeerForgotten:  {anyHost: true},
	state.KindSetCopySynced:        {anyHost: true},
	state.KindAdvanceOperation:     {anyHost: true}, // a step of an operation the host owns
	state.KindSetCLICutoff:         {controlPlane: true},
}

// mayPropose guards internal/propose, the only way a command comes in from
// outside. No host may speak for another host or for the leader. The route
// takes only host certificates (anyHost), so identity is always a host's.
// The actor decides which of a kind's senders applies.
func mayPropose(identity pki.Identity, cmd state.Command) bool {
	allowed := proposers[cmd.Kind]
	controlPlane := identity.HostRoles.Has(hostrole.ControlPlane)
	switch {
	case state.IsCLIActor(cmd.Actor):
		return allowed.cli && controlPlane
	case cmd.Actor == actorOf(identity):
		return allowed.anyHost || (allowed.controlPlane && controlPlane)
	}
	return false
}

// TLSConfig is the server's side of mutual TLS. A control-plane host lets
// a client without a certificate through the handshake, for the routes of
// handleOpen; the other routes refuse it. A host without the control plane
// accepts only control-plane hosts.
func (s *Server) TLSConfig() *tls.Config {
	if !s.config.Roles.Has(hostrole.ControlPlane) {
		return pki.ServerTLSForControlPlane(s.config.Certificate, s.config.CA, s.config.revoked)
	}
	roots := x509.NewCertPool()
	roots.AddCert(s.config.CA)
	return &tls.Config{
		Certificates: []tls.Certificate{s.config.Certificate},
		ClientCAs:    roots,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS13,
	}
}

type identityKey struct{}

// handle checks every call against the state's revocation list and CLI
// cutoff, so every agent refuses a revoked certificate at once.
func (s *Server) handle(pattern string, rule caller, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, errors.New("a client certificate is required"))
			return
		}
		identity, err := pki.Verify(s.config.CA, r.TLS.PeerCertificates[0], s.config.revoked, time.Now())
		if err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if identity.Role == pki.RoleCLI && !s.cliCutoffPassed(identity.NotBefore) {
			writeError(w, http.StatusUnauthorized, errors.New("this CLI certificate was revoked with all issued "+
				"before a newer one (clowderd credentials --revoke-older); get new credentials from a host"))
			return
		}
		if !s.admits(rule, identity) {
			writeError(w, http.StatusForbidden, fmt.Errorf("%s may not call %s, which is for %s", identity.Name, r.URL.Path, rule))
			return
		}
		if !s.checkVersion(w, r) {
			return
		}
		if rule == cliCaller && !s.waitForCatchUp(r) {
			writeError(w, http.StatusServiceUnavailable,
				errors.New("this agent has not caught up with the cluster's state since it started; try again"))
			return
		}
		s.warnAboutDevelopment(r.Header.Get(VersionHeader))
		if err := s.waitForMinIndex(r); err != nil {
			writeError(w, http.StatusGatewayTimeout, err)
			return
		}
		// audit: log which CLI certificate asked for a change, on the agent
		// it reached; the Raft command carries the same actor
		if rule == cliCaller && r.Method != http.MethodGet {
			s.config.Logger.Info("a CLI request", "actor", actorOf(identity), "method", r.Method, "path", r.URL.Path)
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	})
}

// handleOpen registers a route without a certificate: the join, which
// proves itself with a join token instead.
func (s *Server) handleOpen(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if s.checkVersion(w, r) {
			handler(w, r)
		}
	})
}

func (s *Server) checkVersion(w http.ResponseWriter, r *http.Request) bool {
	callerVersion := r.Header.Get(VersionHeader)
	if !buildinfo.Compatible(callerVersion, s.config.Version) {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"version %s cannot talk to agent %s (before 1.0.0 the same minor version, then at most one "+
				"minor version apart); update the older one", callerVersion, s.config.Version))
		return false
	}
	return true
}

// warnAboutDevelopment must be reached only by callers with a certificate.
// On the open routes, anyone could fill the log and the map with versions.
func (s *Server) warnAboutDevelopment(callerVersion string) {
	development := buildinfo.IsDevelopment(callerVersion) || buildinfo.IsDevelopment(s.config.Version)
	if callerVersion == "" || callerVersion == s.config.Version || !development {
		return
	}
	if _, warned := s.warnedVersions.LoadOrStore(callerVersion, true); !warned {
		s.config.Logger.Warn("a development build talks to another build; their formats may differ",
			"caller", callerVersion, "agent", s.config.Version)
	}
}

// waitForCatchUp holds only CLI requests, because the catch-up itself, the
// beats and the joins use the host routes.
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

// consistent answers a read only once this agent's state holds everything
// that the leader had committed when the read came in, as kube-apiserver's
// reads are consistent. A write's index reaches only the reads of the same
// CLI process (MinIndexHeader); without this, a follower that has not
// applied a write yet would hide it from the next command. The read then
// needs a leader. The watch needs no more: it follows a list's index.
func (s *Server) consistent(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), waitTimeout)
		err := s.config.Peers.CatchUpWithLeader(ctx)
		cancel()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("catch up with the leader before the read: %w", err))
			return
		}
		handler(w, r)
	}
}

func identityOf(r *http.Request) pki.Identity {
	identity, _ := r.Context().Value(identityKey{}).(pki.Identity)
	return identity
}

func actorOf(identity pki.Identity) string {
	if identity.Role == pki.RoleHost {
		return state.HostActor(state.HostID(identity.HostID))
	}
	return state.CLIActor(identity.Name)
}
