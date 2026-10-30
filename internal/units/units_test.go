package units

import (
	"testing"
	"time"
)

func TestDurationsTakeDaysBesidesGosUnits(t *testing.T) {
	day := 24 * time.Hour
	cases := map[string]time.Duration{
		"90s": 90 * time.Second, "1h30m": 90 * time.Minute, "30d": 30 * day, "1d12h": day + 12*time.Hour, "0d": 0,
	}
	for text, want := range cases {
		if got, err := ParseDuration(text); err != nil || got != want {
			t.Errorf("%s: %v, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "30", "d", "-1d", "1.5d", "1d-2h", "-5m", "1dd", "1d2d", "99999d", "60000d2562047h"} {
		if _, err := ParseDuration(text); err == nil {
			t.Errorf("%q: no error", text)
		}
	}
}

func TestSizesUseBinaryUnits(t *testing.T) {
	cases := map[string]int64{"512M": 512 << 20, "20G": 20 << 30, "1T": 1 << 40, "4K": 4 << 10}
	for text, want := range cases {
		if got, err := ParseSize(text); err != nil || got != want {
			t.Errorf("%s: %d, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "20", "G", "-1G", "1.5G", "20GB", "9999999999T"} {
		if _, err := ParseSize(text); err == nil {
			t.Errorf("%q: no error", text)
		}
	}
}

func TestSizesAreWrittenInTheLargestUnit(t *testing.T) {
	cases := map[int64]string{20 << 30: "20G", 1 << 40: "1T", 3 << 29: "1.5G", 512 << 20: "512M", 100: "100B"}
	for size, want := range cases {
		if got := FormatSize(size); got != want {
			t.Errorf("%d: %q, want %q", size, got, want)
		}
	}
}
