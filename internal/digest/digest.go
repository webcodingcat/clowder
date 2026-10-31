// Package digest hashes lists of parts, so no two lists give a hash the same
// input: the one way clowder makes the digests of secrets and of what an
// agent applied.
package digest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
)

// WriteParts writes each part to a hash after its length, so no two lists
// of parts give the hash the same input ("ab", "c" and "a", "bc" differ).
func WriteParts(hash io.Writer, parts ...[]byte) {
	for _, part := range parts {
		_, _ = hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part)))) // a hash never fails to write
		_, _ = hash.Write(part)
	}
}

// HMAC is an HMAC-SHA256 over the parts with key, in hex: a digest that
// says nothing about a short secret to whoever lacks the key.
func HMAC(key []byte, parts ...[]byte) string {
	mac := hmac.New(sha256.New, key)
	WriteParts(mac, parts...)
	return hex.EncodeToString(mac.Sum(nil))
}
