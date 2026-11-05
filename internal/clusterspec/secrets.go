package clusterspec

import (
	"cmp"
	"maps"
	"slices"

	"github.com/webcodingcat/clowder/internal/yamlpath"
)

// SecretPlace is a place of cluster.yaml that holds a secret: its path, and
// the name the config has there.
type SecretPlace struct {
	Path string
	Name SecretRef
}

// SecretPlaces lists the places of config that hold a secret, sorted by
// path; a place left empty is none. It is the one list of them: the CLI
// compares it with the values it took out of cluster.yaml, Check holds each
// name to its path, and the state takes a value for each name.
func SecretPlaces(config Config) []SecretPlace {
	var places []SecretPlace
	add := func(path string, name SecretRef) {
		if name != "" {
			places = append(places, SecretPlace{Path: path, Name: name})
		}
	}
	add(yamlpath.Key(yamlpath.Key("", "hetzner"), "token"), config.Hetzner.Token)
	for _, host := range slices.Sorted(maps.Keys(config.Registries)) {
		registry := yamlpath.Key(yamlpath.Key("", "registries"), host)
		add(yamlpath.Key(registry, "password"), config.Registries[host].Password)
	}
	if config.Alerts != nil {
		alerts := yamlpath.Key("", "alerts")
		for index, channel := range config.Alerts.Channels {
			path := yamlpath.Index(yamlpath.Key(alerts, "channels"), index)
			add(yamlpath.Key(path, "webhook"), channel.Webhook)
			add(yamlpath.Key(path, "url"), channel.URL)
			if channel.SMTP != nil {
				add(yamlpath.Key(yamlpath.Key(path, "smtp"), "password"), channel.SMTP.Password)
			}
		}
		if config.Alerts.Heartbeat != nil {
			add(yamlpath.Key(yamlpath.Key(alerts, "heartbeat"), "url"), config.Alerts.Heartbeat.URL)
		}
	}
	slices.SortFunc(places, func(a, b SecretPlace) int { return cmp.Compare(a.Path, b.Path) })
	return places
}
