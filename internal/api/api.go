// Package api is the agents' HTTP API (v1): the types both sides use, the
// server every agent runs and the client of the CLI. Every call is mutual
// TLS: users present their certificate, agents their host certificate.
// Any agent answers; writes go on to the leader, reads come from the agent's
// own copy of the state.
package api

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// Port is where every agent serves the API (underlay and node address).
const Port = 7443

// Headers of the API.
const (
	// VersionHeader carries the caller's version; agent and CLI refuse each
	// other when they are more than one minor version apart.
	VersionHeader = "X-Clowder-Version"
	// IndexHeader carries the Raft index of a write in its answer.
	IndexHeader = "X-Clowder-Index"
	// MinIndexHeader asks a read to wait until the agent has applied this
	// index, so a read after a write sees the write on any agent.
	MinIndexHeader = "X-Clowder-Min-Index"
)

// Address joins a host and the API port; a seed may name its own port.
func Address(seed string) string {
	if _, _, err := net.SplitHostPort(seed); err == nil {
		return seed
	}
	return net.JoinHostPort(seed, strconv.Itoa(Port))
}

// Error is the body of every answer that is not 2xx.
type Error struct {
	Message string `json:"error"`
}

// StatusError is an answer of the API that is not 2xx.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// Status is the overview of the cluster.
type Status struct {
	Name string `json:"name"`
	// Revision of the applied cluster.yaml; 0 before the first apply.
	Revision int64      `json:"revision"`
	Leader   string     `json:"leader"` // host name; empty without a leader
	Index    uint64     `json:"index"`
	Hosts    []HostInfo `json:"hosts"`
}

// HostInfo is one host as the CLI shows it.
type HostInfo struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Voter    bool   `json:"voter"`
	Underlay string `json:"underlay"`
	Version  string `json:"version,omitempty"`
}

// UserInfo is one user.
type UserInfo struct {
	Name         string `json:"name"`
	Certificates int    `json:"certificates"`
}

// AuditEntry is one applied command.
type AuditEntry struct {
	Index uint64    `json:"index"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
	Kind  string    `json:"kind"`
}

// Written is the answer to a write.
type Written struct {
	Index uint64 `json:"index"`
}
