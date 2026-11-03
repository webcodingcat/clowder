package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/command"
)

// forgetHost answers drbdsetup as a host with the resources clw-a and
// clw-b: status gives what status says, and forget-peer on a resource
// fails with what refusals holds for it.
type forgetHost struct {
	status   string
	refusals map[string]error
	commands []string
}

func (f *forgetHost) run(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, line)
	if line == "drbdsetup status --json" {
		return f.status, nil
	}
	if len(args) > 1 && args[0] == "forget-peer" {
		return "", f.refusals[args[1]]
	}
	return "", nil
}

func newForgetDriver(t *testing.T, host *forgetHost) Driver {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"clw-a", "clw-b"} {
		if err := os.WriteFile(filepath.Join(dir, name+".res"), []byte("resource"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Driver{ResourceDir: dir, Run: host.run}
}

func TestAPeerIsForgottenOnEveryResourceOnceItIsGone(t *testing.T) {
	host := &forgetHost{
		// clw-a has the removed host 2 configured still, clw-b only host 1
		status: `[{"name": "clw-b", "connections": [{"peer-node-id": 1}]},
			{"name": "clw-a", "connections": [{"peer-node-id": 1}, {"peer-node-id": 2}]}]`,
		refusals: map[string]error{
			"clw-b": &command.ExitError{Code: 10, Stderr: "clw-b: State change failed: (-172) Invalid peer-node-id"},
		},
	}
	driver := newForgetDriver(t, host)
	if err := driver.ForgetPeer(context.Background(), 2); err == nil || !strings.Contains(err.Error(), "clw-a has peer 2") {
		t.Errorf("a resource with the peer configured: %v", err)
	}
	host.status = `[{"name": "clw-b", "connections": [{"peer-node-id": 1}]}, {"name": "clw-a", "connections": []}]`
	host.commands = nil
	if err := driver.ForgetPeer(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	want := []string{"drbdsetup status --json", "drbdsetup forget-peer clw-a 2", "drbdsetup forget-peer clw-b 2"}
	if !slices.Equal(host.commands, want) {
		t.Errorf("commands %q", host.commands)
	}
}

func TestAPeerStaysUntilEveryResourceForgotIt(t *testing.T) {
	host := &forgetHost{status: `[{"name": "clw-a", "connections": []}]`}
	driver := newForgetDriver(t, host)
	if err := driver.ForgetPeer(context.Background(), 2); err == nil || !strings.Contains(err.Error(), "clw-b is down") {
		t.Errorf("a resource that is down: %v", err)
	}
	host.status = `[{"name": "clw-a", "connections": []}, {"name": "clw-b", "connections": []}]`
	host.refusals = map[string]error{"clw-a": errors.New("timed out")}
	if err := driver.ForgetPeer(context.Background(), 2); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("a forget that timed out: %v", err)
	}
}
