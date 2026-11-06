// Package secretvalue is clowder's encrypted value: a YAML scalar tagged
// !secret that holds age's ASCII armor. What age encrypts is an envelope
// that binds the value to the Secret it is for (its ID), so a value copied
// to another Secret is refused when it is decrypted.
//
// The package also takes the values out of a YAML file before the file is
// decoded (Extract), and puts new armor in their place without touching
// the rest of the file (Rewrite). It knows YAML paths, not the kinds of
// files: the caller checks which places may hold a secret.
//
// No error contains a value: a file may be read with a wrong key or be
// broken, and the message goes to a terminal or a log.
//
// Every value of a Secret is such a value.
package secretvalue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Tag is the YAML tag of an encrypted value.
const Tag = "!secret"

// ArmorHeader is the first line of every value's armor.
const ArmorHeader = armor.Header

// version is the envelope's format. A newer clowder may write a higher
// version.
const version = 1

// Value is a value in clear and the place it is for.
type Value struct {
	For   string
	Bytes []byte
}

// envelope is what age encrypts. The value is Base64 (encoding/json does
// this for []byte), so there is no escaping that changes its size, no
// question of UTF-8, and binary values need no other format.
type envelope struct {
	Version int    `json:"v"`
	For     string `json:"for"`
	Value   []byte `json:"value"`
}

// Encrypt encrypts value for the recipients and returns age's armor, in
// the form that goes into a file after the tag.
func Encrypt(value Value, recipients ...age.Recipient) (string, error) {
	if value.For == "" {
		return "", errors.New("encrypt a secret value: it is for no place")
	}
	plaintext, err := json.Marshal(envelope{Version: version, For: value.For, Value: value.Bytes})
	if err != nil {
		return "", fmt.Errorf("encrypt a secret value: %w", err)
	}
	var armored bytes.Buffer
	armorWriter := armor.NewWriter(&armored)
	writer, err := age.Encrypt(armorWriter, recipients...)
	if err != nil {
		return "", fmt.Errorf("encrypt a secret value: %w", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return "", fmt.Errorf("encrypt a secret value: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("encrypt a secret value: %w", err)
	}
	if err := armorWriter.Close(); err != nil {
		return "", fmt.Errorf("encrypt a secret value: %w", err)
	}
	return strings.TrimRight(armored.String(), "\n") + "\n", nil
}

// OpenFor decrypts the value found in a Secret whose ID is want. It
// refuses a value meant for another Secret, such as the Hetzner token of
// clowder-system copied into a stack's Secret.
func OpenFor(armored, want string, identities ...age.Identity) (Value, error) {
	opened, err := Open(armored, identities...)
	if err != nil {
		return Value{}, err
	}
	if opened.For != want {
		return Value{}, fmt.Errorf("the value is for %s, not for %s", opened.For, want)
	}
	return opened, nil
}

// Open decrypts armor with one of the identities. The caller checks
// whether the value is meant for the place where it was found (Value.For),
// or uses OpenFor, which does that check.
func Open(armored string, identities ...age.Identity) (Value, error) {
	reader, err := age.Decrypt(armor.NewReader(strings.NewReader(armored)), identities...)
	if err != nil {
		return Value{}, fmt.Errorf("open a secret value (encrypted for another cluster or key?): %w", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		return Value{}, fmt.Errorf("open a secret value: %w", err)
	}
	// json's messages may quote the text they choke on, which is the value
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
