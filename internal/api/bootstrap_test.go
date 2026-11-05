package api

import (
	"bytes"
	"testing"
	"time"
)

var testBootstrapKey = []byte("a bootstrap key of at least 32 letters")

func testBootstrapRequest() BootstrapRequest {
	return BootstrapRequest{
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
