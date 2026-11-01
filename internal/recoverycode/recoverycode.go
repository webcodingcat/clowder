// Package recoverycode makes and checks the codes that give a user access
// back: root on a host makes one (clowderd user recovery-code), the user
// types it into clowder user recover or clowder recovery rotate. A code is
// an HMAC with a key every agent derives from the data key (Purpose), over
// the user and a quarter of an hour, so every agent checks it alone and
// nothing is stored.
package recoverycode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Purpose names the key, derived from the data key, that codes are made
// with.
const Purpose = "clowder user recovery"

// Window is how long one code is made for. A code made in one window is
// taken in that window and the next: 15 to 30 minutes.
const Window = 15 * time.Minute

// skew is how far ahead an agent looks: in the last minute of a window it
// takes the next window's code too, for a host whose clock runs a little
// ahead. Never further, or a code made now would open tomorrow's door.
const skew = time.Minute

// size is a code's length in bytes: 80 bits, enough that nobody tries every
// code against the proofs the unchecked offer shows, short enough to type
// from a console.
const size = 10

// crockford is Crockford's base32 alphabet: no I, L, O or U, so what looks
// alike reads alike.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Code is a recovery code.
type Code [size]byte

// String writes the code in four groups of four, as K7QM-2XRA-P59D-W4TN.
func (c Code) String() string {
	text := crockford.EncodeToString(c[:])
	return text[0:4] + "-" + text[4:8] + "-" + text[8:12] + "-" + text[12:16]
}

// Parse reads a code as people type it: case does not count, O reads as 0,
// I and L as 1, and hyphens are skipped.
func Parse(text string) (Code, error) {
	normal := strings.NewReplacer("-", "", "O", "0", "I", "1", "L", "1").
		Replace(strings.ToUpper(strings.TrimSpace(text)))
	var code Code
	decoded, err := crockford.DecodeString(normal)
	if err != nil || len(normal) != crockford.EncodedLen(size) {
		return code, fmt.Errorf("%q is no recovery code: 16 letters and digits, such as K7QM-2XRA-P59D-W4TN", text)
	}
	copy(code[:], decoded)
	return code, nil
}

// Make is the code for a user in the window of now.
func Make(key []byte, user string, now time.Time) Code {
	return forWindow(key, user, windowOf(now))
}

// Valid tells whether a code is the user's in one of the windows Accepted.
func Valid(key []byte, user string, code Code, now time.Time) bool {
	valid := false
	for _, window := range Accepted(now) {
		expected := forWindow(key, user, window)
		// no early end: every window costs the same
		valid = hmac.Equal(expected[:], code[:]) || valid
	}
	return valid
}

// Accepted are the windows whose codes an agent takes now: every window
// between now − Window and now + skew, so the current one, the one before,
// and in the last minute the next.
func Accepted(now time.Time) []int64 {
	var windows []int64
	for window := windowOf(now.Add(-Window)); window <= windowOf(now.Add(skew)); window++ {
		windows = append(windows, window)
	}
	return windows
}

// Codes are the user's codes of the windows Accepted, in their order: the
// offer shows a proof made with each.
func Codes(key []byte, user string, now time.Time) []Code {
	var codes []Code
	for _, window := range Accepted(now) {
		codes = append(codes, forWindow(key, user, window))
	}
	return codes
}

func windowOf(t time.Time) int64 {
	return t.Unix() / int64(Window/time.Second)
}

// forWindow is the HMAC over fields in a fixed order, one per line; no
// field holds a line end (user names may not, the window is a number), so
// "a1"+"23" and "a12"+"3" stay apart.
func forWindow(key []byte, user string, window int64) Code {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("code\n" + user + "\n" + strconv.FormatInt(window, 10)))
	var code Code
	copy(code[:], mac.Sum(nil))
	return code
}
