package clusterdir

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCAPEM is a self-signed certificate, as acme.ca holds one.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTheFilesNamedInClusterYAMLAreReadByTheirNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	ca := testCAPEM(t)
	if err := os.WriteFile(filepath.Join(dir, "certs", "pebble.pem"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := &Directory{Path: dir, Config: Config{ACME: ACME{CA: "certs/pebble.pem"}}}
	files, err := directory.Files()
	if err != nil || len(files) != 1 || string(files["certs/pebble.pem"]) != string(ca) {
		t.Fatalf("files %v, %v", files, err)
	}

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

func TestFilesMustBeThereAndSmall(t *testing.T) {
	config := Config{ACME: ACME{CA: "ca.pem"}}
	problems := CheckFiles(config, map[string][]byte{})
	if len(problems) != 1 || !strings.Contains(problems[0], "file ca.pem is named in cluster.yaml but missing") {
		t.Errorf("missing: %v", problems)
	}
	large := append(testCAPEM(t), make([]byte, maxFileSize)...)
	problems = CheckFiles(config, map[string][]byte{"ca.pem": large})
	if len(problems) != 1 || !strings.Contains(problems[0], "larger than 64 KiB") {
		t.Errorf("too large: %v", problems)
	}
}

func TestFileNamesAreRelativeWithSlashes(t *testing.T) {
	for name, valid := range map[string]bool{
		"ca.pem": true, "certs/ca.pem": true,
		"/etc/ca.pem": false, "../ca.pem": false, "certs/../ca.pem": false, `certs\ca.pem`: false,
		"C:/ca.pem": false, ".": false, "certs/": false, "./ca.pem": false,
	} {
		if validFileName(name) != valid {
			t.Errorf("%q: want valid %v", name, valid)
		}
	}
}
