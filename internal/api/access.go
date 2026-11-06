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

// Access control: who may call what. As with Kubernetes' node authorizer,
// every route names the callers it accepts (Server.routes). A host may act
// only for itself (mayPropose, and the state's checks of each report). The
// checks read the caller's certificate, which names the host's roles.
//
// On a host without the control plane, the TLS handshake already refuses
// every client certificate except a control-plane host's (TLSConfig). Only
// the control plane calls such a host. So it serves neither the CLI nor the
// join, and it does not listen on its public address. On a control-plane
// host, the routes tell the callers apart.

// caller says who may call a route.
type caller int

const (
	// cliCaller is a CLI certificate.
	cliCaller caller = iota
	// anyHost is a host of the cluster with any roles.
	anyHost
	// controlPlaneHost is a host with the role control-plane.
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

// admits tells whether a verified caller may call a route with this rule.
// An unknown rule admits nobody.
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

// cliKinds are the commands that a control-plane host may forward for the
// CLI. The CLI's certificate ends at that host. The list holds exactly the
// kinds that the CLI's routes make; add a kind here when you add its route.
// The host names the CLI's actor (cli/<name>) and could name another one.
// That is accepted, because the name grants nothing.
var cliKinds = map[string]bool{
	state.KindSetHostPhase: true, state.KindRestartStack: true, state.KindRequestMove: true,
	state.KindAddJoinToken: true,
}

// hostKinds are the reports that any host sends about itself, as itself.
// The state checks that each report is about the actor's own host (its
// stacks, its copies, the operation it owns), as Kubernetes'
// NodeRestriction does.
var hostKinds = map[string]bool{
	state.KindReportHost:     true,
	state.KindSetStackStatus: true, state.KindForgetCopy: true, state.KindSetVolumeInitialized: true,
	state.KindReportPeerForgotten: true, state.KindSetCopySynced: true,
	state.KindAdvanceOperation: true, // a step of an operation the host owns
	state.KindSetHostPhase:     true, // its own: recovering, then active (the state checks who)
}

// controlPlaneKinds are the commands that a control-plane host sends for
// the cluster, as itself. The only one is the cutoff for CLI certificates,
// which root on that host sets (clowderd credentials --revoke-older).
var controlPlaneKinds = map[string]bool{
	state.KindSetCLICutoff: true,
}

// mayPropose tells whether a host may send a command to the leader
// (internal/propose). No other command comes in from outside. The leader
// makes its own commands (placements, the ingress), and no host may speak
// for another host or for the leader.
func mayPropose(identity pki.Identity, cmd state.Command) bool {
	itself := cmd.Actor == actorOf(identity)
	controlPlane := identity.HostRoles.Has(hostrole.ControlPlane)
	// a kind may be in more than one set (set-host-phase is the CLI's and
	// a host's own); the actor decides which set applies
	switch {
	case state.IsCLIActor(cmd.Actor) && cliKinds[cmd.Kind]:
		return controlPlane
	case hostKinds[cmd.Kind]:
		return itself
	case controlPlaneKinds[cmd.Kind]:
		return itself && controlPlane
	}
	return false
}

// TLSConfig is the server's side of mutual TLS. On a control-plane host,
// the handshake checks a client certificate if the client sends one. The
// routes then refuse calls without a certificate, except the routes of
// handleOpen. A host without the control plane accepts only control-plane
// hosts.
func (s *Server) TLSConfig() *tls.Config {
	if !s.config.Roles.Has(hostrole.ControlPlane) {
		return pki.ServerTLSForControlPlane(s.config.Certificate, s.config.CA, s.revoked)
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

// identityKey is where a request's caller is kept in its context.
type identityKey struct{}

// handle registers a route that only the callers of rule may use. Every
// call is checked against the CA, and against the revocation list and the
// CLI cutoff in the state. So every agent refuses a revoked certificate at
// once.
func (s *Server) handle(pattern string, rule caller, handler http.HandlerFunc) {
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

// handleOpen registers a route that callers reach without a certificate.
// This is the join, which proves itself with a join token instead. Only a
// control-plane host serves it, because only a control-plane host lets a
// caller without a certificate through the handshake (TLSConfig).
func (s *Server) handleOpen(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if s.checkVersion(w, r) {
			handler(w, r)
		}
	})
}

// checkVersion refuses a caller whose version is not compatible with this
// agent's (buildinfo.Compatible).
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

// warnAboutDevelopment logs, once per version, that a development build
// talks to another build. Compatible lets such a call through. Only callers
// with a certificate reach this function. On the open routes, anyone could
// fill the log and the map with versions.
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

func (s *Server) revoked(serial string) bool {
	var revoked bool
	s.config.Cluster.Read(func(current *state.State) {
		_, revoked = current.Revoked[serial]
	})
	return revoked
}

// waitForCatchUp holds a CLI request until this agent has caught up after
// its start (ServerConfig.CaughtUp), for at most waitTimeout. Host routes
// do not wait, because the catch-up itself, the beats and the joins use
// them.
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
