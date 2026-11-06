package daemon

import (
	"testing"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/daemontest"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/supervisor"
)

func TestTheRestartRulesComeFromTheStacksOfThisHost(t *testing.T) {
	cluster := daemontest.NewCluster(t)
	rules := restartRules{cluster, 0}
	shop := daemontest.ShopProject
	tests := []struct {
		name, project, service string
		want                   supervisor.Rule
		known                  bool
	}{
		{"clowder's Traefik", ingress.Project, "traefik", traefikRule, true},
		{"a service of a stack here", shop, "app", supervisor.Rule{Restart: "always", RestartUnhealthy: true}, true},
		{"a service the stack lacks", shop, "cache", supervisor.Rule{}, false},
		{"a stack the state lacks", "default_blog", "app", supervisor.Rule{}, false},
		{"no project of a stack", "not a project", "app", supervisor.Rule{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if rule, known := rules.of(test.project, test.service); rule != test.want || known != test.known {
				t.Errorf("%+v, %v; want %+v, %v", rule, known, test.want, test.known)
			}
		})
	}

	// a pushed-out service never restarts
	cluster.Lock()
	cluster.State.Stacks["default/shop"].PushedOut = []string{"app"} // as set-pushed-out writes it
	cluster.Unlock()
	if rule, known := rules.of(shop, "app"); !known || rule.Restart != "no" {
		t.Errorf("the rule of a pushed-out service %+v, %v", rule, known)
	}

	// a stack that moved to another host is not this host's to restart
	cluster.Lock()
	other := state.HostID(1)
	cluster.State.Stacks["default/shop"].Host = &other
	cluster.Unlock()
	if rule, known := rules.of(shop, "app"); known {
		t.Errorf("the rule of a stack on another host: %+v", rule)
	}
}
