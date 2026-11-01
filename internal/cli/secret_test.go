package cli

import (
	"io"
	"strings"
	"testing"
)

func TestAValueFromAPipeLosesOneLineEnd(t *testing.T) {
	for input, want := range map[string]string{
		"token\n":    "token",
		"token\r\n":  "token",
		"token":      "token",
		"token\n\n":  "token\n",
		"two\nlines": "two\nlines",
	} {
		value, err := readValue(strings.NewReader(input), io.Discard)
		if err != nil || string(value) != want {
			t.Errorf("%q: %q, %v", input, value, err)
		}
	}
	if _, err := readValue(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty value: %v", err)
	}
}

func TestTheRecoveryKeyIsConfirmedByItsLastCharacters(t *testing.T) {
	key := "AGE-SECRET-KEY-1QQQQQQQQQQABCDEFGH"
	if err := confirmKey(strings.NewReader("wrong\n abcdefgh \n"), io.Discard, key); err != nil {
		t.Errorf("right at the second try, in lower case: %v", err)
	}
	if err := confirmKey(strings.NewReader("a\nb\nc\nABCDEFGH\n"), io.Discard, key); err == nil ||
		!strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("wrong three times: %v", err)
	}
	if err := confirmKey(strings.NewReader(""), io.Discard, key); err == nil {
		t.Error("no input confirmed the key")
	}
}
