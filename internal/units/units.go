// Package units parses the sizes used in Compose's x-clowder and in
// cluster.yaml, so both read "20G" the same way.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize reads a size like "20G" or "512M": a positive whole number and
// one of K, M, G, T (powers of 1024).
func ParseSize(text string) (int64, error) {
	units := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	text = strings.TrimSpace(text)
	if len(text) < 2 {
		return 0, fmt.Errorf("size %q needs a number and a unit (K, M, G, T)", text)
	}
	unit, ok := units[text[len(text)-1]]
	if !ok {
		return 0, fmt.Errorf("size %q needs a unit (K, M, G, T)", text)
	}
	number, err := strconv.ParseInt(text[:len(text)-1], 10, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("size %q is not a positive whole number with a unit", text)
	}
	return number * unit, nil
}
