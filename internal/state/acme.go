package state

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
)

// ACMEAccount is the cluster's account at an ACME directory. An account key
// belongs to one directory, so the leader creates a new account when
// acme.directory changes.
type ACMEAccount struct {
	Directory string `json:"directory"`
	// URL is the account's ID at the directory (its "kid").
	URL string `json:"url"`
	// Key is the account key, PKCS #8 DER, encrypted with the data key.
	Key Encrypted `json:"key"`
}

// Certificate is the certificate of one domain.
type Certificate struct {
	Domain string `json:"domain"`
	// Key is the private key in PEM, encrypted. It is PEM because Traefik
	// reads it. The account key is not PEM, because only the leader reads
	// it.
	Key Encrypted `json:"key"`
	// Chain is the certificate and its intermediates, PEM.
	Chain     []byte    `json:"chain"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// Lifetime is how long the certificate is valid.
func (c *Certificate) Lifetime() time.Duration {
	return c.NotAfter.Sub(c.NotBefore)
}

// PutACMEAccount records the account that the leader created.
type PutACMEAccount struct {
	Account ACMEAccount `json:"account"`
}

func applyPutACMEAccount(s *State, _ Command, payload PutACMEAccount) (func(), error) {
	account := payload.Account
	if account.Directory == "" || account.URL == "" || len(account.Key) == 0 {
		return nil, errors.New("an account needs its directory, URL and key")
	}
	return func() { s.ACMEAccount = &account }, nil
}

// base64URL matches what RFC 8555 allows in a token and a key
// authorization.
var base64URL = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// PutACMEChallenge publishes the answer to an HTTP-01 challenge. Every
// agent serves it, so whichever host holds the Floating IP can answer.
type PutACMEChallenge struct {
	Token            string `json:"token"`
	KeyAuthorization string `json:"key_authorization"`
}

func applyPutACMEChallenge(s *State, _ Command, payload PutACMEChallenge) (func(), error) {
	// the token becomes part of a URL path. In RFC 8555, the key
	// authorization is the token, a dot and the account key's thumbprint.
	thumbprint, ok := strings.CutPrefix(payload.KeyAuthorization, payload.Token+".")
	if !base64URL.MatchString(payload.Token) || !ok || !base64URL.MatchString(thumbprint) {
		return nil, fmt.Errorf("token %q with key authorization %q is not an HTTP-01 answer",
			payload.Token, payload.KeyAuthorization)
	}
	return func() { s.Challenges[payload.Token] = payload.KeyAuthorization }, nil
}

// DeleteACMEChallenge removes an answer once its order has ended.
type DeleteACMEChallenge struct {
	Token string `json:"token"`
}

func applyDeleteACMEChallenge(s *State, _ Command, payload DeleteACMEChallenge) (func(), error) {
	return func() { delete(s.Challenges, payload.Token) }, nil
}

// PutCertificate records a domain's new certificate, replacing the one it
// had.
type PutCertificate struct {
	Certificate Certificate `json:"certificate"`
}

func applyPutCertificate(s *State, _ Command, payload PutCertificate) (func(), error) {
	certificate := payload.Certificate
	switch {
	case !names.ValidDomain(certificate.Domain) || certificate.Domain != strings.ToLower(certificate.Domain):
		return nil, fmt.Errorf("%q is not a domain in lowercase", certificate.Domain)
	case len(certificate.Key) == 0 || len(certificate.Chain) == 0:
		return nil, fmt.Errorf("the certificate of %s needs its key and chain", certificate.Domain)
	case !certificate.NotAfter.After(certificate.NotBefore):
		return nil, fmt.Errorf("the certificate of %s ends before it starts", certificate.Domain)
	}
	return func() { s.Certificates[certificate.Domain] = &certificate }, nil
}

// DeleteCertificate removes the certificate of a domain that no stack
// names.
type DeleteCertificate struct {
	Domain string `json:"domain"`
}

func applyDeleteCertificate(s *State, _ Command, payload DeleteCertificate) (func(), error) {
	return func() { delete(s.Certificates, payload.Domain) }, nil
}
