package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
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

// commandKind says how Apply runs one kind of command, and which release
// introduced the kind. The leader writes a kind only once every agent can
// read it (State.CheckKnown).
type commandKind struct {
	since string
	apply handler
}

var commandKinds = map[string]commandKind{
	KindBootstrap:            {"0.1.0", decoded(applyBootstrap)},
	KindSetHostPhase:         {"0.1.0", decoded(applySetHostPhase)},
	KindReportHost:           {"0.1.0", decoded(applyReportHost)},
	KindSetIngress:           {"0.1.0", decoded(applySetIngress)},
	KindPutACMEAccount:       {"0.1.0", decoded(applyPutACMEAccount)},
	KindPutACMEChallenge:     {"0.1.0", decoded(applyPutACMEChallenge)},
	KindDeleteACMEChallenge:  {"0.1.0", decoded(applyDeleteACMEChallenge)},
	KindPutCertificate:       {"0.1.0", decoded(applyPutCertificate)},
	KindDeleteCertificate:    {"0.1.0", decoded(applyDeleteCertificate)},
	KindSetCLICutoff:         {"0.2.0", decoded(applySetCLICutoff)},
	KindChangeResources:      {"0.2.0", decoded(applyChangeResources)},
	KindRestartStack:         {"0.1.0", decoded(applyRestartStack)},
	KindSetStackStatus:       {"0.1.0", decoded(applySetStackStatus)},
	KindSetStackWaitReason:   {"0.1.0", decoded(applySetStackWaitReason)},
	KindSetPushedOut:         {"0.1.0", decoded(applySetPushedOut)},
	KindPutAlert:             {"0.1.0", decoded(applyPutAlert)},
	KindResolveAlert:         {"0.1.0", decoded(applyResolveAlert)},
	KindPutVolume:            {"0.1.0", decoded(applyPutVolume)},
	KindForgetCopy:           {"0.1.0", decoded(applyForgetCopy)},
	KindSetVolumeInitialized: {"0.2.0", decoded(applySetVolumeInitialized)},
	KindStartOperation:       {"0.2.0", decoded(applyStartOperation)},
	KindAdvanceOperation:     {"0.2.0", decoded(applyAdvanceOperation)},
	KindFinishOperation:      {"0.2.0", decoded(applyFinishOperation)},
	KindAddJoinToken:         {"0.2.0", decoded(applyAddJoinToken)},
	KindAddHost:              {"0.2.0", decoded(applyAddHost)},
	KindConfirmServer:        {"0.2.0", decoded(applyConfirmServer)},
	KindSetHostVoter:         {"0.2.0", decoded(applySetHostVoter)},
	KindReportPeerForgotten:  {"0.2.0", decoded(applyReportPeerForgotten)},
	KindSetCopySynced:        {"0.2.0", decoded(applySetCopySynced)},
	KindExtendOperation:      {"0.2.0", decoded(applyExtendOperation)},
	KindRollBackOperation:    {"0.2.0", decoded(applyRollBackOperation)},
	KindRequestMove:          {"0.2.0", decoded(applyRequestMove)},
	KindRecoverRemap:         {"0.2.0", decoded(applyRecoverRemap)},
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
	kind, err := readable(cmd)
	if err != nil {
		return nil, fmt.Errorf("the command at index %d: %w", index, err)
	}
	s.Index = index
	s.changes = nil
	if s.Keys == nil && cmd.Kind != KindBootstrap {
		return nil, fmt.Errorf("%s: %w", cmd.Kind, ErrNotBootstrapped)
	}
	s.makeMaps()
	change, err := kind.apply(s, cmd)
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
func readable(cmd Command) (commandKind, error) {
	if cmd.Version < max(1, CommandVersion-1) || cmd.Version > CommandVersion {
		return commandKind{}, fmt.Errorf("%s: command version %d is %w (it reads %d)",
			cmd.Kind, cmd.Version, ErrUnreadable, CommandVersion)
	}
	kind, ok := commandKinds[cmd.Kind]
	if !ok {
		return commandKind{}, fmt.Errorf("unknown command %q: %w", cmd.Kind, ErrUnreadable)
	}
	return kind, nil
}

// ErrNotReadByAll is CheckKnown's error for a kind that some agent of the
// cluster may not read yet. It is not a refusal: the command may commit
// later, once every agent runs the release that introduced the kind.
var ErrNotReadByAll = errors.New("not every agent reads it yet")

// CheckKnown returns ErrNotReadByAll while the oldest agent (MinVersion)
// may not read a kind of command yet, because an agent stops at an entry
// that it cannot read.
func (s *State) CheckKnown(kind string) error {
	known, ok := commandKinds[kind]
	if !ok {
		return fmt.Errorf("unknown command %q", kind)
	}
	if oldest := s.MinVersion(); oldest != "" && !buildinfo.Includes(oldest, known.since) {
		return fmt.Errorf("%s: %w: it came with %s, the oldest agent runs %s",
			kind, ErrNotReadByAll, known.since, oldest)
	}
	return nil
}

// MinVersion is the oldest release that an agent of the cluster runs, or
// empty if no agent counts. Removed hosts, hosts that have not reported a
// version yet, and development builds (which read every kind of their code)
// do not count. A joined host that cannot read a kind written before its
// first report stops at it.
func (s *State) MinVersion() string {
	var oldest string
	for _, host := range s.Hosts {
		if host.Phase == Removed || host.Version == "" || buildinfo.IsDevelopment(host.Version) {
			continue
		}
		if oldest == "" || buildinfo.Compare(host.Version, oldest) < 0 {
			oldest = host.Version
		}
	}
	return oldest
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
