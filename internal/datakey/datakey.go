// Package datakey is the cluster's data key: an age identity that every
// control-plane host keeps (only root may read it). Secrets are encrypted
// with it before they go into the cluster state. So a Raft snapshot that
// ends up in the wrong hands holds no secret in plain text. A joining
// control-plane host gets the key in the answer to its join; other hosts
// never hold it.
package datakey

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/state"
)

// Key is the data key.
type Key struct {
	identity *age.X25519Identity
}

// New makes a data key. init does this once per cluster.
func New() (*Key, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("make the data key: %w", err)
	}
	return &Key{identity: identity}, nil
}

// Parse reads a key in age's text form (AGE-SECRET-KEY-1...).
func Parse(text string) (*Key, error) {
	identity, err := age.ParseX25519Identity(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("read the data key: %w", err)
	}
	return &Key{identity: identity}, nil
}

// Text returns the key in age's text form, for the key file and the join
// answer.
func (k *Key) Text() string {
	return k.identity.String()
}

// Load reads the key from its file.
func Load(path string) (*Key, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the data key: %w", err)
	}
	return Parse(string(content))
}

// Save writes the key to path, readable only by root. A crash leaves the
// old key or the new one, never a partial key.
func (k *Key) Save(path string) error {
	if err := atomicfile.Write(path, []byte(k.Text()+"\n"), 0o600); err != nil {
		return fmt.Errorf("save the data key: %w", err)
	}
	return nil
}

// Encrypt encrypts a secret for the cluster state.
func (k *Key) Encrypt(plaintext []byte) (state.Encrypted, error) {
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, k.identity.Recipient())
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return encrypted.Bytes(), nil
}

// Decrypt decrypts a secret encrypted with the data key.
func (k *Key) Decrypt(encrypted state.Encrypted) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, errors.New("decrypt: nothing encrypted")
	}
	reader, err := age.Decrypt(bytes.NewReader(encrypted), k.identity)
	if err != nil {
		return nil, fmt.Errorf("decrypt (encrypted with another data key?): %w", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// Derive returns a 32-byte key for one purpose (HKDF-SHA256). The derived
// key is the same on every host and after every restart, and it reveals
// nothing about the data key.
func (k *Key) Derive(purpose string) ([]byte, error) {
	// the key's text form is its secret scalar in a fixed encoding, and the
	// key file holds exactly this text. So the input is the same on every
	// host.
	derived, err := hkdf.Key(sha256.New, []byte(k.identity.String()), nil, purpose, 32)
	if err != nil {
		return nil, fmt.Errorf("derive a key for %s: %w", purpose, err)
	}
	return derived, nil
}
