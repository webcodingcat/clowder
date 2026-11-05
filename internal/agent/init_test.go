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
		{
			BootstrapKey: testBootstrapKey, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("2001:db8::2"),
			PublicAddress: netip.MustParseAddr("198.51.100.2"),
		},
		{
			BootstrapKey: testBootstrapKey, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("2001:db8::3"),
		},
		{BootstrapKey: testBootstrapKey, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2")},
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

// testBootstrapKey is long enough (pki.MinBootstrapKeyLength).
const testBootstrapKey = "the-bootstrap-key-of-the-agents-tests"

// node0Options are what cloud-init gives node0's clowderd init.
func node0Options() InitOptions {
	return InitOptions{
		BootstrapKey: testBootstrapKey, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
		PublicAddress: netip.MustParseAddr("198.51.100.2"),
	}
}

func TestInitTakesOnlyAHostnameTheStateTakes(t *testing.T) {
	files := Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	options := node0Options()
	options.Name = "Node0.example.com"
	err := Init(context.Background(), files, options, &fakeNetwork{}, slog.Default())
	if err == nil || !strings.Contains(err.Error(), `the hostname "Node0.example.com" cannot name a host`) {
		t.Errorf("init with a FQDN: %v", err)
	}
	if _, err := os.Stat(files.raftDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused init left %s: %v", files.raftDir(), err)
	}
}

func TestInitTakesNoShortBootstrapKey(t *testing.T) {
	files := Files{Dir: t.TempDir(), SecretsDir: t.TempDir()}
	options := node0Options()
	options.BootstrapKey = "short"
	err := Init(context.Background(), files, options, &fakeNetwork{}, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "fewer than 32 characters") {
		t.Errorf("init with a short key: %v", err)
	}
}
