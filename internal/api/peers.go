package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/state"
)

// waitTimeout is the one budget of a call that waits: a read that waits
// for its minimum index, or a call that waits for the leader and then asks
// it. An election takes seconds, and a peer that restarts may lose its
// leader for a moment.
const waitTimeout = 10 * time.Second

// leaderPoll is how often a wait for the leader looks again.
const leaderPoll = 50 * time.Millisecond

// HostConfig is what both sides of an agent's API know about its host: the
// Server, which answers, and the Peers, which ask the other agents.
type HostConfig struct {
	Cluster Cluster
	CA      *x509.Certificate
	// Certificate is the host's certificate, for TLS on both sides.
	Certificate tls.Certificate
	// Roles are the host's roles, as its certificate names them (the agent
	// checks that at its start).
	Roles hostrole.Set
	// APIAddress gives a host's API address (its node address and the API
	// port).
	APIAddress func(id state.HostID) string
	// Keys is nil on a host without the control-plane role.
	Keys    Keys
	HostID  state.HostID
	Version string
	Logger  *slog.Logger
}

// Keys is the cluster's data key as the API uses it. It is nil on a host
// without the control-plane role, which holds no data key.
type Keys interface {
	Encrypt(plaintext []byte) (state.Encrypted, error)
	Decrypt(encrypted state.Encrypted) ([]byte, error)
	// Derive gives a separate key for one purpose (the secrets' digests).
	Derive(purpose string) ([]byte, error)
}

// revoked tells whether the state revoked the certificate with this
// serial number.
func (c HostConfig) revoked(serial string) bool {
	var revoked bool
	c.Cluster.Read(func(current *state.State) {
		_, revoked = current.Revoked[serial]
	})
	return revoked
}

// waitForLeader lets a call wait a moment instead of failing during an
// election, or while this agent has just become leader and still applies
// its log (raftnode.Node.IsLeader). It waits until ctx ends, so every
// caller gives ctx a deadline: waitTimeout for the wait and the call to the
// leader together.
func (c HostConfig) waitForLeader(ctx context.Context) (leader state.HostID, here bool, err error) {
	for {
		if c.Cluster.IsLeader() {
			return c.HostID, true, nil
		}
		if id, ok := c.Cluster.Leader(); ok && id != c.HostID {
			return id, false, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, false, errors.New("the cluster has no leader right now")
			}
			return 0, false, fmt.Errorf("wait for a leader: %w", ctx.Err())
		case <-time.After(leaderPoll):
		}
	}
}

// proposeHere stamps the leader's time (Command.At), whichever agent took
// the request.
func (c HostConfig) proposeHere(ctx context.Context, cmd state.Command) (uint64, error) {
	if !c.Cluster.IsLeader() {
		return 0, errors.New("not the leader; try again")
	}
	cmd.At = time.Now().UTC()
	return c.Cluster.Propose(ctx, cmd)
}

// Peers is how this agent asks the other agents, as its host: the leader
// for commits, the secrets of this host and the commit index; the voters
// for beats and silences; any host for its copies' progress. The agent's
// parts use it, and so does the Server for what it forwards.
type Peers struct {
	config HostConfig
	// forwarder asks within waitTimeout. streamer relays answers that may
	// take long (logs that follow, a join), so it has no timeout; the
	// caller's context ends it.
	forwarder *http.Client
	streamer  *http.Client
}

// NewPeers creates the client side of this agent's API.
func NewPeers(config HostConfig) *Peers {
	// a host without the control plane calls only the control plane: the
	// voters with its beats, and the leader with its reports
	toHosts := pki.ClientTLSToHosts
	if !config.Roles.Has(hostrole.ControlPlane) {
		toHosts = pki.ClientTLSToControlPlane
	}
	transport := &http.Transport{TLSClientConfig: toHosts(config.Certificate, config.CA, config.revoked)}
	return &Peers{
		config:    config,
		forwarder: &http.Client{Timeout: waitTimeout, Transport: transport},
		streamer:  &http.Client{Transport: transport},
	}
}

// ask sends a request to a host's API and reads the answer into answer
// (nil for none). A minIndex above 0 makes the host answer only once its
// state holds that index (MinIndexHeader).
func (p *Peers) ask(ctx context.Context, method string, host state.HostID, path string, body []byte, minIndex uint64,
	answer any,
) error {
	request, err := http.NewRequestWithContext(ctx, method, "https://"+p.config.APIAddress(host)+path,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set(VersionHeader, p.config.Version)
	if minIndex > 0 {
		request.Header.Set(MinIndexHeader, strconv.FormatUint(minIndex, 10))
	}
	return send(p.forwarder, request, answer)
}

// Propose commits a command, on the leader or through it. It is the only
// way to commit, for the API's routes as well as for the agent's own
// reports and the leader's controllers.
func (p *Peers) Propose(ctx context.Context, cmd state.Command) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	id, here, err := p.config.waitForLeader(ctx)
	switch {
	case err != nil:
		return 0, err
	case here:
		return p.config.proposeHere(ctx, cmd)
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("encode the command: %w", err)
	}
	var written Written
	if err := p.ask(ctx, http.MethodPost, id, "/v1/internal/propose", body, 0, &written); err != nil {
		// pass the leader's answer back unchanged; a 409 stays a refusal
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

// Beat tells a voter that this host is alive.
func (p *Peers) Beat(ctx context.Context, voter state.HostID) error {
	if err := p.ask(ctx, http.MethodPost, voter, "/v1/internal/beats", nil, 0, nil); err != nil {
		return fmt.Errorf("beat to host %d: %w", voter, err)
	}
	return nil
}

// AskSilence asks a voter how long it has not heard from a host
// (controller.AskSilence).
func (p *Peers) AskSilence(ctx context.Context, voter, host state.HostID) (time.Duration, error) {
	var answer Silence
	if err := p.ask(ctx, http.MethodGet, voter, fmt.Sprintf("/v1/internal/beats/%d", host), nil, 0, &answer); err != nil {
		return 0, fmt.Errorf("ask host %d about host %d: %w", voter, host, err)
	}
	return answer.SilentFor, nil
}

// Progress asks a host how far its copies have synced
// (controller.Progress).
func (p *Peers) Progress(ctx context.Context, host state.HostID) (map[string]int64, error) {
	var progress map[string]int64
	if err := p.ask(ctx, http.MethodGet, host, "/v1/internal/volumes/progress", nil, 0, &progress); err != nil {
		return nil, fmt.Errorf("ask host %d how far its copies are: %w", host, err)
	}
	return progress, nil
}

// CatchUpWithLeader returns once this agent's state holds everything that
// the leader had committed when it was asked. A host that was away (fenced,
// rebooted) starts its stacks only after this returns. A stack that moved
// to another host in the meantime must not start here from an old state.
func (p *Peers) CatchUpWithLeader(ctx context.Context) error {
	asking, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for {
		id, here, err := p.config.waitForLeader(asking)
		switch {
		case err != nil:
			return err
		case here:
			return nil // IsLeader is true only once the barrier applied the log (raftnode.Node)
		}
		var answer CommitIndex
		err = p.ask(asking, http.MethodGet, id, "/v1/internal/commit-index", nil, 0, &answer)
		if err == nil {
			return p.config.Cluster.WaitCommitted(ctx, answer.Index)
		}
		// a leader that has just stepped down answers 503 while this agent
		// still names it; the next leader is asked within the same budget
		var statusError *StatusError
		if !errors.As(err, &statusError) || statusError.Status != http.StatusServiceUnavailable {
			return fmt.Errorf("ask the leader (host %d) for its commit index: %w", id, err)
		}
		select {
		case <-asking.Done():
			return fmt.Errorf("ask the leader (host %d) for its commit index: %w", id, err)
		case <-time.After(leaderPoll):
		}
	}
}

// SecretsOfThisHost returns the secrets that this host may read, by ID
// (state.SecretsOfHost). A control-plane host opens them itself; any other
// host asks the leader.
func (p *Peers) SecretsOfThisHost(ctx context.Context) (map[string]HostSecret, error) {
	if p.config.Keys != nil {
		return p.config.secretsOf(p.config.HostID)
	}
	var answer map[string]HostSecret
	if err := p.askForThisHost(ctx, "/v1/internal/hosts/self/secrets", &answer); err != nil {
		return nil, fmt.Errorf("ask the control plane for this host's secrets: %w", err)
	}
	return answer, nil
}

// CertificatesOfThisHost returns the certificates' keys by domain, while
// this host holds the Floating IP. A control-plane host opens them itself;
// any other host asks the leader.
func (p *Peers) CertificatesOfThisHost(ctx context.Context) (map[string]HostCertificate, error) {
	if p.config.Keys != nil {
		return p.config.certificatesOf(p.config.HostID)
	}
	var answer map[string]HostCertificate
	if err := p.askForThisHost(ctx, "/v1/internal/hosts/self/tls", &answer); err != nil {
		return nil, fmt.Errorf("ask the control plane for the certificates' keys: %w", err)
	}
	return answer, nil
}

// askForThisHost asks the leader, which is a control-plane host. The leader
// waits until its state holds this host's (MinIndexHeader), so that it
// does not judge from an older state what this host may read.
func (p *Peers) askForThisHost(ctx context.Context, path string, answer any) error {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	id, here, err := p.config.waitForLeader(ctx)
	switch {
	case err != nil:
		return err
	case here:
		// a host without the data key never votes, so it never leads
		return errors.New("this host leads but holds no data key; its roles and key files disagree")
	}
	var index uint64
	p.config.Cluster.Read(func(current *state.State) { index = current.Index })
	if err := p.ask(ctx, http.MethodGet, id, path, nil, index, answer); err != nil {
		return fmt.Errorf("the leader (host %d): %w", id, err)
	}
	return nil
}
