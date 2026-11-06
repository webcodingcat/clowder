// Package yamlpath names a place in a YAML document in the one way that
// clowder writes it: spec.data.password, spec.data["tls.crt"],
// items[0].name. internal/secretvalue builds such paths as it reads a
// file, and resource.SecretValuePath uses this package to name the place
// of a Secret's value, so the two always agree.
package yamlpath

import (
	"regexp"
	"strconv"
)

// identifier is a key that needs no quotes: a field name. Every other key
// (a domain, an address with "[" and "]") is quoted, so a path reads back
// unambiguously.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Key returns the path of key in the mapping at parent ("" is the
// document).
func Key(parent, key string) string {
	if !identifier.MatchString(key) {
		return parent + "[" + strconv.Quote(key) + "]"
	}
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// Index returns the path of item index of the sequence at parent.
func Index(parent string, index int) string {
	return parent + "[" + strconv.Itoa(index) + "]"
}
