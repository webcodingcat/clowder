package digest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestPartsAreToldApartWhereverTheyAreCut(t *testing.T) {
	if SHA256([]byte("ab"), []byte("c")) == SHA256([]byte("a"), []byte("bc")) {
		t.Error("ab|c and a|bc hash alike")
	}
	whole := sha256.Sum256(append(binary.BigEndian.AppendUint64(nil, 1), 'x'))
	if got := SHA256([]byte("x")); got != hex.EncodeToString(whole[:]) {
		t.Errorf("SHA256 of x is %s, want the SHA-256 of its length and its bytes", got)
	}
	if HMAC([]byte("k1"), []byte("x")) == HMAC([]byte("k2"), []byte("x")) {
		t.Error("two keys give the same HMAC")
	}
}
