package docker

import (
	"slices"
	"testing"
)

func TestTheDockerConfigGoesBeforeTheCommand(t *testing.T) {
	for _, test := range []struct {
		config string
		want   []string
	}{
		{"/run/clowder/secrets/clowder-registries/shop", []string{
			"--config", "/run/clowder/secrets/clowder-registries/shop", "pull", "--quiet", "nginx",
		}},
		{"", []string{"pull", "--quiet", "nginx"}},
	} {
		if got := withConfig(test.config, "pull", "--quiet", "nginx"); !slices.Equal(got, test.want) {
			t.Errorf("config %q: %q, want %q", test.config, got, test.want)
		}
	}
}
