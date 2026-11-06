package buildinfo

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// version is "major.minor.patch" with an optional suffix: 1.2.3~rc1 (a
// release candidate) or 0.0.0~dev+abc (a development build).
var version = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(.*)$`)

// parsed is a version split into its numbers and suffix.
type parsed struct {
	numbers [3]int
	suffix  string
}

func parse(text string) (parsed, bool) {
	match := version.FindStringSubmatch(text)
	if match == nil {
		return parsed{}, false
	}
	var result parsed
	for index := range result.numbers {
		result.numbers[index], _ = strconv.Atoi(match[index+1])
	}
	result.suffix = match[4]
	return result, true
}

// Compare orders versions as Debian does: a version with a suffix sorts
// before the plain release (1.2.3~rc1 < 1.2.3). A version that does not
// parse sorts before every other version.
func Compare(a, b string) int {
	first, firstOK := parse(a)
	second, secondOK := parse(b)
	switch {
	case !firstOK || !secondOK:
		return cmp.Compare(rank(firstOK), rank(secondOK))
	case first.numbers != second.numbers:
		return slices.Compare(first.numbers[:], second.numbers[:])
	case first.suffix == "" || second.suffix == "":
		// the one with a suffix is the earlier build
		return cmp.Compare(len(second.suffix), len(first.suffix))
	default:
		return compareSuffixes(first.suffix, second.suffix)
	}
}

// suffixRun is a run of digits or of other characters in a suffix.
var suffixRun = regexp.MustCompile(`\d+|\D+`)

// compareSuffixes compares runs of digits by their number and the rest as
// text, as Debian does: ~rc2 < ~rc10.
func compareSuffixes(a, b string) int {
	first, second := suffixRun.FindAllString(a, -1), suffixRun.FindAllString(b, -1)
	for index := range min(len(first), len(second)) {
		left, right := first[index], second[index]
		leftNumber, leftErr := strconv.Atoi(left)
		rightNumber, rightErr := strconv.Atoi(right)
		order := cmp.Compare(left, right)
		if leftErr == nil && rightErr == nil {
			order = cmp.Compare(leftNumber, rightNumber)
		}
		if order != 0 {
			return order
		}
	}
	return cmp.Compare(len(first), len(second))
}

// rank orders false before true.
func rank(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Compatible tells whether two builds may talk to each other (a CLI and an
// agent, or two agents). Before 1.0.0, only builds of the same minor
// version may talk: the state and the commands may change with any minor
// release, while a patch release changes neither. From 1.0.0 on, builds of
// the same major version may talk if they are at most one minor version
// apart, because N and N-1 run side by side during an upgrade. Development
// builds talk to everything (callers warn about that), and so does a
// caller without a readable version (curl, a script). The check is there
// to explain a mismatch, not to keep anyone out.
func Compatible(a, b string) bool {
	if IsDevelopment(a) || IsDevelopment(b) {
		return true
	}
	first, _ := parse(a)
	second, _ := parse(b)
	major, minorA, minorB := first.numbers[0], first.numbers[1], second.numbers[1]
	switch {
	case major != second.numbers[0]:
		return false
	case major == 0:
		return minorA == minorB
	default:
		return max(minorA, minorB)-min(minorA, minorB) <= 1
	}
}

// IsDevelopment tells whether a version belongs to a development build, or
// does not parse as a version at all (a build without one, "dev").
func IsDevelopment(text string) bool {
	version, ok := parse(text)
	return !ok || strings.HasPrefix(version.suffix, "~dev")
}

// Includes tells whether a build has what release brought: the build's
// numbers are the release's numbers or later ones, whatever its suffix
// (1.2.0~rc1 includes 1.2.0). A development build is built from the
// current code and includes every release.
func Includes(build, release string) bool {
	if IsDevelopment(build) {
		return true
	}
	first, _ := parse(build)
	second, _ := parse(release)
	return slices.Compare(first.numbers[:], second.numbers[:]) >= 0
}
