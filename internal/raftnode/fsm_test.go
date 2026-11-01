package raftnode

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/raft"
)

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
