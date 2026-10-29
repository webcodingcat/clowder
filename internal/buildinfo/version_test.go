package buildinfo

import "testing"

func TestVersionsCompareLikeDebian(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1},
		{"1.2.3", "1.2.3", 0},
		{"0.0.0~dev+abc", "0.1.0", -1},
		{"dev", "0.0.1", -1},
		{"1.2.3~rc1", "1.2.3", -1},
		{"1.2.3~rc2", "1.2.3~rc1", 1},
		{"1.2.3~rc10", "1.2.3~rc2", 1},
	}
	for _, test := range tests {
		if got := Compare(test.a, test.b); got != test.want {
			t.Errorf("%s vs %s: %d", test.a, test.b, got)
		}
	}
}

func TestNeighbouringReleasesAreCompatible(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.4.0", "1.4.7", true},
		{"1.4.0", "1.5.2", true},
		{"1.3.9", "1.5.0", false},
		{"1.9.0", "2.0.0", false},
		{"0.0.0~dev+abc", "3.1.0", true},
		{"dev", "1.0.0", true},
	}
	for _, test := range tests {
		if got := Compatible(test.a, test.b); got != test.want {
			t.Errorf("%s and %s: %v", test.a, test.b, got)
		}
	}
}
