//go:build live

package live

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/resource"
)

// TestAccess checks the CLI's certificates: the name in a certificate is
// its actor in the agents' log and in the audit, and the CLI warns before
// a certificate ends.
func TestAccess(t *testing.T) {
	server, tofu := baseline(t, threeHosts)
	runSteps(t, []step{
		{"a named certificate's actor is in the log and the audit", func(t *testing.T) { namedActor(t, server, tofu) }},
		{"the CLI warns before its certificate ends", func(t *testing.T) { warnsBeforeTheEnd(t, tofu) }},
	})
}

// auditorName is the name in the certificate of namedActor.
const auditorName = "live-auditor"

// namedActor gets credentials with a name, applies and deletes a Secret
// with them, and finds the actor cli/<name> where an operator looks for it:
// in the journal of the agent that took each write, in the journal of the
// host that issued the certificate, and in the cluster's audit.
func namedActor(t *testing.T, server host, tofu outputs) {
	since := server.must(t, "date '+%Y-%m-%d %H:%M:%S'")
	named := filepath.Join(workDir, "named-config")
	issued := server.must(t, "clowderd credentials --name "+auditorName+" 2>/dev/null") // stdout alone
	if err := os.WriteFile(named, []byte(issued), 0o600); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(workDir, "audited.yaml")
	secret := document(resource.KindSecret, "audited", "stringData: {"+stackSecretKey+": audited}")
	if err := os.WriteFile(secretFile, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClowder(t, "--config", named, "apply", "-f", secretFile)
	mustClowder(t, "--config", named, "delete", "-f", secretFile)

	actor := `"actor":"cli/` + auditorName + `"`
	journal := func(server host, message string) int {
		t.Helper()
		count := server.must(t, fmt.Sprintf("journalctl -u clowderd --since '%s' -o cat | grep -F '%s' | grep -cF '%s' || true",
			since, message, actor))
		return atoi(t, count)
	}
	if journal(server, "issued a CLI certificate") != 1 {
		t.Errorf("the first host's journal does not log the certificate it issued with the actor cli/%s", auditorName)
	}
	requests := 0
	for name, other := range scenarioHosts(server, tofu) {
		if labLayout(t).hasRole(name, "control-plane") {
			requests += journal(other, "a CLI request")
		}
	}
	// a dry run that the CLI sends first is a request too, but no command
	if requests < 2 {
		t.Errorf("the agents' journals log %d CLI requests of cli/%s, want the apply and the delete", requests, auditorName)
	}

	var entries []api.AuditEntry
	if err := namedClient(t, named).Get(context.Background(), "/v1/audit?limit=100", &entries); err != nil {
		t.Fatal(err)
	}
	audited := 0
	for _, entry := range entries {
		if entry.Actor == "cli/"+auditorName {
			audited++
		}
	}
	if audited != 2 {
		t.Errorf("the audit has %d commands of cli/%s, want 2: %+v", audited, auditorName, entries)
	}
}

// atoi reads a count that a host printed.
func atoi(t *testing.T, text string) int {
	t.Helper()
	var count int
	if _, err := fmt.Sscan(text, &count); err != nil {
		t.Fatalf("read the count %q: %v", text, err)
	}
	return count
}

// namedClient is an API client with the credentials of a file, for what
// the CLI has no command for. Endpoints, if any, replace those of the file,
// to ask one host.
func namedClient(t *testing.T, file string, endpoints ...string) *api.Client {
	t.Helper()
	config, err := clientconfig.Read(file)
	if err != nil {
		t.Fatal(err)
	}
	cliContext, err := config.Context("")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(cliContext.Certificate), []byte(cliContext.Key))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.ParseCertificate([]byte(cliContext.CA))
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) == 0 {
		endpoints = cliContext.Endpoints
	}
	return api.NewClient(endpoints, api.DefaultConnectTimeout, certificate, ca, buildinfo.Version)
}

// warnsBeforeTheEnd gives the CLI a certificate that ends in ten days and
// one that has ended. The cluster issues CLI certificates for a year, so
// the test makes these itself, signed by a CA of its own: the CLI warns
// before it connects, and the host then refuses the certificate. The
// test's own certificate, a year from its end, gets no warning.
func warnsBeforeTheEnd(t *testing.T, tofu outputs) {
	if output := mustClowder(t, "cluster", "status"); strings.Contains(output, "warning: the CLI certificate") {
		t.Errorf("a fresh certificate got a warning:\n%s", output)
	}
	own, err := clientconfig.Read(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	ownContext, err := own.Context("")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"ending", time.Now().Add(10*24*time.Hour + time.Hour), "ends on %s, in 10 days"},
		{"ended", time.Now().Add(-time.Hour), "ended on %s"},
	} {
		certificate, key := certificateUntil(t, test.notAfter)
		file := filepath.Join(workDir, test.name+"-config")
		config := clientconfig.Config{CurrentContext: test.name, Contexts: []clientconfig.Context{{
			Name: test.name, Endpoints: []string{tofu.PublicAddress.Value}, CA: ownContext.CA,
			Certificate: certificate, Key: key,
		}}}
		if err := clientconfig.Write(file, config); err != nil {
			t.Fatal(err)
		}
		output, err := clowder("--config", file, "cluster", "status")
		// the CLI prints the certificate's date, which is in UTC
		want := fmt.Sprintf("warning: the CLI certificate of context %s "+test.want, test.name,
			test.notAfter.UTC().Format(time.DateOnly))
		if err == nil || !strings.Contains(output, want) {
			t.Errorf("the CLI with a certificate that %s: %v, want %q and a refusal", test.name, err, want)
		}
	}
}

// certificateUntil is a CLI certificate and its key, in PEM, that ends at
// notAfter, signed by a throwaway CA that the cluster does not know.
func certificateUntil(t *testing.T, notAfter time.Time) (certificatePEM, keyPEM string) {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "throwaway"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := pki.EncodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(encodedKey)
}

// recoverAccess loses the test's credentials and gets new ones from root on
// the host, as after a stolen laptop: with --revoke-older the old
// certificate, as a thief would still hold it, is refused from then on.
func recoverAccess(t *testing.T, server host) {
	stolen, err := os.ReadFile(credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(credentialsFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := clowder("cluster", "status"); err == nil {
		t.Fatal("the CLI reached the cluster without credentials")
	}
	// stdout alone: must's output has the command's stderr too
	issued := server.must(t, "clowderd credentials --revoke-older 2>/dev/null")
	writeCredentials(t, []byte(issued))
	mustClowder(t, "cluster", "status")

	// the host that issued them refuses the old certificate at once, the
	// others once they applied the cutoff: the CLI may ask any of them
	writeCredentials(t, stolen)
	eventually(t, 30*time.Second, "the old certificate refused", func() error {
		if _, err := clowder("cluster", "status"); err == nil || !strings.Contains(err.Error(), "revoke-older") {
			return fmt.Errorf("the old certificate after --revoke-older: %w", err)
		}
		return nil
	})
	writeCredentials(t, []byte(issued))
	mustClowder(t, "cluster", "status")
}

// writeCredentials puts the test's credentials in place, as the CLI writes
// them.
func writeCredentials(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(credentialsFile(), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
