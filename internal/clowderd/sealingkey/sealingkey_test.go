package sealingkey

import (
	"context"
	"io"
	"testing"

	"github.com/webcodingcat/clowder/internal/clowderd/hostfiles"
	"github.com/webcodingcat/clowder/internal/hostrole"
)

func TestOnlyAControlPlaneHostHandsOutTheSealingKey(t *testing.T) {
	files := hostfiles.Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	if err := files.WriteHost(hostfiles.Host{ID: 1, Name: "node1", Roles: hostrole.DefaultJoin}); err != nil {
		t.Fatal(err)
	}
	err := Print(context.Background(), io.Discard, files, false)
	want := "this host has the roles worker,ingress; clowderd sealing-key runs on a control-plane host only"
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}
