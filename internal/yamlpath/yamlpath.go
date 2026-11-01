// Package yamlpath names a place in a YAML document the one way clowder
// writes it: hoster.token, alerts.channels[0].url,
// registries["ghcr.io"].password. The names of the secrets in cluster.yaml
// are such paths: internal/secretvalue builds them as it reads a file, and
// clusterspec.SecretPlaces as it lists the places, so the two always agree.
package yamlpath

import (
	"regexp"
	"strconv"
)

// identifier is a key that needs no quotes: a field name. Every other key
// (a domain, an address with "[" and "]") is quoted, so a path reads back
// unambiguously.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Key is the path of key in the mapping at parent ("" is the document).
func Key(parent, key string) string {
	if !identifier.MatchString(key) {
		return parent + "[" + strconv.Quote(key) + "]"
	}
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// Index is the path of item index of the sequence at parent.
func Index(parent string, index int) string {
	return parent + "[" + strconv.Itoa(index) + "]"
}
