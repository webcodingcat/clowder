package controller

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/units"
)

// Keys seals and opens the state's secrets (datakey.Key).
type Keys interface {
	Seal(plaintext []byte) (state.Sealed, error)
	Open(sealed state.Sealed) ([]byte, error)
}

const (
	// dnsRetry is how long a domain whose DNS does not point at the
	// Floating IP waits: its order never reached the CA, so it does not
	// count towards the backoff.
	dnsRetry = 10 * time.Minute
	// firstBackoff and maxBackoff bound the wait after a failed order: it
	// doubles from the first to the last, well below Let's Encrypt's limit
	// on failed validations.
	firstBackoff = 10 * time.Minute
	maxBackoff   = 24 * time.Hour
	// orderTimeout bounds one order, from the account to the certificate.
	orderTimeout = 10 * time.Minute
	// selfCheckTimeout is how long the leader waits for its own Traefik to
	// answer a challenge before it gives the order up.
	selfCheckTimeout = 2 * time.Minute
	// selfCheckPause is the wait between two tries of the self-check.
	selfCheckPause = 2 * time.Second
	// requestTimeout bounds one DNS lookup and one request of the
	// self-check.
	requestTimeout = 10 * time.Second
	// cleanUpTimeout bounds removing a challenge after its order ended,
	// also one whose context ended.
	cleanUpTimeout = 30 * time.Second
)

// retry is when a domain may be ordered again, and how long it waits after
// its next failure.
type retry struct {
	at      time.Time
	backoff time.Duration
}

// acmeRunner orders and renews the HTTP routes' certificates while this
// agent leads. Its fields besides the state are what tests replace.
type acmeRunner struct {
	cluster Cluster
	propose Propose
	keys    Keys
	// lookup resolves a domain (net.Resolver.LookupNetIP).
	lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)
	// httpPort is where the self-check reaches Traefik on the Floating IP:
	// its web entrypoint.
	httpPort uint16
	logger   *slog.Logger
	// retries are the domains that wait after a failure, kept in memory: a
	// new leader starts again at the first backoff
	retries map[string]retry
}

// RunACME keeps a certificate for each domain of an HTTP route while this
// agent leads, until ctx ends: at a change of the state and every interval.
// It orders one with HTTP-01, renews it before it ends and deletes those no
// stack names any more.
func RunACME(ctx context.Context, cluster Cluster, propose Propose, keys Keys, logger *slog.Logger) {
	runner := &acmeRunner{
		cluster: cluster, propose: propose, keys: keys, lookup: net.DefaultResolver.LookupNetIP,
		httpPort: 80, logger: logger, retries: map[string]retry{},
	}
	for {
		changed := cluster.Changed()
		if cluster.IsLeader() {
			runner.pass(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(interval):
		}
	}
}

// pass deletes what no order needs any more, then orders each certificate
// that is missing or due for renewal, one domain after the other.
func (r *acmeRunner) pass(ctx context.Context) {
	var copied *state.State
	var err error
	r.cluster.Read(func(current *state.State) { copied, err = current.Copy() })
	if err != nil {
		r.logger.Error("keep the certificates", "error", err)
		return
	}
	if copied.Config.Revision == 0 {
		return // no cluster.yaml yet: no Floating IP
	}
	floatingIP, err := netip.ParseAddr(copied.Config.Spec.Ingress.FloatingIP)
	if err != nil {
		r.logger.Error("keep the certificates", "error", err)
		return
	}
	// a pass orders one domain after the other, so no order of this leader
	// is in progress now: what is left is from a leader that stopped
	for _, token := range slices.Sorted(maps.Keys(copied.Challenges)) {
		r.commit(ctx, state.KindDeleteACMEChallenge, state.DeleteACMEChallenge{Token: token})
	}
	domains := routeDomains(copied)
	for _, domain := range slices.Sorted(maps.Keys(copied.Certificates)) {
		if !slices.Contains(domains, domain) {
			r.logger.Info("deleting the certificate of a domain no stack names", "domain", domain)
			r.commit(ctx, state.KindDeleteCertificate, state.DeleteCertificate{Domain: domain})
		}
	}

	for domain := range r.retries {
		if !slices.Contains(domains, domain) {
			delete(r.retries, domain)
		}
	}

	config := copied.Config.Spec.ACME
	var client *acme.Client // made for the first domain that needs one
	for _, domain := range domains {
		// an order takes minutes: a leader that lost its lead stops
		if !r.cluster.IsLeader() {
			return
		}
		now := time.Now()
		if now.Before(r.retries[domain].at) {
			continue // waits after a failure
		}
		certificate := copied.Certificates[domain]
		if !renewalDue(certificate, config, now) {
			continue
		}
		if certificate != nil {
			if _, ignored := renewBefore(certificate, config); ignored {
				r.logger.Warn("acme.renew_before is not shorter than the certificate's lifetime; renewing when a third is left",
					"domain", domain, "renew_before", config.RenewBefore, "lifetime", certificate.Lifetime())
			}
		}
		if err := r.checkDNS(ctx, domain, floatingIP); err != nil {
			r.retries[domain] = retry{at: now.Add(dnsRetry), backoff: r.retries[domain].backoff}
			r.logger.Warn("not ordering a certificate: the domain does not point at the Floating IP",
				"domain", domain, "error", err, "retry_in", dnsRetry)
			continue
		}
		if client == nil {
			connected, err := r.connect(ctx, copied)
			if err != nil {
				// the CA's limit on new accounts is tight: one try per pass,
				// and the domain waits as after a failed order
				r.failed(domain, now, err)
				return
			}
			client = connected
		}
		if err := r.order(ctx, client, domain, floatingIP); err != nil {
			r.failed(domain, now, err)
			continue
		}
		delete(r.retries, domain)
		r.logger.Info("got a certificate", "domain", domain)
	}
}

// failed makes a domain wait after a failure that reached the CA: the
// first backoff, then twice the last, at most maxBackoff.
func (r *acmeRunner) failed(domain string, now time.Time, err error) {
	backoff := max(r.retries[domain].backoff, firstBackoff)
	r.retries[domain] = retry{at: now.Add(backoff), backoff: min(2*backoff, maxBackoff)}
	r.logger.Warn("ordering a certificate failed", "domain", domain, "error", err, "retry_in", backoff)
}

// commit writes a command that needs no answer; a failure is logged, and
// the next pass tries again.
func (r *acmeRunner) commit(ctx context.Context, kind string, payload any) {
	if err := commit(ctx, r.propose, kind, payload); err != nil {
		r.logger.Warn("keep the certificates", "command", kind, "error", err)
	}
}

// routeDomains are the domains of the stacks' HTTP routes, sorted.
func routeDomains(current *state.State) []string {
	domains := map[string]bool{}
	for _, stack := range current.Stacks {
		for _, route := range stack.Routes {
			// a route is "<host><path>" with the host in lowercase
			host, _, _ := strings.Cut(route, "/")
			domains[host] = true
		}
	}
	return slices.Sorted(maps.Keys(domains))
}

// renewBefore is how long before its end a certificate is renewed: a third
// of its lifetime, or acme.renew_before while that is shorter than the
// lifetime. A longer one would renew every new certificate at once, so it
// is ignored for that certificate, and ignored is true. Both come from the
// state alone, so no setting and no change of leader can renew in a loop.
func renewBefore(certificate *state.Certificate, config clusterdir.ACME) (before time.Duration, ignored bool) {
	lifetime := certificate.Lifetime()
	byDefault := lifetime / 3
	if config.RenewBefore == "" {
		return byDefault, false
	}
	configured, err := units.ParseDuration(config.RenewBefore)
	if err != nil {
		return byDefault, false // cluster.yaml's check refuses one that does not read
	}
	if configured >= lifetime {
		return byDefault, true
	}
	return configured, false
}

// renewalDue tells whether a domain needs a certificate now: it has none,
// or its renewal window began.
func renewalDue(certificate *state.Certificate, config clusterdir.ACME, now time.Time) bool {
	if certificate == nil {
		return true
	}
	before, _ := renewBefore(certificate, config)
	return !now.Before(certificate.NotAfter.Add(-before))
}

// checkDNS makes sure a domain resolves to the Floating IP and nothing
// else before the CA is asked: a validation that fails counts against the
// CA's limits, and one that reaches another address fails.
func (r *acmeRunner) checkDNS(ctx context.Context, domain string, floatingIP netip.Addr) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	addresses, err := r.lookup(ctx, "ip", domain)
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return fmt.Errorf("%s resolves to no address", domain)
	}
	for _, address := range addresses {
		if address.Unmap() != floatingIP {
			return fmt.Errorf("%s resolves to %s, not only to the Floating IP %s", domain, address, floatingIP)
		}
	}
	return nil
}

// connect makes the ACME client with the cluster's account at the
// directory, and makes the account first if there is none there yet.
func (r *acmeRunner) connect(ctx context.Context, copied *state.State) (*acme.Client, error) {
	config := copied.Config.Spec.ACME
	httpClient, err := acmeHTTPClient(copied.Config.Files[config.CA])
	if err != nil {
		return nil, err
	}
	client := &acme.Client{DirectoryURL: config.DirectoryURL(), HTTPClient: httpClient, UserAgent: "clowder"}
	if account := copied.ACMEAccount; account != nil && account.Directory == client.DirectoryURL {
		der, err := r.keys.Open(account.Key)
		if err != nil {
			return nil, fmt.Errorf("open the ACME account's key: %w", err)
		}
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("read the ACME account's key: %w", err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("the ACME account's key is a %T, which cannot sign", key)
		}
		client.Key, client.KID = signer, acme.KeyID(account.URL)
		return client, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	client.Key = key
	var contact []string
	if config.Email != "" {
		contact = []string{"mailto:" + config.Email}
	}
	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	// the CA's terms are accepted for the operator; the README says so
	account, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS)
	if err != nil {
		return nil, fmt.Errorf("make an ACME account at %s: %w", client.DirectoryURL, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	sealed, err := r.keys.Seal(der)
	if err != nil {
		return nil, err
	}
	err = commit(ctx, r.propose, state.KindPutACMEAccount, state.PutACMEAccount{Account: state.ACMEAccount{
		Directory: client.DirectoryURL, URL: account.URI, Key: sealed,
	}})
	if err != nil {
		return nil, fmt.Errorf("record the ACME account: %w", err)
	}
	r.logger.Info("made an ACME account", "directory", client.DirectoryURL)
	return client, nil
}

// acmeHTTPClient trusts the system's CAs and the one of acme.ca, if any:
// a test CA such as Pebble's.
func acmeHTTPClient(caPEM []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool() // no system pool: only acme.ca, if any
	}
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("acme.ca holds no PEM certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport}, nil
}

// order gets a new certificate for a domain and records it.
func (r *acmeRunner) order(ctx context.Context, client *acme.Client, domain string, floatingIP netip.Addr) error {
	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}
	for _, url := range order.AuthzURLs {
		if err := r.authorize(ctx, client, url, domain, floatingIP); err != nil {
			return err
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("wait for the order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{domain}}, key)
	if err != nil {
		return err
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("finalize the order: %w", err)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return fmt.Errorf("read the certificate: %w", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return fmt.Errorf("the CA's certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	sealed, err := r.keys.Seal(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return err
	}
	var chainPEM []byte
	for _, der := range chain {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return commit(ctx, r.propose, state.KindPutCertificate, state.PutCertificate{Certificate: state.Certificate{
		Domain: domain, Key: sealed, Chain: chainPEM, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
	}})
}

// authorize proves to the CA that the cluster serves the domain: it
// publishes the answer to the HTTP-01 challenge in the state, where every
// agent serves it, checks that it comes back through the Floating IP, and
// only then lets the CA validate.
func (r *acmeRunner) authorize(ctx context.Context, client *acme.Client, url, domain string, floatingIP netip.Addr) error {
	authorization, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("read the authorization: %w", err)
	}
	if authorization.Status == acme.StatusValid {
		return nil // validated by an earlier order
	}
	var challenge *acme.Challenge
	for _, offered := range authorization.Challenges {
		if offered.Type == "http-01" {
			challenge = offered
		}
	}
	if challenge == nil {
		return fmt.Errorf("the CA offers no HTTP-01 challenge for %s", domain)
	}
	keyAuthorization, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}
	err = commit(ctx, r.propose, state.KindPutACMEChallenge, state.PutACMEChallenge{
		Token: challenge.Token, KeyAuthorization: keyAuthorization,
	})
	if err != nil {
		return fmt.Errorf("publish the challenge: %w", err)
	}
	defer func() {
		// also when the order's context ended
		cleanUp, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanUpTimeout)
		defer cancel()
		r.commit(cleanUp, state.KindDeleteACMEChallenge, state.DeleteACMEChallenge{Token: challenge.Token})
	}()
	if err := r.selfCheck(ctx, floatingIP, domain, challenge.Token, keyAuthorization); err != nil {
		return err
	}
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept the challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authorization.URI); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	return nil
}

// selfCheck asks Traefik on the Floating IP for the challenge, as the CA
// will, until the answer comes back; at most selfCheckTimeout. The Floating
// IP is on every host's interface, so this reaches the leader's own
// Traefik: on one host the ingress host's.
func (r *acmeRunner) selfCheck(ctx context.Context, floatingIP netip.Addr, domain, token, keyAuthorization string) error {
	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()
	url := "http://" + netip.AddrPortFrom(floatingIP, r.httpPort).String() + ingress.ChallengePath + token
	client := &http.Client{
		// no proxy from the environment: the request must reach the
		// Floating IP itself
		Transport: &http.Transport{},
		// the CA does not follow Traefik's redirect to https either
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       requestTimeout,
	}
	defer client.CloseIdleConnections()
	for {
		err := fetchChallenge(ctx, client, url, domain, keyAuthorization)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the challenge did not come back through the Floating IP within %v: %w", selfCheckTimeout, err)
		case <-time.After(selfCheckPause):
		}
	}
}

// fetchChallenge asks for a challenge once, with the domain as the host.
func fetchChallenge(ctx context.Context, client *http.Client, url, domain, want string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	request.Host = domain
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }() // read in full below
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<10))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(body) != want {
		return fmt.Errorf("%s answered %s with %q", url, response.Status, body)
	}
	return nil
}
