// Package digest hashes lists of parts so that two different lists never
// give the hash the same input. clowder uses it for every digest of a
// secret and of what an agent applied.
package digest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
)

// WriteParts writes each part to hash, preceded by its length, so that
// "ab", "c" and "a", "bc" differ.
func WriteParts(hash io.Writer, parts ...[]byte) {
	for _, part := range parts {
		_, _ = hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part)))) // a hash never fails to write
		_, _ = hash.Write(part)
	}
}

// HMAC returns the HMAC-SHA256 of the parts under key, in hex. Without the
// key, the digest reveals nothing about a secret, even a short one.
func HMAC(key []byte, parts ...[]byte) string {
	mac := hmac.New(sha256.New, key)
	WriteParts(mac, parts...)
	return hex.EncodeToString(mac.Sum(nil))
}
