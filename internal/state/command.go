package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CommandVersion is the format of the commands this build writes. Apply also
// reads the version before it: agents of two neighboring releases run side
// by side during an upgrade.
const CommandVersion = 1

// Command is one entry of the Raft log.
type Command struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// At is the leader's clock when it wrote the command; Apply uses it
	// instead of reading the clock, so every agent gets the same state.
	At time.Time `json:"at"`
	// Actor is who asked: LeaderActor, HostActor or UserActor.
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// The kinds of commands. Each has a payload type of the same name.
const (
	KindBootstrap            = "bootstrap"
	KindApplyConfig          = "apply-config"
	KindSetHostPhase         = "set-host-phase"
	KindReportHost           = "report-host"
	KindSetIngress           = "set-ingress"
	KindPutACMEAccount       = "put-acme-account"
	KindPutACMEChallenge     = "put-acme-challenge"
	KindDeleteACMEChallenge  = "delete-acme-challenge"
	KindPutCertificate       = "put-certificate"
	KindDeleteCertificate    = "delete-certificate"
	KindAddUser              = "add-user"
	KindRecoverUser          = "recover-user"
	KindSetRecoveryRecipient = "set-recovery-recipient"
	KindPutResource          = "put-resource"
	KindDeleteResource       = "delete-resource"
	KindRestartStack         = "restart-stack"
	KindSetStackStatus       = "set-stack-status"
	KindSetStackWaitReason   = "set-stack-wait-reason"
	KindSetPushedOut         = "set-pushed-out"
	KindPutAlert             = "put-alert"
	KindResolveAlert         = "resolve-alert"
	KindPutVolume            = "put-volume"
	KindForgetCopy           = "forget-copy"
)

// LeaderActor is the leader acting on its own (its control loops).
const LeaderActor = "leader"

// hostActorPrefix starts the actor of every host.
const hostActorPrefix = "host/"

// IsHostActor tells whether an actor is a host (and not the leader or a user).
func IsHostActor(actor string) bool {
	return strings.HasPrefix(actor, hostActorPrefix)
}

// HostActor is the agent of a host, as its certificate names it.
func HostActor(id HostID) string {
	return hostActorPrefix + strconv.Itoa(int(id))
}

// userActorPrefix starts the actor of every user.
const userActorPrefix = "user/"

// IsUserActor tells whether an actor is a user of the CLI.
func IsUserActor(actor string) bool {
	return strings.HasPrefix(actor, userActorPrefix)
}

// UserActor is a user of the CLI, as their certificate names them.
func UserActor(name string) string {
	return userActorPrefix + name
}

// NewCommand wraps a payload into a command of the current version.
func NewCommand(kind string, at time.Time, actor string, payload any) (Command, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Command{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return Command{Version: CommandVersion, Kind: kind, At: at.UTC(), Actor: actor, Payload: encoded}, nil
}

// handler checks one kind of command against the state and returns the
// change it makes, which Apply runs only for a valid command. The change
// cannot fail: everything that can refuse the command runs before it, in
// the handler, which only reads the state. So a refused command leaves no
// trace, without the cost of working on a copy of the state.
type handler func(s *State, cmd Command) (change func(), err error)

var handlers = map[string]handler{
	KindBootstrap:            decoded(applyBootstrap),
	KindApplyConfig:          decoded(applyConfig),
	KindSetHostPhase:         decoded(applySetHostPhase),
	KindReportHost:           decoded(applyReportHost),
	KindSetIngress:           decoded(applySetIngress),
	KindPutACMEAccount:       decoded(applyPutACMEAccount),
	KindPutACMEChallenge:     decoded(applyPutACMEChallenge),
	KindDeleteACMEChallenge:  decoded(applyDeleteACMEChallenge),
	KindPutCertificate:       decoded(applyPutCertificate),
	KindDeleteCertificate:    decoded(applyDeleteCertificate),
	KindAddUser:              decoded(applyAddUser),
	KindRecoverUser:          decoded(applyRecoverUser),
	KindSetRecoveryRecipient: decoded(applySetRecoveryRecipient),
	KindPutResource:          decoded(applyPutResource),
	KindDeleteResource:       decoded(applyDeleteResource),
	KindRestartStack:         decoded(applyRestartStack),
	KindSetStackStatus:       decoded(applySetStackStatus),
	KindSetStackWaitReason:   decoded(applySetStackWaitReason),
	KindSetPushedOut:         decoded(applySetPushedOut),
	KindPutAlert:             decoded(applyPutAlert),
	KindResolveAlert:         decoded(applyResolveAlert),
	KindPutVolume:            decoded(applyPutVolume),
	KindForgetCopy:           decoded(applyForgetCopy),
}

// decoded turns a function of a typed payload into a handler. Unknown fields
// are ignored: a command of the next release may carry fields this one does
// not know yet.
func decoded[Payload any](apply func(s *State, cmd Command, payload Payload) (func(), error)) handler {
	return func(s *State, cmd Command) (func(), error) {
		var payload Payload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return nil, fmt.Errorf("read the payload: %w", err)
		}
		return apply(s, cmd, payload)
	}
}

// ErrNotBootstrapped is returned for every command but the bootstrap before
// the cluster has one.
var ErrNotBootstrapped = errors.New("the cluster is not bootstrapped yet")

// ApplyEntry applies a Raft log entry: a command as JSON. An entry that does
// not read as one is refused like any other wrong command.
func (s *State) ApplyEntry(index uint64, data []byte) ([]Change, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		s.Index = index
		return nil, fmt.Errorf("read the command at index %d: %w", index, err)
	}
	return s.Apply(index, cmd)
}

// Apply applies the command at the given Raft index and returns the
// resources it changed, each with the index as its ResourceVersion. A
// refused command returns an error and leaves the state as it was, apart
// from the index: the log entry is applied either way.
func (s *State) Apply(index uint64, cmd Command) ([]Change, error) {
	s.Index = index
	s.changes = nil
	if cmd.Version < max(1, CommandVersion-1) || cmd.Version > CommandVersion {
		return nil, fmt.Errorf("%s: command version %d is not readable by this agent (%d)",
			cmd.Kind, cmd.Version, CommandVersion)
	}
	apply, ok := handlers[cmd.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown command %q", cmd.Kind)
	}
	if s.Keys == nil && cmd.Kind != KindBootstrap {
		return nil, fmt.Errorf("%s: %w", cmd.Kind, ErrNotBootstrapped)
	}
	s.makeMaps()
	change, err := apply(s, cmd)
	switch {
	case errors.Is(err, errUnchanged):
		// no audit entry: a GitOps loop applies the same files every few
		// minutes, and would push the real changes out
		return nil, nil
	case err != nil:
		return nil, &CommandError{Kind: cmd.Kind, Err: err}
	}
	change()
	s.Audit = append(s.Audit, AuditEntry{Index: index, At: cmd.At, Actor: cmd.Actor, Kind: cmd.Kind})
	if extra := len(s.Audit) - auditSize; extra > 0 {
		s.Audit = slices.Delete(s.Audit, 0, extra)
	}
	changes := s.changes
	s.changes = nil
	return changes, nil
}

// CommandError is a command that its handler refused: the command's kind,
// and why. The kind comes first in the message, for the logs; the API
// shows users the reason alone, as they named a resource, not a command.
type CommandError struct {
	Kind string
	Err  error
}

func (e *CommandError) Error() string { return e.Kind + ": " + e.Err.Error() }

func (e *CommandError) Unwrap() error { return e.Err }

// errUnchanged is a handler's answer to a command that would write what is
// there already: Apply takes it as applied, and the command leaves no trace,
// not even in the audit.
var errUnchanged = errors.New("unchanged")

// makeMaps creates the maps that are still nil (a fresh state, or one read
// from a snapshot that left empty maps out), so handlers can write to them.
func (s *State) makeMaps() {
	if s.ClusterSecrets == nil {
		s.ClusterSecrets = map[string]SealedSecret{}
	}
	if s.Secrets == nil {
		s.Secrets = map[string]*Secret{}
	}
	if s.Hosts == nil {
		s.Hosts = map[HostID]*Host{}
	}
	if s.Alerts == nil {
		s.Alerts = map[string]*Alert{}
	}
	if s.Challenges == nil {
		s.Challenges = map[string]string{}
	}
	if s.Certificates == nil {
		s.Certificates = map[string]*Certificate{}
	}
	if s.Users == nil {
		s.Users = map[string]*User{}
	}
	if s.Revoked == nil {
		s.Revoked = map[string]Revocation{}
	}
	if s.UsedCodes == nil {
		s.UsedCodes = map[string]time.Time{}
	}
	if s.Namespaces == nil {
		s.Namespaces = map[string]*Namespace{}
	}
	if s.Stacks == nil {
		s.Stacks = map[string]*Stack{}
	}
	if s.Volumes == nil {
		s.Volumes = map[string]*Volume{}
	}
}

// Refused carries why Apply refused a command, so the one who proposed it can
// tell a refusal (the command is wrong) from a command that did not commit
// (no leader, no quorum). The Raft member wraps Apply's error in it, which
// is usually a CommandError: the same refusal, seen by the proposer.
type Refused struct {
	Err error
}

func (r *Refused) Error() string { return r.Err.Error() }

func (r *Refused) Unwrap() error { return r.Err }
