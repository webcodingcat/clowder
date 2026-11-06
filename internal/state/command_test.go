package state

import (
	"errors"
	"fmt"
	"testing"
)

func TestNothingButTheBootstrapRunsOnAnEmptyState(t *testing.T) {
	state := &State{}
	cmd, err := NewCommand(KindSetCLICutoff, testTime, HostActor(0), SetCLICutoff{Cutoff: testTime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Apply(1, cmd); !errors.Is(err, ErrNotBootstrapped) {
		t.Errorf("want ErrNotBootstrapped, got %v", err)
	}
	if state.Index != 1 {
		t.Errorf("the index must follow the log even for refused commands: %d", state.Index)
	}
}

func TestAnEntryTheAgentCannotReadLeavesTheStateAndItsIndex(t *testing.T) {
	c := newCluster(t)
	before, index := c.encoded(), c.state.Index
	payload := `"payload":{"name":"alice","serial":"a1"}`
	for _, test := range []struct {
		name, entry string
	}{
		{"not JSON", `{"version":`},
		{"a newer version", fmt.Sprintf(`{"version":%d,"kind":"add-user",%s}`, CommandVersion+1, payload)},
		{"an older version", fmt.Sprintf(`{"version":%d,"kind":"add-user",%s}`, CommandVersion-2, payload)},
		{"no version", `{"kind":"add-user",` + payload + `}`},
		{"an unknown kind", fmt.Sprintf(`{"version":%d,"kind":"make-coffee",%s}`, CommandVersion, payload)},
	} {
		if _, err := c.state.ApplyEntry(index+1, []byte(test.entry)); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: want ErrUnreadable, got %v", test.name, err)
		}
		if c.state.Index != index || c.encoded() != before {
			t.Errorf("%s: the state changed, index %d", test.name, c.state.Index)
		}
	}
}

func TestARefusedCommandMovesTheIndexOn(t *testing.T) {
	c := newCluster(t)
	err := c.apply(KindBootstrap, LeaderActor, Bootstrap{})
	var refusal *CommandError
	if !errors.As(err, &refusal) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("want a CommandError, got %v", err)
	}
	if c.state.Index != c.index {
		t.Errorf("index %d, want %d", c.state.Index, c.index)
	}
}

// The proposer tells a refusal from a command that did not commit by
// Refused, and still reads Apply's CommandError through it.
func TestARefusalCarriesApplysError(t *testing.T) {
	c := newCluster(t)
	applied := c.apply(KindBootstrap, LeaderActor, Bootstrap{})
	var err error = &Refused{Err: applied}
	var refused *Refused
	var refusal *CommandError
	if !errors.As(err, &refused) || !errors.As(err, &refusal) || err.Error() != applied.Error() {
		t.Errorf("refused %v, command error %v", err, refusal)
	}
}

func TestCheckKnownRefusesOnlyAnUnknownKind(t *testing.T) {
	if err := CheckKnown(KindBootstrap); err != nil {
		t.Errorf("a known kind: %v", err)
	}
	if err := CheckKnown("make-coffee"); err == nil {
		t.Error("an unknown kind passed")
	}
}

func TestTheAuditKeepsTheLastEntries(t *testing.T) {
	c := newCluster(t)
	for range auditSize + 10 {
		c.mustApply(KindReportHost, HostActor(0), ReportHost{Host: 0, Version: "1.0.0"})
	}
	audit := c.state.Audit
	if len(audit) != auditSize || audit[len(audit)-1].Index != c.index || audit[0].Index != c.index-auditSize+1 {
		t.Errorf("%d entries from %d to %d", len(audit), audit[0].Index, audit[len(audit)-1].Index)
	}
	if last := audit[len(audit)-1]; last.Actor != HostActor(0) || last.Kind != KindReportHost {
		t.Errorf("last entry %+v", last)
	}
}
