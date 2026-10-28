package sopsage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
)

// SOPS encrypts each value with AES-256-GCM and a 32-byte nonce; the
// additional data is the path of the value (keys joined by ":", plus ":").
const nonceSize = 32

var encryptedValue = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.+),iv:(.+),tag:(.+),type:(.+)\]`)

// encryptValue seals one value as SOPS writes it.
func encryptValue(plaintext []byte, valueType string, key []byte, additionalData string) (string, error) {
	gcm, err := newGCM(key, nonceSize)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("make a nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, []byte(additionalData))
	data, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	return fmt.Sprintf("ENC[AES256_GCM,data:%s,iv:%s,tag:%s,type:%s]",
		base64.StdEncoding.EncodeToString(data), base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(tag), valueType), nil
}

// decryptValue opens one value; it returns the plaintext and its SOPS type
// (str, int, float, bool, time, bytes, comment).
func decryptValue(value string, key []byte, additionalData string) ([]byte, string, error) {
	match := encryptedValue.FindStringSubmatch(value)
	if match == nil {
		return nil, "", errors.New("not an encrypted SOPS value")
	}
	var parts [3][]byte
	for index := range parts {
		decoded, err := base64.StdEncoding.DecodeString(match[index+1])
		if err != nil {
			return nil, "", fmt.Errorf("decode the encrypted value: %w", err)
		}
		parts[index] = decoded
	}
	data, nonce, tag := parts[0], parts[1], parts[2]
	gcm, err := newGCM(key, len(nonce))
	if err != nil {
		return nil, "", err
	}
	plaintext, err := gcm.Open(nil, nonce, append(data, tag...), []byte(additionalData))
	if err != nil {
		return nil, "", errors.New("the value does not open with the data key (wrong key or changed file)")
	}
	return plaintext, match[4], nil
}

// newGCM sets up AES-GCM with the nonce size of the value at hand.
func newGCM(key []byte, nonceLength int) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("data key: %w", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, nonceLength)
	if err != nil {
		return nil, fmt.Errorf("data key: %w", err)
	}
	return gcm, nil
}
