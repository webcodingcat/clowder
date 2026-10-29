// Package names holds the rules for the names clowder takes from users, so
// the CLI, the API, cluster.yaml and the state check them alike.
package names

import "regexp"

var (
	// name is what Compose allows as a project name; it has no "/", which
	// separates the stack from the volume in a volume ID, and no "..": a
	// secret's name is a file name on the host.
	name = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// variable is an environment variable's name.
	variable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidStack tells whether name may name a stack.
func ValidStack(stack string) bool {
	return name.MatchString(stack)
}

// ValidSecret tells whether name may name a secret of a stack.
func ValidSecret(secret string) bool {
	return name.MatchString(secret)
}

// ValidVariable tells whether name may name an environment variable.
func ValidVariable(variableName string) bool {
	return variable.MatchString(variableName)
}
