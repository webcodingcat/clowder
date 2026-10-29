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

// Compare orders versions as Debian does: a suffix sorts before the plain
// release (1.2.3~rc1 < 1.2.3). A version that does not parse sorts before
// every other.
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

// Compatible tells whether a CLI and an agent may talk: releases of the same
// major version at most one minor version apart (N and N-1 run side by side
// during an upgrade). Development builds talk to everything, and so does a
// caller without a version it can read (curl, a script): the check is there
// to explain a mismatch, not to keep anyone out.
func Compatible(a, b string) bool {
	first, firstOK := parse(a)
	second, secondOK := parse(b)
	if !firstOK || !secondOK || isDevelopment(first) || isDevelopment(second) {
		return true
	}
	if first.numbers[0] != second.numbers[0] {
		return false
	}
	return max(first.numbers[1], second.numbers[1])-min(first.numbers[1], second.numbers[1]) <= 1
}

func isDevelopment(version parsed) bool {
	return strings.HasPrefix(version.suffix, "~dev")
}
