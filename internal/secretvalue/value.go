// Package secretvalue seals and opens the values of a SealedSecret, each
// bound to the ID of its Secret (docs/developing/specs.md).
//
// No error message contains a value. A file may be read with a wrong key
// or be broken, and the message goes to a terminal or a log.
package secretvalue

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
)

// version is the envelope's format version. A newer clowder may write a
// higher version.
const version = 1

// Value is a value in clear, and the ID of the Secret it is for.
type Value struct {
	For   string
	Bytes []byte
}

// envelope is what age encrypts. encoding/json writes the []byte value as
// Base64, so no escaping changes its size and binary values need no other
// format.
type envelope struct {
	Version int    `json:"v"`
	For     string `json:"for"`
	Value   []byte `json:"value"`
}

// Seal encrypts value for the sealing key's recipient and returns one line
// of Base64.
func Seal(value Value, recipient age.Recipient) (string, error) {
	if value.For == "" {
		return "", errors.New("seal a secret value: it is for no Secret")
	}
	plaintext, err := json.Marshal(envelope{Version: version, For: value.For, Value: value.Bytes})
	if err != nil {
		return "", fmt.Errorf("seal a secret value: %w", err)
	}
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, recipient)
	if err != nil {
		return "", fmt.Errorf("seal a secret value: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return "", fmt.Errorf("seal a secret value: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("seal a secret value: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext.Bytes()), nil
}

// OpenFor decrypts a value that was found in the Secret with the ID want,
// and refuses a value meant for another Secret.
func OpenFor(sealed, want string, identities ...age.Identity) (Value, error) {
	opened, err := Open(sealed, identities...)
	if err != nil {
		return Value{}, err
	}
	if opened.For != want {
		return Value{}, fmt.Errorf("the value is sealed for %s, not for %s", opened.For, want)
	}
	return opened, nil
}

// Open decrypts a sealed value with one of the identities. The caller must
// check Value.For, or use OpenFor.
func Open(sealed string, identities ...age.Identity) (Value, error) {
	// base64's errors name only an offset, never the text
	ciphertext, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return Value{}, fmt.Errorf("open a secret value: it is not Base64 on one line: %w", err)
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if err != nil {
		return Value{}, fmt.Errorf("open a secret value (sealed for another cluster or key?): %w", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return Value{}, fmt.Errorf("open a secret value: %w", err)
	}
	// drop json's message: it may quote the text it fails on, the value
	var written envelope
	if json.Unmarshal(plaintext, &written) != nil {
		return Value{}, errors.New("open a secret value: it holds no envelope of clowder's")
	}
	switch {
	case written.Version > version:
		return Value{}, fmt.Errorf("open a secret value: written by a newer clowder (format %d)", written.Version)
	case written.Version != version || written.For == "":
		return Value{}, errors.New("open a secret value: it holds no envelope of clowder's")
	}
	return Value{For: written.For, Bytes: written.Value}, nil
}
