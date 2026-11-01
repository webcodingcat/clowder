package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/datakey"
	"github.com/webcodingcat/clowder/internal/recoverycode"
)

func TestTheRecoveryCodeComesFromTheDataKeyAlone(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	key, err := datakey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(files.dataKey()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := key.Save(files.dataKey()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var out bytes.Buffer
	if err := printRecoveryCode(&out, files, "alice", now); err != nil {
		t.Fatal(err)
	}
	code, err := recoverycode.Parse(strings.SplitN(out.String(), "\n", 2)[0])
	if err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	// what every agent checks it with
	derived, err := key.Derive(recoverycode.Purpose)
	if err != nil {
		t.Fatal(err)
	}
	if !recoverycode.Valid(derived, "alice", code, now) {
		t.Errorf("code %v does not fit", code)
	}
	if err := printRecoveryCode(&out, files, "Alice Smith", now); err == nil {
		t.Error("a name no user can have gave a code")
	}
}

func TestVersionPrintsTheBuildVersion(t *testing.T) {
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := "clowderd " + buildinfo.Version + "\n"; out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
