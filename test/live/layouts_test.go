package live

import "strings"

// The layouts of the live tests' labs: which hosts a cluster has and their
// roles. bin/dev live up --layout <name> writes the name into the work
// directory, TestSetup builds the cluster after it, and each scenario's
// baseline checks that its lab has the layout it needs. The file scenarios
// names each scenario's layout. This file has no build tag, so that
// TestEveryScenarioIsListedOnce can check those names.
const (
	// threeHosts is three hosts with every role, the joined two drained,
	// so that stacks start on the first host. Most scenarios run there.
	threeHosts = "three"
	// splitRoles is three control-plane hosts and two workers that hold
	// the ingress. Stacks and copies with a disk go on the workers only.
	splitRoles = "roles"
	// singleHost is one host with every role, a cluster of its own.
	singleHost = "single"
)

// allRoles are the roles the module gives a host it has no roles for.
const allRoles = "control-plane,worker,ingress"

// workerRoles are the roles of the workers of splitRoles.
const workerRoles = "worker,ingress"

// layout is a lab's cluster: the first host and those that join it.
type layout struct {
	name       string
	firstRoles string
	// joining are the hosts that join in TestSetup, in this order.
	joining []layoutHost
	// voters is how many hosts vote once all have joined.
	voters int
	// drainJoined drains the joined hosts at the baseline, so that the
	// scenarios' stacks start on the first host.
	drainJoined bool
}

// layoutHost is a host that joins, with its roles.
type layoutHost struct {
	name  string
	roles string
}

var layouts = map[string]layout{
	threeHosts: {
		name: threeHosts, firstRoles: allRoles,
		joining:     []layoutHost{{firstJoining, allRoles}, {secondJoining, allRoles}},
		voters:      3,
		drainJoined: true,
	},
	splitRoles: {
		name: splitRoles, firstRoles: "control-plane",
		joining: []layoutHost{
			{firstJoining, "control-plane"},
			{secondJoining, "control-plane"},
			{thirdJoining, workerRoles},
			{replacement, workerRoles},
		},
		voters: 3,
	},
	singleHost: {name: singleHost, firstRoles: allRoles, voters: 1},
}

// The hosts of the layouts besides the first one (clowder0, the module's
// default name). In the layout three, TestReplace makes the third and its
// replacement.
const (
	firstHost     = "clowder0"
	firstJoining  = "clowder1"
	secondJoining = "clowder2"
	thirdJoining  = "clowder3"
	replacement   = "clowder4"
)

// rolesOf is a host's roles in the layout; a host it does not name has all
// three, as in the module.
func (l layout) rolesOf(name string) string {
	if name == firstHost {
		return l.firstRoles
	}
	for _, joining := range l.joining {
		if joining.name == name {
			return joining.roles
		}
	}
	return allRoles
}

// hasRole tells whether a host has a role in the layout.
func (l layout) hasRole(name, role string) bool {
	for _, each := range strings.Split(l.rolesOf(name), ",") {
		if each == role {
			return true
		}
	}
	return false
}
