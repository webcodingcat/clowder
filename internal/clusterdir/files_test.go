package clusterdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/pki"
)

func TestTheFilesNamedInClusterYAMLAreReadByTheirNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	authority, err := pki.NewCA("test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca := authority.CertificatePEM()
	if err := os.WriteFile(filepath.Join(dir, "certs", "pebble.pem"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := &Directory{Path: dir, Config: clusterspec.Config{ACME: clusterspec.ACME{CA: "certs/pebble.pem"}}}
	files, err := directory.Files()
	if err != nil || len(files) != 1 || string(files["certs/pebble.pem"]) != string(ca) {
		t.Fatalf("files %v, %v", files, err)
	}

	// what clusterspec.CheckFiles refuses, Files refuses too
	if err := os.WriteFile(filepath.Join(dir, "certs", "pebble.pem"), []byte("no certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Files(); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("a CA without a certificate: %v", err)
	}
	directory.Config.ACME.CA = "certs/missing.pem"
	if _, err := directory.Files(); err == nil {
		t.Error("a missing file was read")
	}
}
