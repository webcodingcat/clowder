// Package units parses the sizes and durations used in Compose's x-clowder
// and in cluster.yaml, so every place reads "20G" and "30d" the same way.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseDuration reads a duration in Go's format ("90s", "1h30m") that may
// start with whole days ("30d", "1d12h"): certificates and heartbeats are
// thought of in days, which Go's format lacks. Negative durations are
// refused.
func ParseDuration(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	problem := fmt.Errorf("duration %q is not like 90s, 1h30m, 30d or 1d12h", text)
	rest := text
	var days time.Duration
	if before, after, hasDays := strings.Cut(text, "d"); hasDays {
		// 16 bits of days (179 years) cannot overflow a Duration
		number, err := strconv.ParseUint(before, 10, 16)
		if err != nil {
			return 0, problem
		}
		days = time.Duration(number) * 24 * time.Hour
		if after == "" {
			return days, nil
		}
		rest = after
	}
	duration, err := time.ParseDuration(rest)
	if err != nil || duration < 0 {
		return 0, problem
	}
	if duration > math.MaxInt64-days {
		return 0, fmt.Errorf("duration %q is too long", text)
	}
	return days + duration, nil
}

// ParseSize reads a size like "20G" or "512M": a positive whole number and
// one of K, M, G, T (powers of 1024).
func ParseSize(text string) (int64, error) {
	multipliers := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	text = strings.TrimSpace(text)
	if len(text) < 2 {
		return 0, fmt.Errorf("size %q needs a number and a unit (K, M, G, T)", text)
	}
	unit, ok := multipliers[text[len(text)-1]]
	if !ok {
		return 0, fmt.Errorf("size %q needs a unit (K, M, G, T)", text)
	}
	number, err := strconv.ParseInt(text[:len(text)-1], 10, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("size %q is not a positive whole number with a unit", text)
	}
	if number > math.MaxInt64/unit {
		return 0, fmt.Errorf("size %q is too large", text)
	}
	return number * unit, nil
}

// FormatSize writes a size in bytes for people, in the largest unit that
// fits: "20G", "1.5G" (one decimal, rounded), "512M"; under 1K in bytes
// ("100B"). Whole sizes read back with ParseSize, the others do not.
func FormatSize(size int64) string {
	for _, unit := range []struct {
		letter string
		bytes  int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if size >= unit.bytes {
			if size%unit.bytes == 0 {
				return strconv.FormatInt(size/unit.bytes, 10) + unit.letter
			}
			return strconv.FormatFloat(float64(size)/float64(unit.bytes), 'f', 1, 64) + unit.letter
		}
	}
	return strconv.FormatInt(size, 10) + "B"
}
