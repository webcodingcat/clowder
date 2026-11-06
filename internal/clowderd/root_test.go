package clowderd

import (
	"bytes"
	"testing"

	"github.com/webcodingcat/clowder/internal/buildinfo"
)

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
