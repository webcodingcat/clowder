package digest

import (
	"crypto/sha256"
	"testing"
)

func TestPartsAreToldApartWhereverTheyAreCut(t *testing.T) {
	sum := func(parts ...[]byte) [32]byte {
		hash := sha256.New()
		WriteParts(hash, parts...)
		return [32]byte(hash.Sum(nil))
	}
	if sum([]byte("ab"), []byte("c")) == sum([]byte("a"), []byte("bc")) {
		t.Error("ab|c and a|bc hash alike")
	}
	if HMAC([]byte("k1"), []byte("x")) == HMAC([]byte("k2"), []byte("x")) {
		t.Error("two keys give the same HMAC")
	}
}
