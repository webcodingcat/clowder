package units

import "testing"

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
