package raftnode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/webcodingcat/clowder/internal/state"
)

func applyCommand(t *testing.T, machine *fsm, index uint64, cmd state.Command) error {
	t.Helper()
	encoded, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err, _ = machine.Apply(&raft.Log{Index: index, Data: encoded}).(error)
	return err
}

func halted(machine *fsm) bool {
	select {
	case <-machine.halted:
		return true
	default:
		return false
	}
}

func TestAnEntryTheStateCannotReadHaltsTheFSM(t *testing.T) {
	machine := newFSM()
	if err := applyCommand(t, machine, 1, bootstrapCommand(t)); err != nil {
		t.Fatal(err)
	}
	// a refusal is no reason to stop
	if err := applyCommand(t, machine, 2, bootstrapCommand(t)); err == nil || halted(machine) {
		t.Fatalf("a second bootstrap: %v, halted %v", err, halted(machine))
	}
	unknown, _ := machine.Apply(&raft.Log{Index: 3, Data: []byte(`{"version":1,"kind":"make-coffee"}`)}).(error)
	if !errors.Is(unknown, state.ErrUnreadable) || !halted(machine) || !errors.Is(machine.haltError(), state.ErrUnreadable) {
		t.Fatalf("an unknown kind: %v, halted %v", unknown, halted(machine))
	}
	// nothing after it is applied, restored or written to a snapshot
	if err := applyCommand(t, machine, 4, finishBootstrap(t)); err == nil {
		t.Error("an entry after the halt was taken")
	}
	var index uint64
	var inBootstrap bool
	machine.read(func(current *state.State) { index, inBootstrap = current.Index, current.InBootstrap() })
	if index != 2 || !inBootstrap {
		t.Errorf("after the halt: index %d, in bootstrap %v", index, inBootstrap)
	}
	if _, err := machine.Snapshot(); err == nil {
		t.Error("a halted fsm wrote a snapshot")
	}
	var snapshot bytes.Buffer
	if err := (&state.State{Index: 9}).WriteSnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := machine.Restore(io.NopCloser(&snapshot)); err == nil {
		t.Error("a halted fsm restored a snapshot")
	}
}

func TestASnapshotTheStateCannotReadHaltsTheFSM(t *testing.T) {
	machine := newFSM()
	err := machine.Restore(io.NopCloser(strings.NewReader("not a snapshot")))
	if !errors.Is(err, state.ErrUnreadable) || !halted(machine) {
		t.Errorf("restore: %v, halted %v", err, halted(machine))
	}
}

func TestAHaltedMemberStopsAgainAfterARestart(t *testing.T) {
	cluster := newTestCluster(t)
	dir, address := t.TempDir(), freeAddress(t)
	node := cluster.open(0, dir, address)
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	waitForLeader(t, node)
	// what a newer leader would write: a kind this build does not know
	if err := node.raft.Apply([]byte(`{"version":1,"kind":"make-coffee"}`), time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	waitForHalt(t, node)
	if err := node.raft.Snapshot().Error(); err == nil {
		t.Error("a halted member took a snapshot")
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := cluster.open(0, dir, address)
	defer func() { _ = reopened.Close() }()
	waitForHalt(t, reopened)
	if !errors.Is(reopened.HaltError(), state.ErrUnreadable) {
		t.Errorf("halt error %v", reopened.HaltError())
	}
}

func waitForHalt(t *testing.T, node *Node) {
	t.Helper()
	select {
	case <-node.Halted():
	case <-time.After(15 * time.Second):
		t.Fatal("the member did not halt")
	}
}

func TestTheLoopsShareOneCopyUntilTheNextChange(t *testing.T) {
	machine := newFSM()
	first, err := machine.sharedCopy()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := machine.sharedCopy(); err != nil || again != first {
		t.Errorf("a second reader got another copy: %v", err)
	}
	encoded, err := json.Marshal(bootstrapCommand(t))
	if err != nil {
		t.Fatal(err)
	}
	if err, _ := machine.Apply(&raft.Log{Index: 1, Data: encoded}).(error); err != nil {
		t.Fatal(err)
	}
	after, err := machine.sharedCopy()
	if err != nil || after == first || after.Index != 1 || first.Index != 0 {
		t.Errorf("after a change: the same copy %v, index %d (the first %d), %v", after == first, after.Index,
			first.Index, err)
	}
}
