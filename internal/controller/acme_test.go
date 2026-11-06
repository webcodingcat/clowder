package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// fakeCA is an ACME directory as small as x/crypto/acme needs: it takes
// every signature as valid, validates a challenge by asking for it as a CA
// would, and issues certificates of 90 days from its own key.
type fakeCA struct {
	t      *testing.T
	server *httptest.Server
	key    *ecdsa.PrivateKey
	root   *x509.Certificate
	// challenges is where the agents serve the answers: "127.0.0.1:<port>"
	challenges string

	mu         sync.Mutex
	accounts   int
	orders     int
	failOrders bool
	validated  bool
	chain      []byte // the PEM chain of the last order
}

const fakeLifetime = 90 * 24 * time.Hour

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := &fakeCA{t: t, key: key, root: root}
	ca.server = httptest.NewTLSServer(http.HandlerFunc(ca.serve))
	t.Cleanup(ca.server.Close)
	return ca
}

// caCalls is what the CA saw.
type caCalls struct {
	accounts, orders int
	validated        bool
	chain            []byte
}

// calls reads what the CA saw under its lock: its handler runs in the
// server's goroutines.
func (ca *fakeCA) calls() caCalls {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return caCalls{accounts: ca.accounts, orders: ca.orders, validated: ca.validated, chain: ca.chain}
}

// trust is the PEM certificate of the CA's TLS server, as acme.ca holds it.
func (ca *fakeCA) trust() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.server.Certificate().Raw})
}

func (ca *fakeCA) serve(w http.ResponseWriter, r *http.Request) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	base := ca.server.URL
	w.Header().Set("Replay-Nonce", strconv.FormatInt(time.Now().UnixNano(), 36))
	token := "token" + strconv.Itoa(ca.orders)
	order := func(status string) map[string]any {
		return map[string]any{
			"status": status, "authorizations": []string{base + "/authz"}, "finalize": base + "/finalize",
			"certificate": base + "/certificate",
		}
	}
	switch r.URL.Path {
	case "/directory":
		writeAs(w, http.StatusOK, map[string]string{
			"newNonce": base + "/nonce", "newAccount": base + "/account", "newOrder": base + "/order",
		})
	case "/nonce":
		w.WriteHeader(http.StatusOK)
	case "/account":
		ca.accounts++
		w.Header().Set("Location", base+"/account/"+strconv.Itoa(ca.accounts))
		writeAs(w, http.StatusCreated, map[string]string{"status": "valid"})
	case "/order":
		if ca.failOrders {
			w.Header().Set("Content-Type", "application/problem+json")
			writeAs(w, http.StatusForbidden, map[string]string{"type": "urn:ietf:params:acme:error:rejectedIdentifier"})
			return
		}
		ca.orders++
		ca.validated = false
		w.Header().Set("Location", base+"/order/current")
		writeAs(w, http.StatusCreated, order("pending"))
	case "/order/current":
		status := "pending"
		if ca.validated {
			status = "ready"
		}
		w.Header().Set("Location", base+"/order/current")
		writeAs(w, http.StatusOK, order(status))
	case "/authz":
		status := "pending"
		if ca.validated {
			status = "valid"
		}
		writeAs(w, http.StatusOK, map[string]any{
			"status": status, "identifier": map[string]string{"type": "dns", "value": "shop.example.com"},
			"challenges": []map[string]string{
				{"type": "dns-01", "url": base + "/dns-challenge", "token": token, "status": "pending"},
				{"type": "http-01", "url": base + "/challenge", "token": token, "status": "pending"},
			},
		})
	case "/challenge":
		ca.validated = ca.answered(token)
		writeAs(w, http.StatusOK, map[string]string{"type": "http-01", "url": base + "/challenge", "token": token, "status": "processing"})
	case "/finalize":
		ca.chain = ca.issue(r)
		w.Header().Set("Location", base+"/order/current")
		writeAs(w, http.StatusOK, order("valid"))
	case "/certificate":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(ca.chain)
	default:
		http.NotFound(w, r)
	}
}

// answered asks for the challenge as the CA would: over plain HTTP, the
// domain as the host.
func (ca *fakeCA) answered(token string) bool {
	request, err := http.NewRequest(http.MethodGet, "http://"+ca.challenges+ingress.ChallengePath+token, http.NoBody)
	if err != nil {
		return false
	}
	request.Host = "shop.example.com"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return err == nil && response.StatusCode == http.StatusOK && strings.HasPrefix(string(body), token+".")
}

// issue signs the CSR of a finalize request; it returns the leaf and the
// CA's certificate as PEM.
func (ca *fakeCA) issue(r *http.Request) []byte {
	var signed struct{ Payload string }
	var payload struct{ CSR string }
	body, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(body, &signed)
	}
	var decoded []byte
	if err == nil {
		decoded, err = base64.RawURLEncoding.DecodeString(signed.Payload)
	}
	if err == nil {
		err = json.Unmarshal(decoded, &payload)
	}
	var der []byte
	if err == nil {
		der, err = base64.RawURLEncoding.DecodeString(payload.CSR)
	}
	var request *x509.CertificateRequest
	if err == nil {
		request, err = x509.ParseCertificateRequest(der)
	}
	if err != nil {
		ca.t.Errorf("read the finalize request: %v", err)
		return nil
	}
	now := time.Now().Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(ca.orders) + 1), DNSNames: request.DNSNames,
		NotBefore: now, NotAfter: now.Add(fakeLifetime),
	}
	leaf, err := x509.CreateCertificate(rand.Reader, template, ca.root, request.PublicKey, ca.key)
	if err != nil {
		ca.t.Errorf("issue: %v", err)
		return nil
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.root.Raw})...)
}

func writeAs(w http.ResponseWriter, status int, body any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// prefixKeys "encrypts" by prefixing, so a test sees what was encrypted.
type prefixKeys struct{}

func (prefixKeys) Encrypt(plaintext []byte) (state.Encrypted, error) {
	return append(state.Encrypted("encrypted:"), plaintext...), nil
}

func (prefixKeys) Decrypt(encrypted state.Encrypted) ([]byte, error) {
	plaintext, ok := bytes.CutPrefix(encrypted, []byte("encrypted:"))
	if !ok {
		return nil, errors.New("not encrypted")
	}
	return plaintext, nil
}

// newACMERunner is a leader with the stack shop, whose route is
// shop.example.com, on a cluster whose Floating IP is 127.0.0.1 and whose
// ACME directory is ca. The agents' challenge server listens there, as
// Traefik's web entrypoint would, and the domain resolves to it.
func newACMERunner(t *testing.T, ca *fakeCA) (*acmeRunner, *leader) {
	t.Helper()
	l := &leader{t: t, state: &state.State{}, changed: make(chan struct{})}
	l.apply(state.KindBootstrap, state.LeaderActor, state.Bootstrap{
		Keys:        state.ClusterKeys{CACertificate: []byte("ca"), CAKey: state.Encrypted("k"), SealingKey: state.Encrypted("a")},
		ClusterName: "prod",
		Host: state.Host{
			ID: 0, Name: "node0", ServerID: 40, Underlay: netip.MustParseAddr("10.0.0.2"),
			PublicAddress: netip.MustParseAddr("198.51.100.2"), WGPublicKey: "wg", CertificateSerial: "s0",
			CertificateNotAfter: time.Now().AddDate(1, 0, 0),
		},
	})
	putSystemSecret(l, "hetzner", map[string]string{"token": "t"})
	// the fake CA's TLS certificate is trusted only through the inline acme.ca
	putConfig(l, clusterspec.Config{
		Hetzner:        clusterspec.Hetzner{TokenSecretRef: names.SecretKeyRef{Name: "hetzner", Key: "token"}},
		PrivateNetwork: "10.0.0.0/16",
		Ingress:        clusterspec.Ingress{FloatingIP: "127.0.0.1"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
		ACME:           clusterspec.ACME{Directory: ca.server.URL + "/directory", CA: string(ca.trust()), Email: "ops@example.com"},
	})
	putResource(l, resource.KindStack, "shop", stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", HTTP: &stackspec.HTTP{Host: "shop.example.com", Port: 80}},
	}})

	challenges := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var answer string
		l.Read(func(current *state.State) {
			answer = current.Challenges[strings.TrimPrefix(r.URL.Path, ingress.ChallengePath)]
		})
		if answer == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(challenges.Close)
	address, err := url.Parse(challenges.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(address.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	ca.challenges = address.Host
	runner := &acmeRunner{
		cluster: l, propose: l.propose, keys: prefixKeys{},
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		httpPort: uint16(port), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), retries: map[string]retry{},
	}
	return runner, l
}

func TestACertificateIsOrderedThroughTheChallengeTheAgentsServe(t *testing.T) {
	ca := newFakeCA(t)
	runner, l := newACMERunner(t, ca)
	l.apply(state.KindPutACMEChallenge, state.LeaderActor, state.PutACMEChallenge{Token: "old", KeyAuthorization: "old.left"})
	runner.pass(context.Background())

	var certificate *state.Certificate
	var account *state.ACMEAccount
	var challenges map[string]string
	l.Read(func(current *state.State) {
		certificate, account, challenges = current.Certificates["shop.example.com"], current.ACMEAccount, current.Challenges
	})
	calls := ca.calls()
	if certificate == nil || !calls.validated {
		t.Fatalf("no certificate (validated %v, retries %v)", calls.validated, runner.retries)
	}
	if certificate.Lifetime() != fakeLifetime || !bytes.Equal(certificate.Chain, calls.chain) {
		t.Errorf("certificate %+v", certificate)
	}
	key, err := prefixKeys{}.Decrypt(certificate.Key)
	if block, _ := pem.Decode(key); err != nil || block == nil || block.Type != "PRIVATE KEY" {
		t.Errorf("the certificate's key: %q, %v", key, err)
	}
	if account == nil || account.URL != ca.server.URL+"/account/1" || account.Directory != ca.server.URL+"/directory" {
		t.Errorf("account %+v", account)
	}
	if len(challenges) != 0 {
		t.Errorf("challenges left: %v", challenges)
	}

	// nothing is due: no second order, and the account is kept
	runner.pass(context.Background())
	if calls := ca.calls(); calls.orders != 1 || calls.accounts != 1 {
		t.Errorf("%d orders and %d accounts, want one each", calls.orders, calls.accounts)
	}

	deleteResource(l, resource.KindStack, "shop")
	runner.pass(context.Background())
	l.Read(func(current *state.State) {
		if len(current.Certificates) != 0 {
			t.Errorf("the certificate of a domain no stack names stayed: %v", current.Certificates)
		}
	})
}

func TestADomainThatDoesNotPointHereIsNotOrdered(t *testing.T) {
	ca := newFakeCA(t)
	runner, _ := newACMERunner(t, ca)
	runner.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("198.51.100.1")}, nil
	}
	runner.pass(context.Background())
	retry := runner.retries["shop.example.com"]
	if calls := ca.calls(); calls.orders != 0 || calls.accounts != 0 {
		t.Errorf("%d orders and %d accounts, want none", calls.orders, calls.accounts)
	}
	if wait := time.Until(retry.at); wait < 9*time.Minute || wait > dnsRetry || retry.backoff != 0 {
		t.Errorf("retry %+v: want in 10 minutes, no backoff", retry)
	}
}

func TestFailedOrdersBackOffUpToADay(t *testing.T) {
	ca := newFakeCA(t)
	ca.failOrders = true
	runner, _ := newACMERunner(t, ca)
	for _, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		runner.pass(context.Background())
		retry := runner.retries["shop.example.com"]
		if wait := time.Until(retry.at); wait < want-time.Minute || wait > want {
			t.Fatalf("waits %v, want %v", wait, want)
		}
		retry.at = time.Time{} // the wait is over
		runner.retries["shop.example.com"] = retry
	}
	runner.retries["shop.example.com"] = retry{backoff: maxBackoff}
	runner.pass(context.Background())
	if retry := runner.retries["shop.example.com"]; retry.backoff != maxBackoff || time.Until(retry.at) > maxBackoff {
		t.Errorf("retry %+v: want at most a day", retry)
	}
	if calls := ca.calls(); calls.accounts != 1 {
		t.Errorf("%d accounts, want one", calls.accounts)
	}
}

func TestRenewalComesWhenAThirdOfTheLifetimeIsLeftOrAsConfigured(t *testing.T) {
	day := 24 * time.Hour
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	certificate := &state.Certificate{Domain: "shop.example.com", NotBefore: start, NotAfter: start.Add(90 * day)}
	for _, test := range []struct {
		renewBefore string
		now         time.Time
		due         bool
	}{
		{"", start.Add(59 * day), false},
		{"", start.Add(60 * day), true},
		{"10d", start.Add(60 * day), false},
		{"10d", start.Add(80 * day), true},
		// not shorter than the lifetime: a third, as by default
		{"90d", start.Add(59 * day), false},
		{"120d", start.Add(60 * day), true},
	} {
		config := clusterspec.ACME{RenewBefore: test.renewBefore}
		if due := renewalDue(certificate, config, test.now); due != test.due {
			t.Errorf("renew_before %q on day %v: due %v", test.renewBefore, test.now.Sub(start)/day, due)
		}
	}
	if _, ignored := renewBefore(certificate, clusterspec.ACME{RenewBefore: "90d"}); !ignored {
		t.Error("a renew_before as long as the lifetime was not ignored")
	}
	if !renewalDue(nil, clusterspec.ACME{}, start) {
		t.Error("a domain without a certificate is not due")
	}
	routed := func(routes ...stackspec.HTTP) *state.Stack {
		spec := stackspec.Spec{Services: map[string]stackspec.Service{}}
		for index, route := range routes {
			spec.Services[fmt.Sprint("s", index)] = stackspec.Service{HTTP: &route}
		}
		return &state.Stack{Spec: spec}
	}
	if fmt.Sprint(routeDomains(&state.State{Stacks: map[string]*state.Stack{
		"a": routed(stackspec.HTTP{Host: "shop.example.com"}, stackspec.HTTP{Host: "shop.example.com", Path: "/api"}),
		"b": routed(stackspec.HTTP{Host: "Blog.example.com"}),
	}})) != "[blog.example.com shop.example.com]" {
		t.Error("the domains of the routes")
	}
}

func TestTheSelfCheckAsksTheIngressHostsTraefik(t *testing.T) {
	runner := &acmeRunner{httpPort: 80}
	floatingIP := netip.MustParseAddr("203.0.113.10")
	current := &state.State{}
	if got := runner.checkAddress(current, floatingIP); got.String() != "203.0.113.10:80" {
		t.Errorf("without an ingress host: %s", got)
	}
	ingressHost := state.HostID(2)
	current.Ingress = &ingressHost
	if got := runner.checkAddress(current, floatingIP); got.String() != "10.77.0.3:7480" {
		t.Errorf("with host 2 as the ingress host: %s", got)
	}
}
