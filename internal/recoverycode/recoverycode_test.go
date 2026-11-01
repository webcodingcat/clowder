package recoverycode

import (
	"strings"
	"testing"
	"time"
)

var testKey = []byte("a key derived from the data key")

// windowStart is the start of a window, so the tests can stand at its edges.
var windowStart = time.Unix(1_800_000_000/900*900, 0)

func TestACodeIsTakenForFifteenToThirtyMinutes(t *testing.T) {
	code := Make(testKey, "alice", windowStart)
	tests := []struct {
		name  string
		now   time.Time
		valid bool
	}{
		{"in its window", windowStart.Add(14 * time.Minute), true},
		{"in the next window", windowStart.Add(29 * time.Minute), true},
		{"expired", windowStart.Add(30 * time.Minute), false},
		{"in the last minute before its window", windowStart.Add(-30 * time.Second), true},
		{"before that minute", windowStart.Add(-90 * time.Second), false},
		{"a window further ahead", windowStart.Add(-15 * time.Minute), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Valid(testKey, "alice", code, test.now); got != test.valid {
				t.Errorf("valid %v, want %v", got, test.valid)
			}
		})
	}
}

func TestACodeIsOneUsersWithOneKey(t *testing.T) {
	code := Make(testKey, "alice", windowStart)
	if Valid(testKey, "bob", code, windowStart) {
		t.Error("alice's code opens for bob")
	}
	if Valid([]byte("another cluster's key"), "alice", code, windowStart) {
		t.Error("the code opens with another key")
	}
	if Make(testKey, "a1", windowStart) == Make(testKey, "a12", windowStart) {
		t.Error("two users share a code")
	}
}

func TestCodesReadAsPeopleTypeThem(t *testing.T) {
	code := Make(testKey, "alice", windowStart)
	text := code.String()
	if len(text) != 19 || strings.Count(text, "-") != 3 {
		t.Fatalf("code %q is not four groups of four", text)
	}
	typed := strings.ReplaceAll(text, "-", "")
	variants := []string{
		text,
		strings.ToLower(text),
		typed,
		" " + typed + "\n",
		strings.ReplaceAll(text, "0", "O"),
		strings.ReplaceAll(text, "1", "I"),
		strings.ReplaceAll(strings.ToLower(text), "1", "l"),
	}
	for _, variant := range variants {
		parsed, err := Parse(variant)
		if err != nil || parsed != code {
			t.Errorf("Parse(%q) = %v, %v; want %v", variant, parsed, err, code)
		}
	}
	for _, wrong := range []string{"", "K7QM-2XRA-P59D", "K7QM-2XRA-P59D-W4TNX", "K7QM-2XRA-P59D-W4TU"} {
		if _, err := Parse(wrong); err == nil {
			t.Errorf("Parse(%q) took it", wrong)
		}
	}
}

func TestCodesFollowTheAcceptedWindows(t *testing.T) {
	now := windowStart.Add(14*time.Minute + 30*time.Second)
	codes := Codes(testKey, "alice", now)
	if len(codes) != 3 || codes[1] != Make(testKey, "alice", now) {
		t.Fatalf("codes %v: want the previous, current and next window", codes)
	}
	for _, code := range codes {
		if !Valid(testKey, "alice", code, now) {
			t.Errorf("code %v of the offer is not taken", code)
		}
	}
}
