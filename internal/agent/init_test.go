package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestInitTakesOnlyIPv4(t *testing.T) {
	files := Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	for _, options := range []InitOptions{
		{BootstrapKey: "k", Name: "node0", Underlay: netip.MustParseAddr("2001:db8::2")},
		{
			BootstrapKey: "k", Name: "node0", Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("2001:db8::3"),
		},
	} {
		err := Init(context.Background(), files, options, &fakeNetwork{}, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "IPv4") {
			t.Errorf("init with %s and %s: %v", options.Underlay, options.PublicAddress, err)
		}
	}
	if _, err := os.Stat(files.raftDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused init left %s: %v", files.raftDir(), err)
	}
}
