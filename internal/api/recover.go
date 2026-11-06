package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// RecoverHost is what an agent does for clowderd recover on another host
// (docs/developing/cluster.md, Recover).
type RecoverHost interface {
	// DiskStates returns DRBD's disk state of each resource that is up on
	// this host, by its name (storage.Driver.DiskStates).
	DiskStates(ctx context.Context) (map[string]string, error)
	// Rejoin drops the retired hosts from this host's DRBD resources and
	// restarts the agent, which sets its Raft log aside at its start and
	// follows the new leader as a fresh non-voter. It returns before the
	// restart.
	Rejoin(ctx context.Context, retired []state.HostID) error
}

// HostCopies is a host's answer to RecoverCaller.Copies: DRBD's disk
// state of each copy whose resource is up, by the resource's name
// (names.VolumeName, which holds the data generation).
type HostCopies struct {
	Disks map[string]string `json:"disks"`
}

// RejoinRequest names the hosts that clowderd recover retired.
type RejoinRequest struct {
	Retired []state.HostID `json:"retired"`
}

func (s *Server) localCopies(w http.ResponseWriter, r *http.Request) {
	if s.config.Recover == nil {
		writeError(w, http.StatusNotFound, errors.New("this agent cannot tell its copies"))
		return
	}
	disks, err := s.config.Recover.DiskStates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, HostCopies{Disks: disks})
}

// localRejoin refuses on a control-plane host, whose Raft log may hold the
// only copy of what the cluster committed, and on a host that knows a
// leader: its cluster is not lost, whatever recover found when it asked.
func (s *Server) localRejoin(w http.ResponseWriter, r *http.Request) {
	if s.config.Recover == nil {
		writeError(w, http.StatusNotFound, errors.New("this agent cannot rejoin"))
		return
	}
	var request RejoinRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.config.Roles.Has(hostrole.ControlPlane) {
		writeError(w, http.StatusConflict, fmt.Errorf("a %s host does not rejoin after a recover; it is retired",
			hostrole.ControlPlane))
		return
	}
	if leader, ok := s.config.Cluster.Leader(); ok {
		writeError(w, http.StatusConflict, fmt.Errorf("this host knows a leader, host %d: its cluster is not lost", leader))
		return
	}
	if err := s.config.Recover.Rejoin(r.Context(), request.Retired); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Timeouts of RecoverCaller. A host that accepts no connection within
// recoverDialTimeout counts as not reached. A host that accepted the
// connection has until recoverAnswerTimeout to answer, or until
// rejoinTimeout to drop its peers. Otherwise its answer stays unknown.
const (
	recoverDialTimeout   = 5 * time.Second
	recoverAnswerTimeout = 15 * time.Second
	rejoinTimeout        = 3 * time.Minute
)

// ErrNotReached is RecoverCaller's answer for an agent that accepted no
// connection: it refused the connection or did not accept it in time.
var ErrNotReached = errors.New("not reached")

// RecoverCaller calls other hosts' agents for clowderd recover, over mTLS
// as a host. An address is host:port. Only ErrNotReached means that the
// agent was not reached; any other error leaves the answer unknown.
type RecoverCaller struct {
	client  *http.Client
	version string
}

// NewRecoverCaller returns a caller that authenticates with the host's
// certificate and trusts agents of the CA. It has its own HTTP client
// rather than the API's Client, whose timeouts are meant for a user and
// which cannot tell a failed dial from a later failure.
func NewRecoverCaller(certificate tls.Certificate, ca *x509.Certificate, version string) RecoverCaller {
	dialer := &net.Dialer{Timeout: recoverDialTimeout}
	return RecoverCaller{version: version, client: &http.Client{Transport: &http.Transport{
		DialContext: dialer.DialContext,
		// no revocation list: recover's own may be behind
		TLSClientConfig: pki.ClientTLSToHosts(certificate, ca, nil),
	}}}
}

// KnowsLeader asks whether the agent knows a leader of its cluster.
func (c RecoverCaller) KnowsLeader(ctx context.Context, address string) (bool, error) {
	err := c.call(ctx, address, http.MethodGet, "/v1/internal/leader", nil, &KnownLeader{}, recoverAnswerTimeout)
	var statusError *StatusError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &statusError) && statusError.Status == http.StatusServiceUnavailable:
		return false, nil
	}
	return false, err
}

// Copies asks for the copies on the agent's host (HostCopies).
func (c RecoverCaller) Copies(ctx context.Context, address string) (map[string]string, error) {
	var answer HostCopies
	if err := c.call(ctx, address, http.MethodGet, "/v1/internal/recover/copies", nil, &answer, recoverAnswerTimeout); err != nil {
		return nil, err
	}
	return answer.Disks, nil
}

// Rejoin tells the agent to drop the retired hosts and to follow the new
// leader with a fresh Raft log (RecoverHost.Rejoin).
func (c RecoverCaller) Rejoin(ctx context.Context, address string, retired []state.HostID) error {
	return c.call(ctx, address, http.MethodPost, "/v1/internal/recover/rejoin", RejoinRequest{Retired: retired}, nil,
		rejoinTimeout)
}

func (c RecoverCaller) call(ctx context.Context, address, method, path string, body, out any,
	timeout time.Duration,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://"+address+path, &encoded)
	if err != nil {
		return err
	}
	request.Header.Set(VersionHeader, c.version)
	err = send(c.client, request, out)
	var opError *net.OpError
	if errors.As(err, &opError) && opError.Op == "dial" {
		return fmt.Errorf("%w: %w", ErrNotReached, err)
	}
	return err
}
