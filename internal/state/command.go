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

// CommandVersion is the format of the commands that this build writes.
// Apply also reads the version before it.
const CommandVersion = 1

// Command is one entry of the Raft log.
type Command struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// At is the leader's clock when it wrote the command. Apply uses it
	// instead of reading the clock, so every agent reaches the same state.
	At time.Time `json:"at"`
	// Actor is who asked: LeaderActor, HostActor or CLIActor (cli/<name>).
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// The kinds of commands. Each has a payload type of the same name.
const (
	KindBootstrap            = "bootstrap"
	KindSetHostPhase         = "set-host-phase"
	KindReportHost           = "report-host"
	KindSetIngress           = "set-ingress"
	KindPutACMEAccount       = "put-acme-account"
	KindPutACMEChallenge     = "put-acme-challenge"
	KindDeleteACMEChallenge  = "delete-acme-challenge"
	KindPutCertificate       = "put-certificate"
	KindDeleteCertificate    = "delete-certificate"
	KindSetCLICutoff         = "set-cli-cutoff"
	KindChangeResources      = "change-resources"
	KindRestartStack         = "restart-stack"
	KindSetStackStatus       = "set-stack-status"
	KindSetStackWaitReason   = "set-stack-wait-reason"
	KindSetPushedOut         = "set-pushed-out"
	KindPutAlert             = "put-alert"
	KindResolveAlert         = "resolve-alert"
	KindPutVolume            = "put-volume"
	KindForgetCopy           = "forget-copy"
	KindSetVolumeInitialized = "set-volume-initialized"
	KindStartOperation       = "start-operation"
	KindAdvanceOperation     = "advance-operation"
	KindFinishOperation      = "finish-operation"
	KindAddJoinToken         = "add-join-token"
	KindAddHost              = "add-host"
	KindConfirmServer        = "confirm-server"
	KindSetHostVoter         = "set-host-voter"
	KindReportPeerForgotten  = "report-peer-forgotten"
	KindSetCopySynced        = "set-copy-synced"
	KindExtendOperation      = "extend-operation"
	KindRollBackOperation    = "roll-back-operation"
	KindRequestMove          = "request-move"
	KindRecoverRemap         = "recover-remap"
)

// LeaderActor is the leader acting on its own (its control loops).
const LeaderActor = "leader"

const hostActorPrefix = "host/"

// IsHostActor tells whether an actor is a host (and not the leader or the
// CLI).
func IsHostActor(actor string) bool {
	return strings.HasPrefix(actor, hostActorPrefix)
}

// HostActor returns the actor of a host's agent, as its certificate names
// it.
func HostActor(id HostID) string {
	return hostActorPrefix + strconv.Itoa(int(id))
}

// NewCommand wraps a payload into a command of the current version.
func NewCommand(kind string, at time.Time, actor string, payload any) (Command, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Command{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return Command{Version: CommandVersion, Kind: kind, At: at.UTC(), Actor: actor, Payload: encoded}, nil
}

// handler checks one kind of command and only reads the state. It returns
// the change as a function that cannot fail, so a refused command leaves no
// trace (docs/developing/state.md, Rules).
type handler func(s *State, cmd Command) (change func(), err error)

// commandKinds are the kinds of commands that this build reads, each with
// its handler. A patch release adds no kind (docs/developing/state.md,
// Versions).
var commandKinds = map[string]handler{
	KindBootstrap:            decoded(applyBootstrap),
	KindSetHostPhase:         decoded(applySetHostPhase),
	KindReportHost:           decoded(applyReportHost),
	KindSetIngress:           decoded(applySetIngress),
	KindPutACMEAccount:       decoded(applyPutACMEAccount),
	KindPutACMEChallenge:     decoded(applyPutACMEChallenge),
	KindDeleteACMEChallenge:  decoded(applyDeleteACMEChallenge),
	KindPutCertificate:       decoded(applyPutCertificate),
	KindDeleteCertificate:    decoded(applyDeleteCertificate),
	KindSetCLICutoff:         decoded(applySetCLICutoff),
	KindChangeResources:      decoded(applyChangeResources),
	KindRestartStack:         decoded(applyRestartStack),
	KindSetStackStatus:       decoded(applySetStackStatus),
	KindSetStackWaitReason:   decoded(applySetStackWaitReason),
	KindSetPushedOut:         decoded(applySetPushedOut),
	KindPutAlert:             decoded(applyPutAlert),
	KindResolveAlert:         decoded(applyResolveAlert),
	KindPutVolume:            decoded(applyPutVolume),
	KindForgetCopy:           decoded(applyForgetCopy),
	KindSetVolumeInitialized: decoded(applySetVolumeInitialized),
	KindStartOperation:       decoded(applyStartOperation),
	KindAdvanceOperation:     decoded(applyAdvanceOperation),
	KindFinishOperation:      decoded(applyFinishOperation),
	KindAddJoinToken:         decoded(applyAddJoinToken),
	KindAddHost:              decoded(applyAddHost),
	KindConfirmServer:        decoded(applyConfirmServer),
	KindSetHostVoter:         decoded(applySetHostVoter),
	KindReportPeerForgotten:  decoded(applyReportPeerForgotten),
	KindSetCopySynced:        decoded(applySetCopySynced),
	KindExtendOperation:      decoded(applyExtendOperation),
	KindRollBackOperation:    decoded(applyRollBackOperation),
	KindRequestMove:          decoded(applyRequestMove),
	KindRecoverRemap:         decoded(applyRecoverRemap),
}

// decoded turns a function that takes a typed payload into a handler. It
// ignores unknown fields, because a command of the next release may carry
// fields that this release does not know yet.
func decoded[Payload any](apply func(s *State, cmd Command, payload Payload) (func(), error)) handler {
	return func(s *State, cmd Command) (func(), error) {
		var payload Payload
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return nil, fmt.Errorf("read the payload: %w", err)
		}
		return apply(s, cmd, payload)
	}
}

// ErrNotBootstrapped is returned for every command except the bootstrap
// while the cluster has not been bootstrapped.
var ErrNotBootstrapped = errors.New("the cluster is not bootstrapped yet")

// ErrUnreadable marks a log entry or a snapshot that this agent cannot
// read. It is not a refusal: an agent that skipped the entry would differ
// from the others for good, so the state stays as it is, including its
// index, and the agent stops (raftnode.Node.Halted).
var ErrUnreadable = errors.New("not readable by this agent")

// ApplyEntry applies a Raft log entry: a command as JSON.
func (s *State) ApplyEntry(index uint64, data []byte) ([]Change, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return nil, fmt.Errorf("the command at index %d: %w: %w", index, ErrUnreadable, err)
	}
	return s.Apply(index, cmd)
}

// Apply applies the command at the given Raft index and returns the
// resources it changed, each with the index as its ResourceVersion. A
// refused command returns an error and leaves the state as it was, except
// for the index, because the log entry counts as applied either way, and
// for nil maps made empty, which no snapshot or reader can tell apart. A
// command that this agent cannot read (ErrUnreadable) leaves the index
// unchanged too.
func (s *State) Apply(index uint64, cmd Command) ([]Change, error) {
	apply, err := readable(cmd)
	if err != nil {
		return nil, fmt.Errorf("the command at index %d: %w", index, err)
	}
	s.Index = index
	s.changes = nil
	if s.Keys == nil && cmd.Kind != KindBootstrap {
		return nil, fmt.Errorf("%s: %w", cmd.Kind, ErrNotBootstrapped)
	}
	s.makeMaps()
	change, err := apply(s, cmd)
	switch {
	case errors.Is(err, errUnchanged):
		// no audit entry: a GitOps loop applies the same files every few
		// minutes, and its entries would push the real changes out
		// of the audit
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

// readable returns how to apply a command whose version and kind this agent
// reads, or else an ErrUnreadable.
func readable(cmd Command) (handler, error) {
	if cmd.Version < max(1, CommandVersion-1) || cmd.Version > CommandVersion {
		return nil, fmt.Errorf("%s: command version %d is %w (it reads %d)",
			cmd.Kind, cmd.Version, ErrUnreadable, CommandVersion)
	}
	apply, ok := commandKinds[cmd.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown command %q: %w", cmd.Kind, ErrUnreadable)
	}
	return apply, nil
}

// CheckKnown returns an error for a kind of command that this build does
// not read. The leader checks before it writes, because every agent stops
// at an entry that it cannot read.
func CheckKnown(kind string) error {
	if _, ok := commandKinds[kind]; !ok {
		return fmt.Errorf("unknown command %q", kind)
	}
	return nil
}

// CommandError is a command that its handler refused. It holds the
// command's kind and the reason. The message starts with the kind, for the
// logs. The API shows users only the reason, because they named a resource,
// not a command.
type CommandError struct {
	Kind string
	Err  error
}

func (e *CommandError) Error() string { return e.Kind + ": " + e.Err.Error() }

func (e *CommandError) Unwrap() error { return e.Err }

// errUnchanged is a handler's answer to a command that would write what is
// already there. Apply treats the command as applied and leaves no trace,
// not even in the audit.
var errUnchanged = errors.New("unchanged")

// makeMaps creates the maps that are still nil, so handlers can write to
// them. Maps are nil in a fresh state, or in one read from a snapshot that
// left empty maps out.
func (s *State) makeMaps() {
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
	if s.Revoked == nil {
		s.Revoked = map[string]Revocation{}
	}
	if s.RetiredMinors == nil {
		s.RetiredMinors = map[int]string{}
	}
	if s.Forgetting == nil {
		s.Forgetting = map[HostID][]HostID{}
	}
	if s.JoinTokens == nil {
		s.JoinTokens = map[string]JoinToken{}
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
	if s.Operations == nil {
		s.Operations = map[uint64]*Operation{}
	}
	if s.Locks == nil {
		s.Locks = map[string]uint64{}
	}
}

// Refused carries the reason why Apply refused a command. With it, the
// proposer can tell a refusal (the command is wrong) from a command that
// did not commit (no leader, no quorum). The Raft member wraps Apply's
// error in it. That error is usually a CommandError, so the proposer sees
// the same refusal as Apply.
type Refused struct {
	Err error
}

func (r *Refused) Error() string { return r.Err.Error() }

func (r *Refused) Unwrap() error { return r.Err }
