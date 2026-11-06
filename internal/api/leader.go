package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// KnownLeader is an agent's answer to which host leads its cluster.
type KnownLeader struct {
	Leader state.HostID `json:"leader"`
}

// knownLeader answers with the leader that this agent knows: itself once a
// majority has confirmed its lead, or, on a follower, the leader it hears.
// It answers 503 while it knows none. clowderd recover asks it
// (KnowsLeader). A follower may name a leader that is gone until its
// heartbeat times out. recover then refuses, which is the safe side.
func (s *Server) knownLeader(w http.ResponseWriter, r *http.Request) {
	if s.config.Cluster.IsLeader() {
		if err := s.config.Cluster.VerifyLeader(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, KnownLeader{Leader: s.config.HostID})
		return
	}
	leader, ok := s.config.Cluster.Leader()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("no leader known"))
		return
	}
	writeJSON(w, http.StatusOK, KnownLeader{Leader: leader})
}

// Timeouts of KnowsLeader. A host that accepts no connection within
// leaderDialTimeout counts as not reached. A host that accepted it has
// until leaderAnswerTimeout to answer; otherwise what it knows stays
// unknown.
const (
	leaderDialTimeout   = 5 * time.Second
	leaderAnswerTimeout = 15 * time.Second
)

// ErrNotReached is KnowsLeader's answer for an agent that accepted no
// connection: it refused the connection or did not answer it in time.
var ErrNotReached = errors.New("not reached")

// KnowsLeader asks the agent at address (host:port, over mTLS as a host)
// whether it knows a leader of its cluster, either itself or another host.
// If it does, its cluster is not lost. clowderd recover asks every other
// host before it retires them, and goes on only for the hosts that know no
// leader or are not reached (ErrNotReached). Any other failure, such as a
// refused certificate, another version or an answer that does not come,
// leaves the answer unknown. The question ignores revocations: recover
// runs on a host whose own list may be behind, and only reads the answer.
func KnowsLeader(ctx context.Context, address string, certificate tls.Certificate, ca *x509.Certificate,
	version string,
) (bool, error) {
	dialer := &net.Dialer{Timeout: leaderDialTimeout}
	// not the API's Client: its timeouts are made for a user, and it could
	// not tell a failed dial from a later failure
	client := &http.Client{Timeout: leaderAnswerTimeout, Transport: &http.Transport{
		DialContext: dialer.DialContext,
		// no revocation list: recover's own may be behind
		TLSClientConfig: pki.ClientTLSToHosts(certificate, ca, nil),
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/v1/internal/leader", nil)
	if err != nil {
		return false, err
	}
	request.Header.Set(VersionHeader, version)
	err = send(client, request, &KnownLeader{})
	var statusError *StatusError
	var opError *net.OpError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &statusError) && statusError.Status == http.StatusServiceUnavailable:
		return false, nil
	case errors.As(err, &opError) && opError.Op == "dial":
		return false, fmt.Errorf("%w: %w", ErrNotReached, err)
	}
	return false, err
}
