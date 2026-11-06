package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/hostrole"
	"github.com/webcodingcat/clowder/internal/resource"
)

// Operation is a long task in steps, such as moving a stack or fencing a
// host. While it runs, it holds locks on what it changes, and nothing else
// may change those things (docs/developing/state.md, Operations and locks).
type Operation struct {
	// ID is the Raft index of the command that started the operation. It
	// is unique, and IDs sort by start.
	ID   uint64 `json:"id"`
	Kind string `json:"kind"`
	// Locks are what the operation changes: StackLock, VolumeLock,
	// HostLock.
	Locks []string `json:"locks"`
	// Step is the last step done, or 0 before the first.
	Step int `json:"step"`
	// Owner runs the next step: a host, or nil for the leader.
	Owner *HostID `json:"owner,omitempty"`
	// Deadline is when the next step must be done (the leader's clock).
	Deadline time.Time  `json:"deadline,omitzero"`
	State    OpState    `json:"state"`
	Error    string     `json:"error,omitempty"`
	Move     *StackMove `json:"move,omitempty"`
	Fence    *HostFence `json:"fence,omitempty"`
}

// OpState says where an operation stands.
type OpState string

// The states of an operation. An operation that rolls back waits only for
// the leader to undo its work. Then it fails.
const (
	OpRunning     OpState = "running"
	OpRollingBack OpState = "rolling-back"
	OpDone        OpState = "done"
	OpFailed      OpState = "failed"
)

// The kinds of operations: OperationMoveStack moves a stack to another host
// (StackMove), OperationFenceHost powers a failed host off (HostFence).
const (
	OperationMoveStack = "move-stack"
	OperationFenceHost = "fence-host"
)

// StackMove is what a move-stack operation moves: a stack with its volumes,
// from its host to another (docs/developing/state.md, Moving a stack).
// MoveStopped is the point of no return: before it, a rollback leaves the
// stack on From; after it, only To can run the stack.
type StackMove struct {
	Stack string `json:"stack"` // the stack's ID
	// From is the stack's host when the move started. It is nil for a
	// stack that had no host and waits for its volumes to come to To. Such
	// a move has no stack to stop, so the new host goes from MoveCopies
	// straight to MoveStopped.
	From *HostID `json:"from,omitempty"`
	To   HostID  `json:"to"`
}

// The steps of a move-stack operation (StackMove). MoveStart is the step
// before the first.
const (
	MoveStart   = 0
	MoveCopies  = 1
	MoveSynced  = 2
	MoveStopped = 3
	MoveHost    = 4
)

// MoveStepName names the last step done of a move, for messages.
func MoveStepName(step int) string {
	switch step {
	case MoveStart:
		return "its start"
	case MoveCopies:
		return "placing the copies on the new host"
	case MoveSynced:
		return "the copies synced on the new host"
	case MoveStopped:
		return "the stack stopped on its old host"
	case MoveHost:
		return "giving the stack its new host"
	default:
		return fmt.Sprintf("step %d", step)
	}
}

// HostFence is what a fence-host operation fences: a failed host, whose
// server the leader powers off before its stacks start elsewhere
// (docs/developing/state.md, Fencing a host).
type HostFence struct {
	Host HostID `json:"host"`
	// Stacks are the stacks that were on the host when it was fenced, which
	// the leader waits for to run elsewhere. The command that moves the host
	// to fenced records them.
	Stacks []string `json:"stacks,omitempty"`
}

// The steps of a fence-host operation (HostFence).
const (
	FenceStart = 0
	FenceOff   = 1
)

// FenceOf returns the fence-host operation that holds the host's lock, if
// one does and has not finished.
func (s *State) FenceOf(id HostID) *Operation {
	operation, ok := s.Operations[s.Locks[HostLock(id)]]
	if !ok || operation.Fence == nil || operation.Fence.Host != id {
		return nil
	}
	return operation
}

// HostLocked tells whether an operation holds the host's lock.
func (s *State) HostLocked(id HostID) bool {
	return s.Locks[HostLock(id)] != 0
}

// checkFence ties a fence-host operation to its failed host. The host's
// server must be checked, because Hetzner powers off the server that
// ServerID names.
func (s *State) checkFence(payload StartOperation) error {
	fence := payload.Fence
	switch {
	case (payload.Kind == OperationFenceHost) != (fence != nil):
		return fmt.Errorf("operation %s: a fence goes with %s, and only with it", payload.Kind, OperationFenceHost)
	case fence == nil:
		return nil
	case !slices.Contains(payload.Locks, HostLock(fence.Host)):
		return fmt.Errorf("a fence of host %d must lock it", fence.Host)
	case len(fence.Stacks) > 0:
		return errors.New("a fence starts without stacks; the state records them as the host is fenced")
	}
	host, ok := s.Hosts[fence.Host]
	switch {
	case !ok:
		return fmt.Errorf("no host %d", fence.Host)
	case host.Phase != Failed:
		return fmt.Errorf("host %s is %s, not failed", host.Name, host.Phase)
	case !host.ServerChecked:
		return fmt.Errorf("host %s has no checked server", host.Name)
	}
	return nil
}

// StepTime is how long a step of an operation has, unless its sender gives
// a deadline of its own. A step that waits for a resync gets more time.
const StepTime = 10 * time.Minute

func deadline(cmd Command, given time.Time) time.Time {
	if given.IsZero() {
		return cmd.At.Add(StepTime)
	}
	return given
}

// keptFinished is how many finished operations the state keeps for users
// to look at.
const keptFinished = 100

// The locks an operation can take: on a stack and a volume by their IDs,
// and on a host by its ID, for fencing, drain and remove.
const (
	stackLockPrefix  = "stack/"
	volumeLockPrefix = "volume/"
	hostLockPrefix   = "host/"
)

// StackLock is the lock on a stack, by its ID.
func StackLock(id string) string { return stackLockPrefix + id }

// VolumeLock is the lock on a volume, by its ID.
func VolumeLock(id string) string { return volumeLockPrefix + id }

// HostLock is the lock on a host, by its ID.
func HostLock(id HostID) string { return hostLockPrefix + strconv.Itoa(int(id)) }

// MoveOf returns the move-stack operation that holds the stack's lock, if
// one does and has not finished.
func (s *State) MoveOf(stackID string) *Operation {
	operation, ok := s.Operations[s.Locks[StackLock(stackID)]]
	if !ok || operation.Move == nil || operation.Move.Stack != stackID {
		return nil
	}
	return operation
}

// StopsForMove tells whether the stack's old host stops the stack for a
// move: from MoveSynced until the stack has the new host. A move that rolls
// back leaves the stack running where it was.
func (s *State) StopsForMove(stackID string) bool {
	operation := s.MoveOf(stackID)
	return operation != nil && operation.State == OpRunning &&
		operation.Step >= MoveSynced && operation.Step < MoveHost
}

// checkLockable accepts a lock on a stack or a volume that the state has,
// or on a host that is a member. A lock on nothing would protect nothing.
func (s *State) checkLockable(lock string) error {
	if id, ok := strings.CutPrefix(lock, stackLockPrefix); ok {
		if _, ok := s.Stacks[id]; ok {
			return nil
		}
		return fmt.Errorf("lock %s: no stack %s", lock, id)
	}
	if id, ok := strings.CutPrefix(lock, volumeLockPrefix); ok {
		if _, ok := s.Volumes[id]; ok {
			return nil
		}
		return fmt.Errorf("lock %s: no volume %s", lock, id)
	}
	if id, ok := strings.CutPrefix(lock, hostLockPrefix); ok {
		number, err := strconv.Atoi(id)
		if err != nil {
			return fmt.Errorf("lock %s: %q is no host ID", lock, id)
		}
		host := HostID(number)
		return s.checkLiveHost(&host)
	}
	return fmt.Errorf("lock %q is none of stack/<stack ID>, volume/<volume ID>, host/<host ID>", lock)
}

// checkUnlocked refuses a change to what an operation holds, unless that
// operation makes the change. operation is the operation that sends the
// change, or 0 for none.
func (s *State) checkUnlocked(lock string, operation uint64) error {
	if holder, held := s.Locks[lock]; held && holder != operation {
		return fmt.Errorf("%s is locked by operation %d (%s); try again once it is finished",
			lock, holder, s.Operations[holder].Kind)
	}
	return nil
}

// StartOperation starts an operation and takes its locks. The leader sends
// it. It fails if another operation holds one of the locks.
type StartOperation struct {
	Kind  string   `json:"kind"`
	Locks []string `json:"locks"`
	// Owner runs the first step: a host, or nil for the leader.
	Owner    *HostID    `json:"owner,omitempty"`
	Deadline time.Time  `json:"deadline,omitzero"`
	Move     *StackMove `json:"move,omitempty"`
	Fence    *HostFence `json:"fence,omitempty"`
}

func applyStartOperation(s *State, cmd Command, payload StartOperation) (func(), error) {
	switch {
	case cmd.Actor != LeaderActor:
		return nil, fmt.Errorf("%s cannot start an operation; only the leader does", cmd.Actor)
	case payload.Kind == "":
		return nil, errors.New("the operation needs a kind")
	case len(payload.Locks) == 0:
		return nil, fmt.Errorf("operation %s locks nothing", payload.Kind)
	}
	if err := s.checkLiveHost(payload.Owner); err != nil {
		return nil, err
	}
	if err := s.checkMove(payload); err != nil {
		return nil, err
	}
	if err := s.checkFence(payload); err != nil {
		return nil, err
	}
	for _, lock := range payload.Locks {
		if err := s.checkLockable(lock); err != nil {
			return nil, err
		}
		if err := s.checkUnlocked(lock, 0); err != nil {
			return nil, err
		}
	}
	operation := &Operation{
		ID: s.Index, Kind: payload.Kind, Locks: slices.Compact(slices.Sorted(slices.Values(payload.Locks))),
		Owner: payload.Owner, Deadline: deadline(cmd, payload.Deadline), State: OpRunning, Move: payload.Move,
		Fence: payload.Fence,
	}
	return func() {
		s.Operations[operation.ID] = operation
		for _, lock := range operation.Locks {
			s.Locks[lock] = operation.ID
		}
		// in the same command, so that a beat that brings the host back
		// afterwards cannot stop the fence (phaseChanges)
		if fence := operation.Fence; fence != nil {
			s.Hosts[fence.Host].Phase = Fencing
		}
	}, nil
}

func (s *State) checkMove(payload StartOperation) error {
	move := payload.Move
	switch {
	case (payload.Kind == OperationMoveStack) != (move != nil):
		return fmt.Errorf("operation %s: a move goes with %s, and only with it", payload.Kind, OperationMoveStack)
	case move == nil:
		return nil
	case !slices.Contains(payload.Locks, StackLock(move.Stack)):
		return fmt.Errorf("a move of stack %s must lock it", move.Stack)
	}
	stack, ok := s.Stacks[move.Stack]
	switch {
	case !ok:
		return fmt.Errorf("no stack %s", move.Stack)
	case !samePlace(stack.Host, move.From):
		return fmt.Errorf("stack %s is not where the move starts", move.Stack)
	case move.From != nil && *move.From == move.To:
		return fmt.Errorf("stack %s is on host %d already", move.Stack, move.To)
	}
	host, ok := s.Hosts[move.To]
	if !ok || host.Phase != Active || host.Leaving || !host.Roles.Has(hostrole.Worker) {
		return fmt.Errorf("stack %s cannot move to host %d: it is no active worker", move.Stack, move.To)
	}
	return nil
}

// AdvanceOperation records a step done and who runs the next one.
type AdvanceOperation struct {
	ID uint64 `json:"id"`
	// FromStep is the step that the sender saw as the last one done, so
	// that a step reported twice (a slow host, a change of leader) is
	// refused the second time.
	FromStep int `json:"from_step"`
	Step     int `json:"step"`
	// Owner runs the next step: a host, or nil for the leader.
	Owner *HostID `json:"owner,omitempty"`
	// Deadline is when the next step must be done. Zero means StepTime
	// from the command's time.
	Deadline time.Time `json:"deadline,omitzero"`
}

func applyAdvanceOperation(s *State, cmd Command, payload AdvanceOperation) (func(), error) {
	operation, err := s.running(payload.ID)
	if err != nil {
		return nil, err
	}
	if err := s.checkFenceDecided(operation); err != nil {
		return nil, err
	}
	runner := LeaderActor
	if operation.Owner != nil {
		runner = HostActor(*operation.Owner)
	}
	switch {
	case operation.State != OpRunning:
		return nil, fmt.Errorf("operation %d rolls back", operation.ID)
	case cmd.Actor != runner:
		return nil, fmt.Errorf("operation %d: %s runs its step %d, not %s",
			operation.ID, runner, operation.Step+1, cmd.Actor)
	case payload.FromStep != operation.Step:
		return nil, fmt.Errorf("operation %d is at step %d, no longer %d", operation.ID, operation.Step, payload.FromStep)
	case payload.Step <= operation.Step:
		return nil, fmt.Errorf("operation %d: step %d does not follow step %d", operation.ID, payload.Step, operation.Step)
	}
	if err := s.checkLiveHost(payload.Owner); err != nil {
		return nil, err
	}
	return func() {
		operation.Step, operation.Owner, operation.Deadline = payload.Step, payload.Owner, deadline(cmd, payload.Deadline)
	}, nil
}

// ExtendOperation gives an operation's next step more time. The leader
// sends it for a step that is late but still makes progress, such as a
// resync.
type ExtendOperation struct {
	ID uint64 `json:"id"`
	// Step is the last step done, as the leader saw it. Once another step
	// is done, the extension would be for a step that is over, so it is
	// refused.
	Step     int       `json:"step"`
	Deadline time.Time `json:"deadline"`
}

func applyExtendOperation(s *State, cmd Command, payload ExtendOperation) (func(), error) {
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot extend an operation; only the leader does", cmd.Actor)
	}
	operation, err := s.running(payload.ID)
	switch {
	case err != nil:
		return nil, err
	case payload.Step != operation.Step:
		return nil, fmt.Errorf("operation %d is at step %d, no longer %d", operation.ID, operation.Step, payload.Step)
	case !payload.Deadline.After(operation.Deadline):
		return nil, fmt.Errorf("operation %d: the deadline %s is not later than %s", operation.ID,
			payload.Deadline.Format(time.RFC3339), operation.Deadline.Format(time.RFC3339))
	}
	return func() { operation.Deadline = payload.Deadline }, nil
}

// RollBackOperation turns an operation back before its point of no return.
// The leader finishes it as failed once it is undone, and the operation
// holds its locks until then. A move needs no undoing of its own: its stack
// runs on its old host again (StopsForMove), and the placement gives up the
// copies that the move left.
type RollBackOperation struct {
	ID       uint64 `json:"id"`
	FromStep int    `json:"from_step"` // as in AdvanceOperation
	Error    string `json:"error"`     // why
}

func applyRollBackOperation(s *State, cmd Command, payload RollBackOperation) (func(), error) {
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot roll an operation back; only the leader does", cmd.Actor)
	}
	operation, err := s.running(payload.ID)
	switch {
	case err != nil:
		return nil, err
	case operation.State != OpRunning:
		return nil, fmt.Errorf("operation %d rolls back already", operation.ID)
	case payload.FromStep != operation.Step:
		return nil, fmt.Errorf("operation %d is at step %d, no longer %d", operation.ID, operation.Step, payload.FromStep)
	case operation.Move != nil && operation.Step >= MoveStopped:
		return nil, fmt.Errorf("operation %d is past its point of no return: the stack stopped on its old host",
			operation.ID)
	case payload.Error == "":
		return nil, fmt.Errorf("operation %d: a rollback says why", operation.ID)
	}
	return func() {
		operation.State, operation.Owner, operation.Error = OpRollingBack, nil, payload.Error
	}, nil
}

// FinishOperation ends an operation and frees its locks. The leader sends
// it.
type FinishOperation struct {
	ID     uint64 `json:"id"`
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
}

// applyFinishOperation settles a user's request to move the stack (MoveTo)
// whether the move worked or not, so the placement does not try it again by
// itself.
func applyFinishOperation(s *State, cmd Command, payload FinishOperation) (func(), error) {
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot finish an operation; only the leader does", cmd.Actor)
	}
	operation, err := s.running(payload.ID)
	if err == nil {
		err = s.checkFenceDecided(operation)
	}
	if err != nil {
		return nil, err
	}
	failed := payload.Failed || operation.State == OpRollingBack
	return func() {
		operation.State = OpDone
		if failed {
			operation.State = OpFailed
		}
		if payload.Error != "" {
			operation.Error = payload.Error
		}
		operation.Owner = nil
		for _, lock := range operation.Locks {
			delete(s.Locks, lock)
		}
		if stack, ok := s.Stacks[moveStack(operation)]; ok {
			stack.MoveTo = nil
			if failed {
				stack.MoveFailed = cmd.At
			}
			s.changed(resource.KindStack, &stack.Meta)
		}
		s.forgetFinished()
	}, nil
}

func moveStack(operation *Operation) string {
	if operation.Move == nil {
		return ""
	}
	return operation.Move.Stack
}

// running returns an operation that has not finished, that is, one that
// runs or rolls back.
func (s *State) running(id uint64) (*Operation, error) {
	operation, ok := s.Operations[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("no operation %d", id)
	case operation.State != OpRunning && operation.State != OpRollingBack:
		return nil, fmt.Errorf("operation %d is finished (%s)", id, operation.State)
	}
	return operation, nil
}

func (s *State) forgetFinished() {
	var finished []uint64
	for _, id := range slices.Sorted(maps.Keys(s.Operations)) {
		if state := s.Operations[id].State; state == OpDone || state == OpFailed {
			finished = append(finished, id)
		}
	}
	for _, id := range finished[:max(0, len(finished)-keptFinished)] {
		delete(s.Operations, id)
	}
}

// checkFenceDecided refuses to move a fence-host operation on while its
// host is fencing. Only the operation moves the host on, to fenced or back
// to failed, and it must do so before its next step or its end. Otherwise
// the host would stay fencing with no operation left to move it.
func (s *State) checkFenceDecided(operation *Operation) error {
	if fence := operation.Fence; fence != nil && s.Hosts[fence.Host].Phase == Fencing {
		return fmt.Errorf("operation %d: host %d is still fencing", operation.ID, fence.Host)
	}
	return nil
}
