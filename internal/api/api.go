// Package api is the agents' HTTP API (v1): the types that both sides use,
// the server that every agent runs, and the CLI's client. Every call uses
// mutual TLS: the CLI presents its certificate, and agents present their
// host certificate. Any control-plane agent answers the CLI. It passes
// writes on to the leader and answers reads from its own copy of the state.
package api

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/state"
)

// Port is the port on which every agent serves the API.
const Port = 7443

// Headers of the API.
const (
	// VersionHeader carries the caller's version. An agent refuses a caller
	// whose version is not compatible with its own (buildinfo.Compatible).
	VersionHeader = "X-Clowder-Version"
	// IndexHeader carries the Raft index of a write in its answer.
	IndexHeader = "X-Clowder-Index"
	// MinIndexHeader asks a read to wait until the agent has applied this
	// index, so a read after a write sees the write on any agent.
	MinIndexHeader = "X-Clowder-Min-Index"
)

// Address joins an endpoint and the API port. An endpoint that names its
// own port stays as it is.
func Address(endpoint string) string {
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		return endpoint
	}
	return net.JoinHostPort(endpoint, strconv.Itoa(Port))
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
	// Generation of the ClusterConfiguration; 0 before the first apply.
	Generation int64      `json:"generation"`
	Leader     string     `json:"leader"` // host name; empty without a leader
	Index      uint64     `json:"index"`
	Hosts      []HostInfo `json:"hosts"`
	// Alerts are the open alerts, oldest first.
	Alerts []AlertInfo `json:"alerts,omitempty"`
}

// AlertInfo is one open alert.
type AlertInfo struct {
	Key      string    `json:"key"`
	Severity string    `json:"severity"`
	Subject  string    `json:"subject"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

// AlertTestResult says whether one channel accepted the test alert. Error
// is empty if it did.
type AlertTestResult struct {
	Channel string `json:"channel"`
	Error   string `json:"error,omitempty"`
}

// AlertFailure is an alert message that a channel has not accepted yet.
// The leader keeps trying.
type AlertFailure struct {
	Channel   string `json:"channel"`
	Key       string `json:"key"`
	Event     string `json:"event"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// HostInfo is one host as the CLI shows it.
type HostInfo struct {
	ID       int          `json:"id"`
	Name     string       `json:"name"`
	Roles    hostrole.Set `json:"roles"`
	Phase    string       `json:"phase"`
	Voter    bool         `json:"voter"`
	Underlay string       `json:"underlay"`
	// PublicAddress is where the CLI reaches the host's API.
	PublicAddress string `json:"public_address"`
	Version       string `json:"version,omitempty"`
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

// HostActionAnswer is the answer to a host action. Leaving says that a
// removed host still has stacks or volume copies. The host drains, they
// move to other hosts, and then the host is removed.
type HostActionAnswer struct {
	Written
	Leaving bool `json:"leaving,omitempty"`
}

// MoveRequest asks to move a stack to a host, by its name.
type MoveRequest struct {
	To string `json:"to"`
}

// HostAction is something a user does to a host. POST
// /v1/hosts/{id}/{name} moves the host from one of the phases in From (any
// phase if From is empty) to To. The CLI builds its host subcommands from
// this list.
type HostAction struct {
	Name        string
	Description string
	From        []state.HostPhase
	To          state.HostPhase
	// Final actions cannot be undone; the CLI asks before it sends one.
	Final bool
}

// HostActions are the actions on hosts.
var HostActions = []HostAction{
	{
		Name:        "drain",
		Description: "Move a host's stacks to other hosts and keep new ones off; its volume copies stay",
		From:        []state.HostPhase{state.Active},
		To:          state.Draining,
	},
	{
		Name:        "undrain",
		Description: "Let a drained host take new stacks again",
		From:        []state.HostPhase{state.Draining},
		To:          state.Active,
	},
	{
		Name: "fence-confirm",
		Description: "Confirm that a failed host is off, when the leader cannot power it off: its stacks start " +
			"elsewhere, and the leader powers it on again",
		From:  []state.HostPhase{state.Failed},
		To:    state.Fenced,
		Final: true,
	},
	{
		Name:        "enable",
		Description: "Let a host that was fenced too often come back: the leader powers it on again",
		From:        []state.HostPhase{state.Disabled},
		To:          state.Fenced,
	},
	{
		Name:        "remove",
		Description: "Remove a host from the cluster for good, once its stacks and copies moved off; its certificate is revoked",
		To:          state.Removed,
		Final:       true,
	},
}
