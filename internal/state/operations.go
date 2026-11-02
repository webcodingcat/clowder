package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
)

// Operation is a long task (moving a stack, fencing a host, a backup) as a
// state machine: the leader writes each step, the agent that owns it runs it
// and reports back. Every step first checks whether its goal is reached
// already, so a step survives a new leader or a restarted agent.
//
// Operations are for phase 2 (failover, moving a volume): the state applies
// and checks them, and stacks and volumes obey their locks, but nothing
// starts one yet.
type Operation struct {
	ID     string          `json:"id"` // a ULID: sorts by start time
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
	// Locks are the stacks and volumes the operation changes.
	Locks []string `json:"locks"`
	// Step is the last confirmed step.
	Step     int       `json:"step"`
	Owner    *HostID   `json:"owner,omitempty"` // runs the current step; nil: the leader
	Deadline time.Time `json:"deadline,omitzero"`
	State    OpState   `json:"state"`
	Error    string    `json:"error,omitempty"`
}

// OpState is where an operation is.
type OpState string

// The states of an operation. One that fails before its point of no return
// rolls back first and then ends as failed.
const (
	OpRunning     OpState = "running"
	OpRollingBack OpState = "rolling-back"
	OpDone        OpState = "done"
	OpFailed      OpState = "failed"
)

// keptFinished is how many finished operations stay for `clowder` to show.
const keptFinished = 100

func stackLock(id string) string { return "stack/" + id }

func volumeLock(id string) string { return "volume/" + id }

// validLock accepts what stackLock and volumeLock make: a lock that names
// nothing would never conflict, and so protect nothing.
func validLock(lock string) bool {
	for _, prefix := range []string{"stack/", "volume/"} {
		if id, ok := strings.CutPrefix(lock, prefix); ok {
			_, _, ok := names.SplitID(id)
			return ok
		}
	}
	return false
}

// LockedError says which operation holds a lock. Backups wait for it,
// commands from users are refused with it.
type LockedError struct {
	Lock      string
	Operation string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("%s is locked by operation %s", e.Lock, e.Operation)
}

// checkUnlocked refuses a change to something an operation holds, unless
// that operation makes it.
func (s *State) checkUnlocked(lock, operation string) error {
	if holder, held := s.Locks[lock]; held && holder != operation {
		return &LockedError{Lock: lock, Operation: holder}
	}
	return nil
}

// StartOperation starts an operation and takes its locks. With TakeOver,
// operations holding one of the locks fail and give theirs up; only failover
// does this, when the host running them is gone.
type StartOperation struct {
	Operation Operation `json:"operation"`
	TakeOver  bool      `json:"take_over,omitempty"`
}

func applyStartOperation(s *State, _ Command, payload StartOperation) (func(), error) {
	operation := payload.Operation
	switch {
	case operation.ID == "" || operation.Kind == "":
		return nil, errors.New("the operation needs an ID and a kind")
	case len(operation.Locks) == 0:
		return nil, fmt.Errorf("operation %s locks nothing", operation.ID)
	}
	if _, exists := s.Operations[operation.ID]; exists {
		return nil, fmt.Errorf("operation %s exists already", operation.ID)
	}
	if err := s.checkLiveHost(operation.Owner); err != nil {
		return nil, err
	}
	for _, lock := range operation.Locks {
		if !validLock(lock) {
			return nil, fmt.Errorf("operation %s: lock %q is neither stack/<stack ID> nor volume/<volume ID>",
				operation.ID, lock)
		}
	}
	holders := map[string]bool{}
	for _, lock := range operation.Locks {
		holder, held := s.Locks[lock]
		switch {
		case held && !payload.TakeOver:
			return nil, &LockedError{Lock: lock, Operation: holder}
		case held:
			holders[holder] = true
		}
	}
	operation.Step, operation.State, operation.Error = 0, OpRunning, ""
	return func() {
		for _, holder := range slices.Sorted(maps.Keys(holders)) {
			s.finish(holder, OpFailed, "taken over by "+operation.ID)
		}
		s.Operations[operation.ID] = &operation
		for _, lock := range operation.Locks {
			s.Locks[lock] = operation.ID
		}
	}, nil
}

// AdvanceOperation records a confirmed step and who runs the next one.
type AdvanceOperation struct {
	ID       string    `json:"id"`
	Step     int       `json:"step"`
	Owner    *HostID   `json:"owner,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
	// RollingBack is set from the first step back on.
	RollingBack bool `json:"rolling_back,omitempty"`
}

func applyAdvanceOperation(s *State, _ Command, payload AdvanceOperation) (func(), error) {
	operation, err := s.unfinished(payload.ID)
	if err != nil {
		return nil, err
	}
	if err := s.checkLiveHost(payload.Owner); err != nil {
		return nil, err
	}
	return func() {
		if payload.RollingBack {
			operation.State = OpRollingBack
		}
		operation.Step, operation.Owner, operation.Deadline = payload.Step, payload.Owner, payload.Deadline
	}, nil
}

// FinishOperation ends an operation and frees its locks.
type FinishOperation struct {
	ID     string `json:"id"`
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
}

func applyFinishOperation(s *State, _ Command, payload FinishOperation) (func(), error) {
	if _, err := s.unfinished(payload.ID); err != nil {
		return nil, err
	}
	outcome := OpDone
	if payload.Failed {
		outcome = OpFailed
	}
	return func() { s.finish(payload.ID, outcome, payload.Error) }, nil
}

// unfinished returns an operation that is still running or rolling back.
func (s *State) unfinished(id string) (*Operation, error) {
	operation, ok := s.Operations[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("no operation %s", id)
	case operation.State == OpDone || operation.State == OpFailed:
		return nil, fmt.Errorf("operation %s is finished (%s)", id, operation.State)
	}
	return operation, nil
}

// finish ends an operation, frees its locks and forgets the oldest finished
// operations beyond keptFinished.
func (s *State) finish(id string, outcome OpState, message string) {
	operation := s.Operations[id]
	operation.State, operation.Error = outcome, message
	for _, lock := range operation.Locks {
		if s.Locks[lock] == id {
			delete(s.Locks, lock)
		}
	}
	var finished []string
	for _, other := range slices.Sorted(maps.Keys(s.Operations)) {
		if outcome := s.Operations[other].State; outcome == OpDone || outcome == OpFailed {
			finished = append(finished, other)
		}
	}
	for _, old := range finished[:max(0, len(finished)-keptFinished)] {
		delete(s.Operations, old)
	}
}
