package buildinfo

import "testing"

func TestNeighbouringReleasesAreCompatible(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.4.0", "1.4.7", true},
		{"1.4.0", "1.5.2", true},
		{"1.3.9", "1.5.0", false},
		{"1.9.0", "2.0.0", false},
		{"0.9.0", "1.0.0", false},
		{"0.2.0", "0.2.3", true},
		{"0.2.0~rc1", "0.2.0", true},
		{"0.2.0", "0.3.0", false},
		{"0.1.4", "0.2.0", false},
		{"0.0.0~dev+abc", "3.1.0", true},
		{"0.0.0~dev+abc", "0.2.0", true},
		{"dev", "1.0.0", true},
		{"", "0.2.0", true}, // a caller without a version
	}
	for _, test := range tests {
		if got := Compatible(test.a, test.b); got != test.want {
			t.Errorf("%s and %s: %v", test.a, test.b, got)
		}
	}
}
