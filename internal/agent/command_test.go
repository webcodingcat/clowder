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
	// what every agent checks them with
	derived, err := key.Derive(recoverycode.Purpose)
	if err != nil {
		t.Fatal(err)
	}
	uses := map[string]recoverycode.Use{
		"clowder user recover alice --code ": recoverycode.ForRecover,
		"clowder recovery rotate --code ":    recoverycode.ForRotate,
	}
	for command, use := range uses {
		_, text, found := strings.Cut(out.String(), command)
		text, _, _ = strings.Cut(text, "\n")
		code, err := recoverycode.Parse(text)
		if !found || err != nil {
			t.Fatalf("no code for %q in %q: %v", command, out.String(), err)
		}
		if _, valid := recoverycode.Valid(derived, use, "alice", code, now); !valid {
			t.Errorf("code %v for %s does not fit", code, use)
		}
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
