// Package jointoken makes and reads the one-time tokens a host joins the
// cluster with (clowder host join-token, clowderd join). A token is an ID
// and a secret. The state keeps only the secret's Hash, under the ID, until
// a join uses the token or it expires; whoever reads the state therefore
// cannot join with it.
//
// The joining host trusts the cluster's CA by a proof made with the hash
// (pki.TrustProof): only someone with the token or the state can make it.
// It then sends the token itself, over TLS checked against that CA, and
// the leader compares its hash.
package jointoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// idSize and secretSize are in bytes. The ID only names the token, so the
// agent sends the proof of one token, not of every open one; the secret's
// 256 bits are what nobody guesses.
const (
	idSize     = 8
	secretSize = 32
)

// Token is a join token.
type Token struct {
	// ID names the token in the state and in the first contact.
	ID     string
	secret []byte
}

// New makes a token.
func New() (Token, error) {
	id := make([]byte, idSize)
	secret := make([]byte, secretSize)
	if _, err := rand.Read(id); err != nil {
		return Token{}, fmt.Errorf("make a join token: %w", err)
	}
	if _, err := rand.Read(secret); err != nil {
		return Token{}, fmt.Errorf("make a join token: %w", err)
	}
	return Token{ID: hex.EncodeToString(id), secret: secret}, nil
}

// String is the token as the user copies it: the ID, a dot, the secret.
func (t Token) String() string {
	return t.ID + "." + base64.RawURLEncoding.EncodeToString(t.secret)
}

// Parse reads a token as String writes it.
func Parse(text string) (Token, error) {
	id, encoded, ok := strings.Cut(strings.TrimSpace(text), ".")
	if !ok {
		return Token{}, errors.New("the join token has no dot: it is <id>.<secret>")
	}
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != idSize {
		return Token{}, errors.New("the join token's ID is not 16 hexadecimal digits")
	}
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(secret) != secretSize {
		return Token{}, errors.New("the join token's secret is cut short or not a join token's")
	}
	return Token{ID: id, secret: secret}, nil
}

// Hash is what the state keeps of the token: SHA-256 of its secret, in hex.
// The secret is random, so no salt is needed.
func (t Token) Hash() string {
	sum := sha256.Sum256(t.secret)
	return hex.EncodeToString(sum[:])
}

// ProofKey is the key of the trust proof for a token with this hash: the
// agent has the hash, the joining host makes it from the token.
func ProofKey(hash string) []byte {
	return []byte("clowder join\n" + hash)
}
