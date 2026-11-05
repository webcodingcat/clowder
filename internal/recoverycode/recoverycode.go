// Package recoverycode makes and checks the codes that give a user access
// back: root on a host makes them (clowderd user recovery-code), the user
// types one into clowder user recover or clowder recovery rotate. A code is
// an HMAC with a key every agent derives from the data key (Purpose), over
// its use, the user and a quarter of an hour, so every agent checks it
// alone. A code opens once: the state records a used one by its Digest
// until it expired.
package recoverycode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/digest"
)

// Purpose names the key, derived from the data key, that codes are made
// with.
const Purpose = "clowder user recovery"

// Use is what a code is for. Each use has its own code, so a user who
// recovers can rotate the recovery key right after, and a code seen while
// rotating gives no recovery.
type Use string

const (
	// ForRecover is a code for clowder user recover: new credentials.
	ForRecover Use = "recover"
	// ForRotate is a code for clowder recovery rotate: a new recovery key.
	ForRotate Use = "rotate"
)

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

// Digest names a code without giving it away: the state records the codes
// that were used, and the Raft log and the audit must not show a code that
// may still be taken.
func (c Code) Digest() string {
	sum := sha256.Sum256(c[:])
	return hex.EncodeToString(sum[:])
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

// Make is the user's code for a use in the window of now.
func Make(key []byte, use Use, user string, now time.Time) Code {
	return forWindow(key, use, user, windowOf(now))
}

// Valid tells whether a code is the user's for the use in one of the
// windows Accepted, and until when an agent takes it: the end of the window
// after its own.
func Valid(key []byte, use Use, user string, code Code, now time.Time) (expires time.Time, valid bool) {
	for _, window := range Accepted(now) {
		expected := forWindow(key, use, user, window)
		// no early end: every window costs the same
		if hmac.Equal(expected[:], code[:]) {
			expires, valid = startOf(window+2), true
		}
	}
	return expires, valid
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

// Codes are the user's codes for the use of the windows Accepted, in their
// order: the offer shows a proof made with each.
func Codes(key []byte, use Use, user string, now time.Time) []Code {
	var codes []Code
	for _, window := range Accepted(now) {
		codes = append(codes, forWindow(key, use, user, window))
	}
	return codes
}

func windowOf(t time.Time) int64 {
	return t.Unix() / int64(Window/time.Second)
}

func startOf(window int64) time.Time {
	return time.Unix(window*int64(Window/time.Second), 0).UTC()
}

// forWindow is the HMAC over fields in a fixed order, each after its
// length, so no other fields give it the same input, whatever a user's
// name may hold.
func forWindow(key []byte, use Use, user string, window int64) Code {
	mac := hmac.New(sha256.New, key)
	digest.WriteParts(mac, []byte("code"), []byte(use), []byte(user), []byte(strconv.FormatInt(window, 10)))
	var code Code
	copy(code[:], mac.Sum(nil))
	return code
}
