// Package names holds the rules for the names clowder takes from users, so
// the CLI, the API, cluster.yaml and the state check them alike.
package names

import "regexp"

// stack is what Compose allows as a project name; it has no "/", which
// separates the stack from the volume in a volume ID.
var stack = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidStack tells whether name may name a stack.
func ValidStack(name string) bool {
	return stack.MatchString(name)
}
