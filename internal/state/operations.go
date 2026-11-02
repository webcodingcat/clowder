package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Operation is a long task, such as moving a stack or fencing a host, in
// steps: the leader starts it, whoever owns a step runs it and records it
// done (advance-operation), and the leader finishes it. While it runs it
// holds locks on what it changes, and no one else changes that.
//
// The state holds and checks operations; nothing starts one yet. Moving a
// stack and fencing a host bring the first ones, with the steps, deadlines
// and rollbacks they need.
type Operation struct {
	// ID is the Raft index of the command that started it: unique, and
	// sorted by start.
	ID   uint64 `json:"id"`
	Kind string `json:"kind"`
	// Locks are what it changes: stackLock, volumeLock, hostLock.
	Locks []string `json:"locks"`
	// Step is the last step done; 0 before the first.
	Step int `json:"step"`
	// Owner runs the next step: a host, or nil for the leader.
	Owner *HostID `json:"owner,omitempty"`
	State OpState `json:"state"`
	Error string  `json:"error,omitempty"`
}

// OpState is where an operation is.
type OpState string

// The states of an operation.
const (
	OpRunning OpState = "running"
	OpDone    OpState = "done"
	OpFailed  OpState = "failed"
)

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

func stackLock(id string) string { return stackLockPrefix + id }

func volumeLock(id string) string { return volumeLockPrefix + id }

func hostLock(id HostID) string { return hostLockPrefix + strconv.Itoa(int(id)) }

// checkLockable accepts a lock on a stack or a volume the state has, or on
// a host that is a member: a lock on nothing would protect nothing.
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
// operation makes it: operation is the one that sends the change, 0 for
// none.
func (s *State) checkUnlocked(lock string, operation uint64) error {
	if holder, held := s.Locks[lock]; held && holder != operation {
		return fmt.Errorf("%s is locked by operation %d (%s); try again once it is finished",
			lock, holder, s.Operations[holder].Kind)
	}
	return nil
}

// StartOperation starts an operation and takes its locks; the leader sends
// it. It fails if another operation holds one of them.
type StartOperation struct {
	Kind  string   `json:"kind"`
	Locks []string `json:"locks"`
	// Owner runs the first step: a host, or nil for the leader.
	Owner *HostID `json:"owner,omitempty"`
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
		Owner: payload.Owner, State: OpRunning,
	}
	return func() {
		s.Operations[operation.ID] = operation
		for _, lock := range operation.Locks {
			s.Locks[lock] = operation.ID
		}
	}, nil
}

// AdvanceOperation records a step done and who runs the next one.
type AdvanceOperation struct {
	ID uint64 `json:"id"`
	// FromStep is the step the sender saw as the last one done: a step
	// recorded twice, by a host that took long to tell, or by a leader
	// before and after a change of leader, is refused the second time.
	FromStep int `json:"from_step"`
	Step     int `json:"step"`
	// Owner runs the next step: a host, or nil for the leader.
	Owner *HostID `json:"owner,omitempty"`
}

// applyAdvanceOperation takes the step only from the one who runs it: the
// owner of the step, which is the leader if the operation names no host.
func applyAdvanceOperation(s *State, cmd Command, payload AdvanceOperation) (func(), error) {
	operation, err := s.running(payload.ID)
	if err != nil {
		return nil, err
	}
	runner := LeaderActor
	if operation.Owner != nil {
		runner = HostActor(*operation.Owner)
	}
	switch {
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
		operation.Step, operation.Owner = payload.Step, payload.Owner
	}, nil
}

// FinishOperation ends an operation and frees its locks; the leader sends
// it.
type FinishOperation struct {
	ID     uint64 `json:"id"`
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
}

func applyFinishOperation(s *State, cmd Command, payload FinishOperation) (func(), error) {
	if cmd.Actor != LeaderActor {
		return nil, fmt.Errorf("%s cannot finish an operation; only the leader does", cmd.Actor)
	}
	operation, err := s.running(payload.ID)
	if err != nil {
		return nil, err
	}
	return func() {
		operation.State, operation.Error = OpDone, payload.Error
		if payload.Failed {
			operation.State = OpFailed
		}
		for _, lock := range operation.Locks {
			delete(s.Locks, lock)
		}
		s.forgetFinished()
	}, nil
}

// running returns an operation that has not finished.
func (s *State) running(id uint64) (*Operation, error) {
	operation, ok := s.Operations[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("no operation %d", id)
	case operation.State != OpRunning:
		return nil, fmt.Errorf("operation %d is finished (%s)", id, operation.State)
	}
	return operation, nil
}

// forgetFinished forgets the oldest finished operations beyond
// keptFinished.
func (s *State) forgetFinished() {
	var finished []uint64
	for _, id := range slices.Sorted(maps.Keys(s.Operations)) {
		if s.Operations[id].State != OpRunning {
			finished = append(finished, id)
		}
	}
	for _, id := range finished[:max(0, len(finished)-keptFinished)] {
		delete(s.Operations, id)
	}
}
