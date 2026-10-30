//go:build live

package live

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/units"
)

// storeStack is DEVELOPING.md's example of priorities: db at critical
// (900), app at the stack's high (700) and depending on db, cache at low
// (200); each reserves 200M.
const storeStack = `name: store
x-clowder: {priority: high}
services:
  db:
    image: ` + publicImage + `
    command: [sleep, infinity]
    stop_grace_period: 1s
    deploy: {resources: {reservations: {memory: 200M}}}
    x-clowder: {priority: critical}
  app:
    image: ` + publicImage + `
    command: [sleep, infinity]
    stop_grace_period: 1s
    depends_on: [db]
    deploy: {resources: {reservations: {memory: 200M}}}
  cache:
    image: ` + publicImage + `
    command: [sleep, infinity]
    stop_grace_period: 1s
    deploy: {resources: {reservations: {memory: 200M}}}
    x-clowder: {priority: low}
`

// prioritizedStack is a stack whose services run sleep at a priority and
// reserve memory (name: "priority/memory").
func prioritizedStack(name string, services map[string]string) string {
	content := "name: " + name + "\nservices:\n"
	for _, service := range slices.Sorted(maps.Keys(services)) {
		priority, memory, _ := strings.Cut(services[service], "/")
		content += fmt.Sprintf("  %s:\n    image: %s\n    command: [sleep, infinity]\n    stop_grace_period: 1s\n"+
			"    deploy: {resources: {reservations: {memory: %s}}}\n    x-clowder: {priority: %s}\n",
			service, publicImage, memory, priority)
	}
	return content
}

// pushOutDeadline is how long a pushed-out service may take to come back:
// it must fit for a minute, one service after the other.
const pushOutDeadline = 5 * time.Minute

// pushOut runs store on a host made just big enough for it (system_reserve
// from what /metrics shows), then stacks of higher priority that push its
// services out, lowest first, db never; deleted, they let them come back.
// A stack whose second service finds nothing below its priority starts
// with that service pushed out.
func pushOut(t *testing.T, server host) {
	// echo's services run at the default priority (500), below app's: they
	// would go before app
	mustClowder(t, "stack", "delete", "echo")
	waitGone(t, "echo")
	createAndApply(t, "store", storeStack)
	waitRunning(t, "store", 1)
	// system_reserve takes what is free now but 100M: the host then has
	// 100M free, in MiB rounded down, so a little more. The cases below
	// count on it: 300M is cache's 200M and the free 100M, 500M adds app's
	// 200M, and mixed's api takes the free 100M.
	defaultReserve, err := units.ParseSize(clusterdir.DefaultSystemReserve.Memory)
	if err != nil {
		t.Fatal(err)
	}
	const mib = 1 << 20
	free := int64(metricValue(t, server, "clowder_host_offered_memory_bytes") -
		metricValue(t, server, "clowder_host_reserved_memory_bytes"))
	reserve := (defaultReserve + free - 100*mib) / mib
	t.Logf("free %d MiB; system_reserve %dM", free/mib, reserve)
	changeConfig(t, func(config *clusterdir.Config) { config.SystemReserve.Memory = fmt.Sprintf("%dM", reserve) })

	for _, test := range []struct {
		memory    string
		pushedOut []string
	}{
		{"300M", []string{"cache"}},        // cache (200) and the free 100M
		{"500M", []string{"app", "cache"}}, // app (700) too; db (900) stays
	} {
		createAndApply(t, "urgent", prioritizedStack("urgent", map[string]string{"web": "800/" + test.memory}))
		waitRunning(t, "urgent", 1)
		eventually(t, stackDeadline, fmt.Sprintf("store pushes out %v", test.pushedOut), func() error {
			store := stackInfo(t, "store")
			if !slices.Equal(store.PushedOut, test.pushedOut) {
				return fmt.Errorf("pushed out %v", store.PushedOut)
			}
			for _, service := range test.pushedOut {
				if server.runs(t, "store", service) {
					return fmt.Errorf("%s still runs", service)
				}
			}
			return nil
		})
		if !server.runs(t, "store", "db") {
			t.Error("db was pushed out by a lower priority")
		}
		mustClowder(t, "stack", "delete", "urgent")
		waitGone(t, "urgent")
		eventually(t, pushOutDeadline, "the pushed-out services come back", func() error {
			store := stackInfo(t, "store")
			if len(store.PushedOut) > 0 || !server.runs(t, "store", "cache") || !server.runs(t, "store", "app") {
				return fmt.Errorf("pushed out %v", store.PushedOut)
			}
			return nil
		})
	}

	// api (800) fits the free 100M; worker (100) finds nothing below it
	createAndApply(t, "mixed", prioritizedStack("mixed", map[string]string{"api": "800/100M", "worker": "100/300M"}))
	waitRunning(t, "mixed", 1)
	if mixed := stackInfo(t, "mixed"); !slices.Equal(mixed.PushedOut, []string{"worker"}) {
		t.Errorf("mixed starts with %v pushed out, want worker", mixed.PushedOut)
	}
	if !server.runs(t, "mixed", "api") || server.runs(t, "mixed", "worker") {
		t.Error("mixed should run api and not worker")
	}
	mustClowder(t, "stack", "delete", "mixed")
	waitGone(t, "mixed")
	changeConfig(t, func(config *clusterdir.Config) { config.SystemReserve.Memory = "" })
}
