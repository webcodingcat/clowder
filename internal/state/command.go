package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	KindBootstrap        = "bootstrap"
	KindApplyConfig      = "apply-config"
	KindAddHost          = "add-host"
	KindSetHostPhase     = "set-host-phase"
	KindReportVersion    = "report-version"
	KindAddUser          = "add-user"
	KindRemoveUser       = "remove-user"
	KindRevoke           = "revoke"
	KindPutStack         = "put-stack"
	KindDeleteStack      = "delete-stack"
	KindSetStackStatus   = "set-stack-status"
	KindPutVolume        = "put-volume"
	KindDeleteVolume     = "delete-volume"
	KindStartOperation   = "start-operation"
	KindAdvanceOperation = "advance-operation"
	KindFinishOperation  = "finish-operation"
)

// LeaderActor is the leader acting on its own (its control loops).
const LeaderActor = "leader"

// HostActor is the agent of a host, as its certificate names it.
func HostActor(id HostID) string {
	return "host/" + strconv.Itoa(int(id))
}

// UserActor is a user of the CLI, as their certificate names them.
func UserActor(name string) string {
	return "user/" + name
}

// NewCommand wraps a payload into a command of the current version.
func NewCommand(kind string, at time.Time, actor string, payload any) (Command, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Command{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return Command{Version: CommandVersion, Kind: kind, At: at.UTC(), Actor: actor, Payload: encoded}, nil
}

// handler applies one kind of command. It checks everything first and
// changes the state only when the command is valid, so a refused command
// leaves no trace.
type handler func(s *State, cmd Command) error

var handlers = map[string]handler{
	KindBootstrap:        decoded(applyBootstrap),
	KindApplyConfig:      decoded(applyConfig),
	KindAddHost:          decoded(applyAddHost),
	KindSetHostPhase:     decoded(applySetHostPhase),
	KindReportVersion:    decoded(applyReportVersion),
	KindAddUser:          decoded(applyAddUser),
	KindRemoveUser:       decoded(applyRemoveUser),
	KindRevoke:           decoded(applyRevoke),
	KindPutStack:         decoded(applyPutStack),
	KindDeleteStack:      decoded(applyDeleteStack),
	KindSetStackStatus:   decoded(applySetStackStatus),
	KindPutVolume:        decoded(applyPutVolume),
	KindDeleteVolume:     decoded(applyDeleteVolume),
	KindStartOperation:   decoded(applyStartOperation),
	KindAdvanceOperation: decoded(applyAdvanceOperation),
	KindFinishOperation:  decoded(applyFinishOperation),
}

// decoded turns a function of a typed payload into a handler. Unknown fields
// are ignored: a command of the next release may carry fields this one does
// not know yet.
func decoded[Payload any](apply func(s *State, cmd Command, payload Payload) error) handler {
	return func(s *State, cmd Command) error {
		var payload Payload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return fmt.Errorf("read the payload: %w", err)
		}
		return apply(s, cmd, payload)
	}
}

// ErrNotBootstrapped is returned for every command but the bootstrap before
// the cluster has one.
var ErrNotBootstrapped = errors.New("the cluster is not bootstrapped yet")

// Apply applies the command at the given Raft index. A refused command
// returns an error and leaves the state as it was, apart from the index:
// the log entry is applied either way.
func (s *State) Apply(index uint64, cmd Command) error {
	s.Index = index
	if cmd.Version < CommandVersion-1 || cmd.Version > CommandVersion {
		return fmt.Errorf("%s: command version %d is not readable by this agent (%d)",
			cmd.Kind, cmd.Version, CommandVersion)
	}
	apply, ok := handlers[cmd.Kind]
	if !ok {
		return fmt.Errorf("unknown command %q", cmd.Kind)
	}
	if s.Keys == nil && cmd.Kind != KindBootstrap {
		return fmt.Errorf("%s: %w", cmd.Kind, ErrNotBootstrapped)
	}
	s.makeMaps()
	if err := apply(s, cmd); err != nil {
		return fmt.Errorf("%s: %w", cmd.Kind, err)
	}
	s.Audit = append(s.Audit, AuditEntry{Index: index, At: cmd.At, Actor: cmd.Actor, Kind: cmd.Kind})
	if extra := len(s.Audit) - auditSize; extra > 0 {
		s.Audit = slices.Delete(s.Audit, 0, extra)
	}
	return nil
}

// makeMaps creates the maps that are still nil (a fresh state, or one read
// from a snapshot that left empty maps out), so handlers can write to them.
func (s *State) makeMaps() {
	if s.Secrets == nil {
		s.Secrets = map[string]Sealed{}
	}
	if s.Registries == nil {
		s.Registries = map[string]Sealed{}
	}
	if s.Hosts == nil {
		s.Hosts = map[HostID]*Host{}
	}
	if s.Users == nil {
		s.Users = map[string]*User{}
	}
	if s.Revoked == nil {
		s.Revoked = map[string]time.Time{}
	}
	if s.Stacks == nil {
		s.Stacks = map[string]*Stack{}
	}
	if s.Volumes == nil {
		s.Volumes = map[string]*Volume{}
	}
	if s.Operations == nil {
		s.Operations = map[string]*Operation{}
	}
	if s.Locks == nil {
		s.Locks = map[string]string{}
	}
}
