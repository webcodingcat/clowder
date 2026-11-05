package api

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

var testBootstrapKey = []byte("a bootstrap key of at least 32 letters")

func testBootstrapRequest() BootstrapRequest {
	return BootstrapRequest{
		User:              "alice",
		PublicKey:         bytes.Repeat([]byte{7}, 32),
		RecoveryRecipient: "age1recipient",
		Time:              time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
	}
}

func TestABootstrapMACCoversEveryField(t *testing.T) {
	signed := testBootstrapRequest()
	signed.Sign(testBootstrapKey)
	if !signed.SignedWith(testBootstrapKey) {
		t.Fatal("the request does not check with the key it was signed with")
	}
	if signed.SignedWith([]byte("another bootstrap key of 32 letters")) {
		t.Error("the request checks with another key")
	}
	changes := map[string]func(*BootstrapRequest){
		"user":               func(r *BootstrapRequest) { r.User = "bob" },
		"public key":         func(r *BootstrapRequest) { r.PublicKey = bytes.Repeat([]byte{8}, 32) },
		"recovery recipient": func(r *BootstrapRequest) { r.RecoveryRecipient = "age1other" },
		"time":               func(r *BootstrapRequest) { r.Time = r.Time.Add(time.Second) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := signed
			change(&changed)
			if changed.SignedWith(testBootstrapKey) {
				t.Error("the MAC still checks after the field changed")
			}
		})
	}
}

// With fields joined by line ends, a line end in one field could pass for
// the end of that field: these two requests gave the same MAC.
func TestALineEndInAFieldDoesNotPassForTheNextField(t *testing.T) {
	second := testBootstrapRequest()
	second.RecoveryRecipient = "\nage1recipient"
	first := testBootstrapRequest()
	// first moves the public key into its name and leaves its own key
	// empty; second makes up for the line end after that empty key with one
	// at the start of its recipient
	first.User = "alice\n" + base64.StdEncoding.EncodeToString(second.PublicKey)
	first.PublicKey = nil
	first.RecoveryRecipient = "age1recipient"

	joined := func(r BootstrapRequest) string {
		return "clowder bootstrap\n" + r.User + "\n" + base64.StdEncoding.EncodeToString(r.PublicKey) + "\n" +
			r.RecoveryRecipient + "\n" + r.Time.UTC().Format(time.RFC3339)
	}
	if joined(first) != joined(second) {
		t.Fatal("the two requests do not meet in the joined form; the test shows nothing")
	}
	first.Sign(testBootstrapKey)
	second.Sign(testBootstrapKey)
	if bytes.Equal(first.MAC, second.MAC) {
		t.Error("two requests with other fields share their MAC")
	}
}
