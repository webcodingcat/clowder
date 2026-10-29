package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Operation is a long task (moving a stack, fencing a host, a backup) as a
// state machine: the leader writes each step, the agent that owns it runs it
// and reports back. Every step first checks whether its goal is reached
// already, so a step survives a new leader or a restarted agent.
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

func stackLock(name string) string { return "stack/" + name }

func volumeLock(id string) string { return "volume/" + id }

// LockedError says which operation holds a lock. Backups wait for it,
// commands from users are refused with it.
type LockedError struct {
	Lock      string
	Operation string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("%s is locked by operation %s", e.Lock, e.Operation)
}

// checkUnlocked refuses a change to something an operation holds.
func (s *State) checkUnlocked(lock string) error {
	if holder, held := s.Locks[lock]; held {
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

func applyStartOperation(s *State, _ Command, payload StartOperation) error {
	operation := payload.Operation
	switch {
	case operation.ID == "" || operation.Kind == "":
		return errors.New("the operation needs an ID and a kind")
	case len(operation.Locks) == 0:
		return fmt.Errorf("operation %s locks nothing", operation.ID)
	}
	if _, exists := s.Operations[operation.ID]; exists {
		return fmt.Errorf("operation %s exists already", operation.ID)
	}
	if err := s.checkLiveHost(operation.Owner); err != nil {
		return err
	}
	holders := map[string]bool{}
	for _, lock := range operation.Locks {
		holder, held := s.Locks[lock]
		switch {
		case held && !payload.TakeOver:
			return &LockedError{Lock: lock, Operation: holder}
		case held:
			holders[holder] = true
		}
	}
	for _, holder := range slices.Sorted(maps.Keys(holders)) {
		s.finish(holder, OpFailed, "taken over by "+operation.ID)
	}
	operation.Step, operation.State, operation.Error = 0, OpRunning, ""
	s.Operations[operation.ID] = &operation
	for _, lock := range operation.Locks {
		s.Locks[lock] = operation.ID
	}
	return nil
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

func applyAdvanceOperation(s *State, _ Command, payload AdvanceOperation) error {
	operation, err := s.unfinished(payload.ID)
	if err != nil {
		return err
	}
	if err := s.checkLiveHost(payload.Owner); err != nil {
		return err
	}
	if payload.RollingBack {
		operation.State = OpRollingBack
	}
	operation.Step, operation.Owner, operation.Deadline = payload.Step, payload.Owner, payload.Deadline
	return nil
}

// FinishOperation ends an operation and frees its locks.
type FinishOperation struct {
	ID     string `json:"id"`
	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
}

func applyFinishOperation(s *State, _ Command, payload FinishOperation) error {
	if _, err := s.unfinished(payload.ID); err != nil {
		return err
	}
	outcome := OpDone
	if payload.Failed {
		outcome = OpFailed
	}
	s.finish(payload.ID, outcome, payload.Error)
	return nil
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
		if state := s.Operations[other].State; state == OpDone || state == OpFailed {
			finished = append(finished, other)
		}
	}
	for _, old := range finished[:max(0, len(finished)-keptFinished)] {
		delete(s.Operations, old)
	}
}
