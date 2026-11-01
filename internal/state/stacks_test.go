package state

import (
	"maps"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// shopSpec is a stack of one service.
func shopSpec() stackspec.Spec {
	return stackspec.Spec{Services: map[string]stackspec.Service{"app": {Image: "x"}}}
}

// putShop applies the stack default/shop as the CLI does.
func (c *cluster) putShop(spec stackspec.Spec) {
	c.t.Helper()
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", spec))
}

// runningShop puts default/shop, places it on host 0 and has the host
// report it running with the secrets it came up with.
func (c *cluster) runningShop(applied map[string]string) *Stack {
	c.t.Helper()
	c.putShop(shopSpec())
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &host})
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{
		ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1, AppliedSecrets: applied,
	})
	return c.state.Stacks["default/shop"]
}

func TestAStackIsPutOnceAndOnlyByItsChannel(t *testing.T) {
	c := newCluster(t)
	c.putShop(shopSpec())
	stack := c.state.Stacks["default/shop"]
	if stack.Generation != 1 || stack.Status != StackPending || stack.Owner != OwnerCLI {
		t.Errorf("new stack %+v", stack)
	}
	// written another way, it means the same
	same := shopSpec()
	app := same.Services["app"]
	app.Restart = "always"
	same.Services["app"] = app
	if changes, err := c.changes(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", same)); err != nil ||
		len(changes) != 0 {
		t.Errorf("the same stack written out: changes %v, %v", changes, err)
	}
	gitops := putResource(resource.KindStack, "default", "shop", shopSpec())
	gitops.Owner = GitOpsOwner("main")
	c.refuse(KindPutResource, LeaderActor, gitops, "belongs to cli")
}

func TestOnlyItsHostReportsAStackWhereItIs(t *testing.T) {
	c := newCluster(t)
	c.putShop(shopSpec())
	host := HostID(0)
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host},
		"only its host")
	stack := c.runningShop(map[string]string{"token": "d1"})
	if stack.Status != StackRunning || !maps.Equal(stack.AppliedSecrets, map[string]string{"token": "d1"}) {
		t.Errorf("reported %+v", stack)
	}
	c.refuse(KindSetStackStatus, HostActor(1), SetStackStatus{ID: "default/shop", Status: StackPending, Host: &host},
		"only its host")
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackPending}, "only where it is")
	gone := HostID(7)
	c.refuse(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &gone}, "not a member")
	c.refuse(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: "dancing"}, "unknown status")
}

func TestANewGenerationStartsUntilItsHostReportsIt(t *testing.T) {
	c := newCluster(t)
	stack := c.runningShop(nil)
	host := *stack.Host
	changed := shopSpec()
	changed.Services["app"] = stackspec.Service{Image: "y"}
	c.putShop(changed)
	if stack.Status != StackStarting || stack.Generation != 2 || *stack.Host != host {
		t.Errorf("after a new apply of a running stack: %+v", stack)
	}
	// the report of the generation before, sent before the host saw the apply
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1},
		"about generation 1, but 2 is applied")
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 2})
	// the same report again changes nothing
	if changes, err := c.changes(KindSetStackStatus, HostActor(0),
		SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 2}); err != nil || len(changes) != 0 {
		t.Errorf("the same report: changes %v, %v", changes, err)
	}
}

func TestTheStateReadsTheStackSpecItself(t *testing.T) {
	c := newCluster(t)
	broken := shopSpec()
	broken.Services["app"] = stackspec.Service{Image: "x", DependsOn: []string{"db"}}
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", broken),
		`dependsOn: "db" is no other service`)
	unknown := shopSpec()
	unknown.Priority = "urgent"
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", unknown),
		`no priority category "urgent"`)
	reserved := shopSpec()
	reserved.Priority = "high"
	reserved.Services["app"] = stackspec.Service{
		Image: "x", Resources: stackspec.Resources{Reserve: &stackspec.Amount{Memory: "512M", CPUs: 0.5}},
	}
	c.putShop(reserved)
	service := c.state.Stacks["default/shop"].Services["app"]
	if service.MemoryBytes != 512<<20 || service.CPUs != 0.5 || service.Priority.Category != "high" {
		t.Errorf("the scheduler's service %+v", service)
	}
}

func TestOnlyStacksNamedInClusterYAMLMayHavePrivileges(t *testing.T) {
	c := newCluster(t)
	config := testConfig()
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()})
	privileged := shopSpec()
	privileged.Services["app"] = stackspec.Service{Image: "x", Privileged: true}
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", privileged),
		"only stacks named in stacks.privileged")
	config.Stacks.Privileged = []string{"default/shop"}
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()})
	c.putShop(privileged)
	config.Stacks.Privileged = nil
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()},
		"stack default/shop asks for privileges, so it must stay in stacks.privileged")
}

func TestClusterYAMLKeepsTheCategoriesStacksName(t *testing.T) {
	c := newCluster(t)
	spec := shopSpec()
	spec.Priority = "low"
	c.putShop(spec)
	config := testConfig()
	config.Priorities = clusterspec.Priorities{Default: "normal", Categories: map[string]int{"normal": 500}}
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()},
		`cluster.yaml would break stack default/shop: service app: the cluster has no priority category "low"`)
}

func TestAPortOrRouteBelongsToOneStack(t *testing.T) {
	c := newCluster(t)
	published := func(port uint16, protocol, host, path string) stackspec.Spec {
		return stackspec.Spec{Services: map[string]stackspec.Service{"app": {
			Image: "x", Ports: []stackspec.Port{{Port: port, Protocol: protocol}},
			HTTP: &stackspec.HTTP{Host: host, Port: 80, Path: path},
		}}}
	}
	c.putShop(published(9987, "udp", "shop.example.com", ""))
	blog := func(spec stackspec.Spec) PutResource { return putResource(resource.KindStack, "default", "blog", spec) }
	c.refuse(KindPutResource, UserActor("alice"), blog(published(9987, "udp", "blog.example.com", "")),
		"port udp/9987 is published by stack default/shop already")
	c.refuse(KindPutResource, UserActor("alice"), blog(published(9988, "udp", "Shop.example.com", "/")),
		"route shop.example.com/ belongs to stack default/shop already")
	// another protocol or path is another address
	c.mustApply(KindPutResource, UserActor("alice"), blog(published(9987, "tcp", "shop.example.com", "/blog")))
	// a deleted stack frees what it had
	c.mustApply(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindStack, Namespace: "default", Name: "shop", Owner: OwnerCLI})
	c.mustApply(KindPutResource, UserActor("alice"), blog(published(9987, "udp", "shop.example.com", "")))
}

func TestAStackNamesVolumesAndSecretsOfItsNamespace(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	uses := shopSpec()
	uses.Services["app"] = stackspec.Service{
		Image: "x", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}}, Secrets: []string{"token"},
	}
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", uses),
		"namespace default has no volume data")
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", testVolumeSpec()))
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", uses),
		"namespace default has no secret token")
	c.mustApply(KindPutResource, UserActor("alice"),
		putResource(resource.KindSecret, "default", "token", SealedSecret{Value: sealedOf(5), Digest: "d"}))
	c.putShop(uses)

	// one stack per volume; a secret may serve several
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "blog", uses),
		"volume data is stack default/shop's already")
	// what a stack names stays
	c.refuse(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindVolume, Namespace: "default", Name: "data", Owner: OwnerCLI}, "is stack default/shop's")
	c.refuse(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindSecret, Namespace: "default", Name: "token", Owner: OwnerCLI}, "is stack default/shop's")
	// a deleted stack leaves its volume for the next
	c.mustApply(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindStack, Namespace: "default", Name: "shop", Owner: OwnerCLI})
	if _, ok := c.state.Volumes["default/data"]; !ok {
		t.Error("the stack's delete took its volume along")
	}
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "blog", uses))
}

func TestAPlacedStackNamesNoVolumeWhoseDataIsOnAnotherHost(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	c.mustApply(KindAddHost, LeaderActor, AddHost{Host: testHost(1)})
	c.putShop(shopSpec())
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &host})
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", testVolumeSpec()))
	elsewhere := *c.state.Volumes["default/data"]
	minor := 0
	elsewhere.Status, elsewhere.Minor, elsewhere.Copies = VolumeOK, &minor, []VolumeCopy{{Host: 1, Class: "fast"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: elsewhere})

	uses := shopSpec()
	uses.Services["app"] = stackspec.Service{Image: "x", Volumes: []stackspec.Mount{{Volume: "data", Path: "/data"}}}
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "shop", uses),
		"stack default/shop runs on host 0, but volume data has its data on another host")
	// a stack not placed yet goes where the data is
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "blog", uses))
}

func TestTwoNamespacesHaveAStackOfTheSameName(t *testing.T) {
	c := newCluster(t)
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "other", "shop", shopSpec()),
		`no namespace "other"`)
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "", "other", nil))
	c.putShop(shopSpec())
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "other", "shop", shopSpec()))
	if stack := c.state.Stacks["other/shop"]; stack.Project() == c.state.Stacks["default/shop"].Project() {
		t.Error("the two stacks share their project on the host")
	}
}

func TestARestartTakesAlongWhatDependsOnItAndKeepsTheGeneration(t *testing.T) {
	c := newCluster(t)
	spec := stackspec.Spec{Services: map[string]stackspec.Service{
		"db": {Image: "x"}, "api": {Image: "x", DependsOn: []string{"db"}},
		"web": {Image: "x", DependsOn: []string{"api"}}, "cache": {Image: "x"},
	}}
	c.putShop(spec)
	host := HostID(0)
	c.mustApply(KindSetStackStatus, LeaderActor, SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &host})
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1})
	stack := c.state.Stacks["default/shop"]

	// any channel restarts, also one that does not own the stack
	changes, err := c.changes(KindRestartStack, UserActor("bob"), RestartStack{Namespace: "default", Name: "shop", Services: []string{"db"}})
	if err != nil || len(changes) != 1 || changes[0].ResourceVersion != c.index {
		t.Fatalf("restart: changes %v, %v", changes, err)
	}
	if got := slices.Sorted(maps.Keys(stack.Restarted)); !slices.Equal(got, []string{"api", "db", "web"}) {
		t.Errorf("restarted %v, want db and what depends on it", got)
	}
	if stack.Generation != 1 || stack.Status != StackStarting || stack.ResourceVersion != c.index {
		t.Errorf("restarted stack %+v", stack.Meta)
	}
	// a report from before the restart says nothing about the recreated
	// containers; the host reports them up with the restart's times
	c.refuse(KindSetStackStatus, HostActor(0), SetStackStatus{ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1},
		"the report is about an older restart")
	c.mustApply(KindSetStackStatus, HostActor(0), SetStackStatus{
		ID: "default/shop", Status: StackRunning, Host: &host, Generation: 1, Restarted: maps.Clone(stack.Restarted),
	})

	// none named: all of them
	c.mustApply(KindRestartStack, UserActor("alice"), RestartStack{Namespace: "default", Name: "shop"})
	if len(stack.Restarted) != 4 || !stack.Restarted["cache"].Equal(testTime) {
		t.Errorf("restarted %v", stack.Restarted)
	}
	c.refuse(KindRestartStack, UserActor("alice"), RestartStack{Namespace: "default", Name: "shop", Services: []string{"mail"}},
		"stack default/shop has no service mail")
	c.refuse(KindRestartStack, UserActor("alice"), RestartStack{Namespace: "default", Name: "blog"}, "no stack default/blog")

	// a new generation forgets the services it no longer has
	delete(spec.Services, "cache")
	c.putShop(spec)
	if _, ok := stack.Restarted["cache"]; ok || len(stack.Restarted) != 3 {
		t.Errorf("restarted after cache went %v", stack.Restarted)
	}
}

func TestPushedOutServicesAreTheStacksAndANewGenerationDropsGoneOnes(t *testing.T) {
	c := newCluster(t)
	spec := stackspec.Spec{Services: map[string]stackspec.Service{
		"app": {Image: "x", DependsOn: []string{"db"}}, "db": {Image: "x"}, "cache": {Image: "x"},
	}}
	c.putShop(spec)
	c.refuse(KindSetPushedOut, LeaderActor, SetPushedOut{Stacks: map[string][]string{"default/shop": {"web"}}},
		"stack default/shop has no service web")
	c.refuse(KindSetPushedOut, LeaderActor, SetPushedOut{Stacks: map[string][]string{"default/blog": {}}},
		"no stack default/blog")
	c.mustApply(KindSetPushedOut, LeaderActor,
		SetPushedOut{Stacks: map[string][]string{"default/shop": {"cache", "app", "cache"}}})
	if got := c.state.Stacks["default/shop"].PushedOut; !slices.Equal(got, []string{"app", "cache"}) {
		t.Errorf("pushed out %v", got)
	}
	if changes, err := c.changes(KindSetPushedOut, LeaderActor,
		SetPushedOut{Stacks: map[string][]string{"default/shop": {"app", "cache"}}}); err != nil || len(changes) != 0 {
		t.Errorf("the same list again: changes %v, %v", changes, err)
	}
	delete(spec.Services, "cache")
	c.putShop(spec)
	if got := c.state.Stacks["default/shop"].PushedOut; !slices.Equal(got, []string{"app"}) {
		t.Errorf("pushed out after cache went %v", got)
	}
}
