// Package jointoken makes and reads the one-time tokens that a host joins
// the cluster with (clowder host join-token, clowderd join). A token is an
// ID and a secret. The state keeps only the secret's Hash, under the ID,
// until a join uses the token or it expires. So whoever reads the state
// cannot join with it.
//
// The joining host trusts the cluster's CA because of a proof made with
// the hash (pki.TrustProof). Only someone with the token or the state can
// make that proof. The host then sends the token itself, over TLS checked
// against that CA, and the leader compares its hash.
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

// idSize and secretSize are in bytes. The ID only names the token, so that
// the agent sends the proof for one token, not for every open one. The
// secret's 256 bits are the part that nobody can guess.
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

// String returns the token in the form that the user copies: the ID, a
// dot, and the secret.
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

// Hash returns what the state keeps of the token: the SHA-256 of its
// secret, in hex. The secret is random, so no salt is needed.
func (t Token) Hash() string {
	sum := sha256.Sum256(t.secret)
	return hex.EncodeToString(sum[:])
}

// ProofKey returns the key of the trust proof for a token with this hash.
// The agent has the hash, and the joining host computes it from the token.
// The key is not a list of fields to MAC, and the hash always has 64
// hexadecimal digits. So, unlike in digest.WriteParts, no part needs its
// length in front of it.
func ProofKey(hash string) []byte {
	return []byte("clowder join\n" + hash)
}
