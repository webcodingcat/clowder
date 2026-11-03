package jointoken

import (
	"strings"
	"testing"
)

func TestATokenReadsBackAsItWasWritten(t *testing.T) {
	token, err := New()
	if err != nil {
		t.Fatal(err)
	}
	read, err := Parse(" " + token.String() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != token.ID || read.Hash() != token.Hash() {
		t.Errorf("read %s with hash %s; want %s with %s", read.ID, read.Hash(), token.ID, token.Hash())
	}
	other, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == token.ID || other.Hash() == token.Hash() {
		t.Error("two tokens are the same")
	}
}

func TestTheHashDoesNotShowTheSecret(t *testing.T) {
	token, err := New()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := strings.Cut(token.String(), ".")
	if strings.Contains(token.Hash(), secret) || len(token.Hash()) != 64 {
		t.Errorf("hash %s of secret %s", token.Hash(), secret)
	}
}

func TestOnlyAWholeTokenParses(t *testing.T) {
	token, err := New()
	if err != nil {
		t.Fatal(err)
	}
	text := token.String()
	for _, bad := range []string{
		"", token.ID, "xyz." + text[17:], text[:len(text)-2], token.ID + ".!!", "00" + text,
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
