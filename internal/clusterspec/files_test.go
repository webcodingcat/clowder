package clusterspec

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
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

func TestFilesMustBeThereAndSmallAndCAsHoldACertificate(t *testing.T) {
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
	problems = CheckFiles(config, map[string][]byte{"ca.pem": []byte("no certificate")})
	if len(problems) != 1 || !strings.Contains(problems[0], "holds no PEM certificate") {
		t.Errorf("a CA without a certificate: %v", problems)
	}
	if problems := CheckFiles(config, map[string][]byte{"ca.pem": testCAPEM(t)}); len(problems) != 0 {
		t.Errorf("a CA: %v", problems)
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
