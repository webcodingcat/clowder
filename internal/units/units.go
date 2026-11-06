// Package units parses the sizes and durations in the resources' specs, so
// that every place reads "20G" and "30d" the same way.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseDuration reads a duration in Go's format ("90s", "1h30m"). The
// duration may start with whole days ("30d", "1d12h"): people think of
// certificates and heartbeats in days, and Go's format has no days. It
// refuses negative durations.
func ParseDuration(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	problem := fmt.Errorf("duration %q is not like 90s, 1h30m, 30d or 1d12h", text)
	rest := text
	var days time.Duration
	if before, after, hasDays := strings.Cut(text, "d"); hasDays {
		// a 16-bit count of days (179 years) cannot overflow a Duration
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

// ParseSize reads a size like "20G" or "512M": a positive whole number,
// followed by K, M, G or T (powers of 1024).
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
// fits: "20G", "1.5G" (rounded to one decimal), "512M", or "100B" below
// 1K. ParseSize reads back "20G" and "512M", but not "1.5G" or "100B".
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

// Size is a size in bytes as a spec holds it, read with ParseSize and
// written exactly in the largest unit that holds it whole ("1G", "1536M"),
// so that every written size reads back. A size of 0 means no size and is
// written as ""; the spec's check refuses it.
type Size int64

// MarshalText refuses a negative size or one that is not whole kibibytes,
// because ParseSize never returns such a size.
func (s Size) MarshalText() ([]byte, error) {
	switch {
	case s == 0:
		return []byte{}, nil
	case s < 0 || s%(1<<10) != 0:
		return nil, fmt.Errorf("size %d bytes is not a whole number of kibibytes", int64(s))
	}
	for _, unit := range []struct {
		letter string
		bytes  Size
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}} {
		if s%unit.bytes == 0 {
			return []byte(strconv.FormatInt(int64(s/unit.bytes), 10) + unit.letter), nil
		}
	}
	return []byte(strconv.FormatInt(int64(s>>10), 10) + "K"), nil
}

// UnmarshalText reads the size as ParseSize does.
func (s *Size) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*s = 0
		return nil
	}
	size, err := ParseSize(string(text))
	if err != nil {
		return err
	}
	*s = Size(size)
	return nil
}
