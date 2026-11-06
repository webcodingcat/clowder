package secretvalue

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"filippo.io/age"
)

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

// sealedAsIs encrypts plaintext as it stands, without an envelope.
func sealedAsIs(t *testing.T, plaintext string, recipient age.Recipient) string {
	t.Helper()
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext.Bytes())
}

func TestTheSealingKeyOpensASealedValue(t *testing.T) {
	sealing := newIdentity(t)
	value := Value{For: "shop/db-password", Bytes: []byte("s3cret\x00\xff<&>")}
	sealed, err := Seal(value, sealing.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(sealed, "\n \t") {
		t.Errorf("not one line of Base64: %q", sealed)
	}
	opened, err := Open(sealed, sealing)
	if err != nil {
		t.Fatal(err)
	}
	if opened.For != value.For || !bytes.Equal(opened.Bytes, value.Bytes) {
		t.Errorf("opened %q for %s", opened.Bytes, opened.For)
	}
	if _, err := Open(sealed, newIdentity(t)); err == nil {
		t.Error("another key opened the value")
	}
}

func TestOpenForRefusesAValueOfAnotherSecret(t *testing.T) {
	sealing := newIdentity(t)
	sealed, err := Seal(Value{For: "clowder-system/hetzner", Bytes: []byte("token")}, sealing.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFor(sealed, "shop/db", sealing); err == nil || !strings.Contains(err.Error(), "not for shop/db") {
		t.Errorf("got %v", err)
	}
	if _, err := OpenFor(sealed, "clowder-system/hetzner", sealing); err != nil {
		t.Error(err)
	}
}

func TestOpenRefusesWhatClowderDidNotWriteWithoutTheValue(t *testing.T) {
	sealing := newIdentity(t)
	for _, test := range []struct{ plaintext, want string }{
		{"hunter2-in-clear", "no envelope"},
		{`{"v":1,"for":"","value":"aHVudGVy"}`, "no envelope"},
		{`{"value":"aHVudGVy","for":"cluster"}`, "no envelope"},
		{`{"v":2,"for":"cluster","value":"aHVudGVy","new":true}`, "newer clowder"},
		{`{"v":1,"for":"cluster","value":"not base64 hunter2"}`, "no envelope"},
	} {
		_, err := Open(sealedAsIs(t, test.plaintext, sealing.Recipient()), sealing)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want %q", test.plaintext, err, test.want)
			continue
		}
		if strings.Contains(err.Error(), "hunter") || strings.Contains(err.Error(), "aHVudGVy") {
			t.Errorf("the error names the value: %v", err)
		}
	}
	if _, err := Open("hunter2 is no Base64", sealing); err == nil || strings.Contains(err.Error(), "hunter") {
		t.Errorf("got %v", err)
	}
}

func TestSealNeedsASecret(t *testing.T) {
	if _, err := Seal(Value{Bytes: []byte("x")}, newIdentity(t).Recipient()); err == nil {
		t.Error("sealed a value for no Secret")
	}
}

// The request size counts a value as 1.8 times its size in a request (two
// rounds of Base64) and a fixed part of less than 1 KiB, with the
// resource's JSON.
func TestASealedValueGrowsAsTheRequestSizeCounts(t *testing.T) {
	sealing := newIdentity(t)
	longest := strings.Repeat("n", 63) + "/" + strings.Repeat("s", 63)
	for _, size := range []int{0, 32, 64 << 10} {
		sealed, err := Seal(Value{For: longest, Bytes: bytes.Repeat([]byte{0xff}, size)}, sealing.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		if budget := size*180/100 + 800; len(sealed) > budget {
			t.Errorf("%d bytes take %d in a request, more than %d", size, len(sealed), budget)
		}
	}
}
