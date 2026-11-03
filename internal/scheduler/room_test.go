package scheduler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/state"
)

const mib = int64(1) << 20

// roomCluster is cluster's host offering its services memory (MiB) and 4
// CPUs after the default system_reserve (512M, 0.25).
func roomCluster(memory int64) *state.State {
	current := cluster()
	current.Hosts[0].Capacity = state.Resources{MemoryBytes: (512 + memory) * mib, CPUs: 4.25}
	return current
}

// service is a service at a priority that reserves memory (MiB) and 0.1 CPUs.
func service(priority int, memory int64, dependsOn ...string) state.Service {
	return state.Service{
		Priority: clusterspec.Priority{Value: &priority}, MemoryBytes: memory * mib, CPUs: 0.1, DependsOn: dependsOn,
	}
}

// running puts a stack of the namespace default on the host, running, with
// these services pushed out.
func running(current *state.State, name string, services map[string]state.Service, pushedOut ...string) {
	host := state.HostID(0)
	current.Stacks["default/"+name] = &state.Stack{
		Meta:   state.Meta{Namespace: "default", Name: name},
		Status: state.StackRunning, Host: &host, Services: services, PushedOut: pushedOut,
	}
}

// pending adds the stack default/new, which waits for a host.
func pending(current *state.State, services map[string]state.Service) {
	current.Stacks["default/new"] = &state.Stack{
		Meta:   state.Meta{Namespace: "default", Name: "new"},
		Status: state.StackPending, Services: services,
	}
}

// shop is a stack of three: db at 900, app at 700 depending on it, cache at
// 200, 300M each.
func shop() map[string]state.Service {
	return map[string]state.Service{"db": service(900, 300), "app": service(700, 300, "db"), "cache": service(200, 300)}
}

func TestANewStackPushesOutLowerPrioritiesLowestFirst(t *testing.T) {
	for _, test := range []struct {
		need   int64
		pushed string // shop's new list, or "waits"
	}{
		{100, "[]"},          // the free 100M are enough
		{300, "[cache]"},     // cache (200) first
		{500, "[app cache]"}, // then app (700 < 800)
		{1000, "waits"},      // db (900) stays, and all below 800 are not enough
	} {
		t.Run(fmt.Sprint(test.need), func(t *testing.T) {
			current := roomCluster(1000)
			running(current, "shop", shop())
			pending(current, map[string]state.Service{"web": service(800, test.need)})
			decisions := Plan(current, planTime)
			if test.pushed == "waits" {
				if len(decisions.Placements) != 0 || len(decisions.Waiting) != 1 ||
					!strings.Contains(decisions.Waiting[0].Reason, "service web (1000M and 0.1 CPUs) finds no room") {
					t.Errorf("decisions %+v", decisions)
				}
				return
			}
			if len(decisions.Placements) != 1 || !decisions.Placements[0].SetsHost {
				t.Fatalf("decisions %+v", decisions)
			}
			lists := decisions.Placements[0].PushedOut
			got := fmt.Sprint(lists["default/shop"])
			if test.pushed == "[]" && lists["default/shop"] == nil {
				got = "[]"
			}
			if got != test.pushed || len(lists["default/new"]) != 0 {
				t.Errorf("pushed out %v, want shop %s", lists, test.pushed)
			}
		})
	}
}

func TestEachServiceOfANewStackPushesOutOnlyBelowItsOwnPriority(t *testing.T) {
	// api at 800 pushes out cache and fits, worker at
	// 100 finds nothing below it and starts pushed out
	current := roomCluster(1000)
	running(current, "base", map[string]state.Service{"core": service(900, 700), "cache": service(200, 300)})
	pending(current, map[string]state.Service{"api": service(800, 300), "worker": service(100, 300)})
	decisions := Plan(current, planTime)
	if len(decisions.Placements) != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
	lists := decisions.Placements[0].PushedOut
	if fmt.Sprint(lists) != "map[default/base:[cache] default/new:[worker]]" {
		t.Errorf("pushed out %v", lists)
	}
}

func TestWhatDependsOnAPushedOutServiceGoesWithIt(t *testing.T) {
	current := roomCluster(1000)
	// view at 600 depends on data at 300: its effective priority is 300;
	// data goes first (by name), and view with it
	running(current, "low", map[string]state.Service{"data": service(300, 500), "view": service(600, 500, "data")})
	pending(current, map[string]state.Service{"web": service(500, 200)})
	decisions := Plan(current, planTime)
	if len(decisions.Placements) != 1 || fmt.Sprint(decisions.Placements[0].PushedOut["default/low"]) != "[data view]" {
		t.Errorf("decisions %+v", decisions)
	}
}

func TestAHostOverCapacityPushesOutBelowItsHighestPriority(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity int64
		services map[string]state.Service
		pushed   string
		over     bool
	}{
		{"over by less than cache", 800, shop(), "[cache]", false},
		{"over by more than cache and app", 200, shop(), "", true},
		{"all at the highest priority", 800, map[string]state.Service{
			"a": service(900, 300), "b": service(900, 300), "c": service(900, 300),
		}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := roomCluster(test.capacity)
			running(current, "shop", test.services)
			decisions := Plan(current, planTime)
			var pushed string
			if len(decisions.PushOuts) == 1 {
				pushed = fmt.Sprint(decisions.PushOuts[0]["default/shop"])
			}
			if pushed != test.pushed || (len(decisions.OverCapacity) == 1) != test.over {
				t.Errorf("push-outs %v, over capacity %v", decisions.PushOuts, decisions.OverCapacity)
			}
			if test.over && len(decisions.PushOuts) != 0 {
				t.Error("pushed out although it does not help")
			}
		})
	}
}

func TestAPushedOutServiceComesBackWhenItFitsWithAMargin(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity int64
		want     string
	}{
		{"room with the margin", 1000, "default/shop/cache []"}, // 400M free, cache needs 315M
		{"room without the margin", 910, "none"},                // 310M free
	} {
		t.Run(test.name, func(t *testing.T) {
			current := roomCluster(test.capacity)
			running(current, "shop", shop(), "cache")
			got := "none"
			if back := Plan(current, planTime).ComeBack; back != nil {
				got = fmt.Sprintf("%s/%s %v", back.Stack, back.Service, back.PushedOut)
			}
			if got != test.want {
				t.Errorf("coming back %s, want %s", got, test.want)
			}
		})
	}
}

func TestServicesComeBackByPriorityAndDependencies(t *testing.T) {
	current := roomCluster(1000)
	// 700M free: big (600, 800M) does not fit, app waits for db, so db
	// (300M at 900) comes first
	running(current, "shop", map[string]state.Service{
		"db": service(900, 300), "app": service(950, 100, "db"), "big": service(600, 800), "small": service(100, 100),
	}, "db", "app", "big", "small")
	back := Plan(current, planTime).ComeBack
	if back == nil || back.Service != "db" || fmt.Sprint(back.PushedOut) != "[app big small]" {
		t.Errorf("coming back %+v", back)
	}
}

func TestAWaitingStackGetsRoomBeforeAPushedOutService(t *testing.T) {
	current := roomCluster(1000)
	running(current, "shop", shop(), "cache")
	pending(current, map[string]state.Service{"web": service(800, 300)})
	decisions := Plan(current, planTime)
	if len(decisions.Placements) != 1 || decisions.ComeBack != nil {
		t.Errorf("decisions %+v", decisions)
	}
	// shop's list is rewritten only when the new stack touches it: cache
	// stays out, as it was
	if lists := decisions.Placements[0].PushedOut; fmt.Sprint(lists) != "map[default/new:[]]" {
		t.Errorf("pushed out %v", lists)
	}
}

func TestRoomsAreWhatTheHostsOfferAndTheirServicesReserve(t *testing.T) {
	current := roomCluster(1000)
	running(current, "shop", shop(), "cache") // db and app run: 600M and 0.2 CPUs
	current.Hosts[1] = &state.Host{ID: 1, Name: "node1", Phase: state.Removed}
	rooms := Rooms(current)
	want := HostRoom{
		OfferedMemory: 1000 * mib, ReservedMemory: 600 * mib, OfferedMilliCPUs: 4000, ReservedMilliCPUs: 200,
	}
	if len(rooms) != 1 || rooms[0] != want {
		t.Errorf("rooms %+v, want only node0 with %+v", rooms, want)
	}
}

func TestCategoriesComeFromTheCurrentClusterYAML(t *testing.T) {
	current := roomCluster(1000)
	current.Config.Spec.Priorities = clusterspec.Priorities{
		Default: "normal", Categories: map[string]int{"normal": 500, "low": 600},
	}
	low := state.Service{Priority: clusterspec.Priority{Category: "low"}, MemoryBytes: 900 * mib}
	running(current, "batch", map[string]state.Service{"job": low})
	pending(current, map[string]state.Service{"web": {MemoryBytes: 300 * mib}}) // the default: 500
	// low is 600 now, above the new stack's 500: nothing to push out
	if waiting := Plan(current, planTime).Waiting; len(waiting) != 1 {
		t.Errorf("waiting %+v", waiting)
	}
}
