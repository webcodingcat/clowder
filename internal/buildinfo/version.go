package buildinfo

import (
	"regexp"
	"strconv"
	"strings"
)

// version is "major.minor.patch" with an optional suffix: 1.2.3~rc1 (a
// release candidate) or 0.0.0~dev+abc (a development build).
var version = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(.*)$`)

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

// Compatible tells whether two builds may talk to each other (a CLI and an
// agent, or two agents):
//   - Before 1.0.0, only builds of the same minor version may talk. The
//     state and the commands may change with any minor release, but a
//     patch release changes neither.
//   - From 1.0.0 on, builds of the same major version may talk if they are
//     at most one minor version apart, because N and N-1 run side by side
//     during an upgrade.
//   - Development builds talk to everything (callers warn about that), and
//     so does a caller without a readable version (curl, a script).
//
// The check is there to explain a mismatch, not to keep anyone out.
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
