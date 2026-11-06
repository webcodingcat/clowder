// Package datakey is the cluster's data key: an age identity that every
// host keeps (only root may read it). Secrets are sealed with it before
// they go into the cluster state, so a Raft snapshot or backup that ends up
// in the wrong hands holds no secret in plain text. A joining host gets the
// key in the answer to its join; the Raft backup carries it as well.
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

// New makes a data key; init does this once per cluster.
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

// Text returns the key in age's text form, for the join answer and the backup.
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

// Seal encrypts a secret for the cluster state.
func (k *Key) Seal(plaintext []byte) (state.Sealed, error) {
	var sealed bytes.Buffer
	writer, err := age.Encrypt(&sealed, k.identity.Recipient())
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return sealed.Bytes(), nil
}

// Open decrypts a sealed secret.
func (k *Key) Open(sealed state.Sealed) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, errors.New("open: nothing sealed")
	}
	reader, err := age.Decrypt(bytes.NewReader(sealed), k.identity)
	if err != nil {
		return nil, fmt.Errorf("open (sealed with another data key?): %w", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return plaintext, nil
}

// Derive returns a 32-byte key for one purpose (HKDF-SHA256). The derived
// key is the same on every host and after every restart, and it reveals
// nothing about the data key.
func (k *Key) Derive(purpose string) ([]byte, error) {
	// the key's text form is its secret scalar in a fixed encoding (the key
	// file holds exactly this text), so it is the same wherever it is read
	derived, err := hkdf.Key(sha256.New, []byte(k.identity.String()), nil, purpose, 32)
	if err != nil {
		return nil, fmt.Errorf("derive a key for %s: %w", purpose, err)
	}
	return derived, nil
}
