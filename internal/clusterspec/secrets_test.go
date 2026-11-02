package clusterspec

import (
	"slices"
	"testing"
)

func TestSecretPlacesNameEveryPlaceThatHoldsASecret(t *testing.T) {
	config := Config{
		Hoster:     Hoster{Token: "hoster.token"},
		JoinKeys:   []SecretRef{"join_keys[0]", "join_keys[1]"},
		Registries: map[string]Registry{"ghcr.io": {Password: "a"}, "localhost": {Password: "b"}, "[::1]:5000": {}},
		Alerts: &Alerts{
			Channels: []AlertChannel{
				{Webhook: "w"}, {URL: "u"}, {SMTP: &SMTP{Password: "p"}}, {SMTP: &SMTP{}},
			},
			Heartbeat: &Heartbeat{URL: "h"},
		},
	}
	var paths []string
	for _, place := range SecretPlaces(config) {
		paths = append(paths, place.Path)
	}
	want := []string{
		"alerts.channels[0].webhook", "alerts.channels[1].url", "alerts.channels[2].smtp.password",
		"alerts.heartbeat.url", "hoster.token", "join_keys[0]", "join_keys[1]",
		"registries.localhost.password", `registries["ghcr.io"].password`,
	}
	if !slices.Equal(paths, want) {
		t.Errorf("got %q", paths)
	}
}
