// Package hostrole holds the roles of a host, like the node roles of
// Kubernetes: control-plane, worker and ingress. A host gets its roles when
// it joins (clowderd init and join --roles, within what its join token
// allows), and they stay fixed until it is removed. Its certificate names
// them, so that a host can read another host's roles without the state.
package hostrole

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Role is one role of a host.
type Role string

// The roles. A control-plane host may vote in Raft and lead, and runs the
// commands that need the cluster's keys (clowderd credentials, sealing-key,
// recover). A worker runs stacks and holds volume copies. An ingress host
// may take the Floating IP.
const (
	ControlPlane Role = "control-plane"
	Worker       Role = "worker"
	Ingress      Role = "ingress"
)

// all are the roles in the order in which a Set lists them.
var all = []Role{ControlPlane, Worker, Ingress}

// Set is a host's roles: at least one, each once, in the order
// control-plane, worker, ingress. Parse and New make one. The zero value is
// empty and not valid.
type Set []Role

// The defaults. The first host has every role. Unless the user says
// otherwise, a joining host and its join token get worker and ingress. So a
// token in a worker's user data cannot make a control-plane host.
var (
	DefaultInit = Set{ControlPlane, Worker, Ingress}
	DefaultJoin = Set{Worker, Ingress}
)

// Rule says what Parse accepts, for messages and help texts.
const Rule = "a comma-separated list of control-plane, worker and ingress"

// Parse reads roles as String writes them, in any order.
func Parse(text string) (Set, error) {
	var roles []Role
	for part := range strings.SplitSeq(text, ",") {
		roles = append(roles, Role(strings.TrimSpace(part)))
	}
	return New(roles...)
}

// New makes a Set of roles. It refuses no role, an unknown role and a role
// named twice.
func New(roles ...Role) (Set, error) {
	if len(roles) == 0 {
		return nil, errors.New("no roles: " + Rule)
	}
	var set Set
	for _, role := range roles {
		switch {
		case !slices.Contains(all, role):
			return nil, fmt.Errorf("unknown role %q: %s", role, Rule)
		case slices.Contains(set, role):
			return nil, fmt.Errorf("role %s named twice", role)
		}
		set = append(set, role)
	}
	slices.SortFunc(set, func(a, b Role) int { return slices.Index(all, a) - slices.Index(all, b) })
	return set, nil
}

// Check tells whether the set is one that New makes. The state accepts only
// such sets, so that two equal sets also compare equal.
func (s Set) Check() error {
	made, err := New(s...)
	if err != nil {
		return err
	}
	if !slices.Equal(made, s) {
		return fmt.Errorf("roles %s are not in the order %s", strings.Join(s.Strings(), ","), Set(all))
	}
	return nil
}

// Has tells whether the set holds the role.
func (s Set) Has(role Role) bool {
	return slices.Contains(s, role)
}

// Within tells whether every role of the set is in allowed. A join's roles
// must be within its token's roles.
func (s Set) Within(allowed Set) bool {
	for _, role := range s {
		if !allowed.Has(role) {
			return false
		}
	}
	return true
}

// Strings returns the roles as strings, as a certificate holds them.
func (s Set) Strings() []string {
	texts := make([]string, len(s))
	for index, role := range s {
		texts[index] = string(role)
	}
	return texts
}

// String returns the roles as Parse reads them: "control-plane,worker".
func (s Set) String() string {
	return strings.Join(s.Strings(), ",")
}
