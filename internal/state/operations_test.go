package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/stackspec"
)

// lockCluster is a cluster with what locks can hold: default/shop running
// on host 0, the volume default/data placed on host 0 (no stack names it)
// and host 1 active.
func lockCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	c.addHost(testHost(1))
	c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Active})
	c.runningShop(nil)
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", testVolumeSpec()))
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: c.placedData()})
	return c
}

// placedData is default/data with its one copy on host 0.
func (c *cluster) placedData() Volume {
	placed := *c.state.Volumes["default/data"]
	minor := 3
	placed.Status, placed.Minor, placed.Copies = VolumeOK, &minor, []VolumeCopy{{Host: 0, Class: "fast"}}
	return placed
}

// startOperation starts an operation of the leader that holds the locks
// and returns its ID.
func (c *cluster) startOperation(locks ...string) uint64 {
	c.t.Helper()
	c.mustApply(KindStartOperation, LeaderActor, StartOperation{Kind: "test", Locks: locks})
	return c.index
}

// lockCase is a command that changes what a lock guards in lockCluster.
type lockCase struct {
	name, kind, actor, lock string
	// setup runs before any lock is taken
	setup func(c *cluster)
	// payload is the command; operation is the one that sends it, 0 for none
	payload func(c *cluster, operation uint64) any
	// byOperation: the operation that holds the lock may send it
	byOperation bool
	// free: the handler minds no lock, for the reason its code gives
	free bool
}

var lockCases = []lockCase{
	{
		// it ends every operation and frees its locks (endOperations)
		name: "clowderd recover remaps the cluster", kind: KindRecoverRemap, actor: LeaderActor, lock: "host/1",
		payload: func(*cluster, uint64) any { return RecoverRemap{Survivor: 0} },
		free:    true,
	},
	{
		name: "a user asks to move a stack", kind: KindRequestMove, actor: UserActor("alice"), lock: "stack/default/shop",
		payload: func(*cluster, uint64) any { return RequestMove{Namespace: "default", Name: "shop", To: 1} },
	},
	{
		name: "the host tells its copy is synced", kind: KindSetCopySynced, actor: HostActor(0), lock: "volume/default/data",
		payload: func(*cluster, uint64) any {
			return SetCopySynced{Volume: "default/data", DataGeneration: 1, Synced: true}
		},
		free: true,
	},
	{
		name: "a user changes a stack", kind: KindPutResource, actor: UserActor("alice"), lock: "stack/default/shop",
		payload: func(*cluster, uint64) any {
			spec := shopSpec()
			spec.Services["cache"] = stackspec.Service{Image: "x"}
			return putResource(resource.KindStack, "default", "shop", spec)
		},
	},
	{
		name: "a user deletes a stack", kind: KindDeleteResource, actor: UserActor("alice"), lock: "stack/default/shop",
		payload: func(*cluster, uint64) any {
			return DeleteResource{Kind: resource.KindStack, Namespace: "default", Name: "shop", Owner: OwnerCLI}
		},
	},
	{
		name: "a user deletes a volume", kind: KindDeleteResource, actor: UserActor("alice"), lock: "volume/default/data",
		payload: func(*cluster, uint64) any {
			return DeleteResource{Kind: resource.KindVolume, Namespace: "default", Name: "data", Owner: OwnerCLI}
		},
	},
	{
		name: "a user restarts a stack", kind: KindRestartStack, actor: UserActor("alice"), lock: "stack/default/shop",
		payload: func(*cluster, uint64) any { return RestartStack{Namespace: "default", Name: "shop"} },
	},
	{
		name: "the leader takes a stack off its host", kind: KindSetStackStatus, actor: LeaderActor, lock: "stack/default/shop",
		payload: func(c *cluster, operation uint64) any {
			return SetStackStatus{ID: "default/shop", Status: StackPending, PlannedAt: c.index, Operation: operation}
		},
		byOperation: true,
	},
	{
		name: "the host reports its stack", kind: KindSetStackStatus, actor: HostActor(0), lock: "stack/default/shop",
		payload: func(*cluster, uint64) any {
			host := HostID(0)
			return SetStackStatus{ID: "default/shop", Status: StackFailing, Host: &host, Generation: 1, UpError: "no image"}
		},
		free: true,
	},
	{
		name: "the leader says why a stack waits", kind: KindSetStackWaitReason, actor: LeaderActor, lock: "stack/default/shop",
		payload: func(*cluster, uint64) any { return SetStackWaitReason{ID: "default/shop", Reason: "no room"} },
	},
	{
		name: "the leader pushes a service out", kind: KindSetPushedOut, actor: LeaderActor, lock: "stack/default/shop",
		payload: func(c *cluster, _ uint64) any {
			return SetPushedOut{Stacks: map[string][]string{"default/shop": {"app"}}, PlannedAt: c.index}
		},
	},
	{
		name: "the leader restores a volume", kind: KindPutVolume, actor: LeaderActor, lock: "volume/default/data",
		payload: func(c *cluster, operation uint64) any {
			restored := *c.state.Volumes["default/data"]
			restored.DataGeneration++
			return PutVolume{Volume: restored, PlannedAt: c.index, Operation: operation}
		},
		byOperation: true,
	},
	{
		name: "the host tells its copy holds data", kind: KindSetVolumeInitialized, actor: HostActor(0), lock: "volume/default/data",
		payload: func(*cluster, uint64) any { return SetVolumeInitialized{Volume: "default/data", DataGeneration: 1} },
		free:    true,
	},
	{
		name: "the leader drains a host", kind: KindSetHostPhase, actor: LeaderActor, lock: "host/1",
		payload: func(_ *cluster, operation uint64) any {
			return SetHostPhase{Host: 1, Phase: Draining, Operation: operation}
		},
		byOperation: true,
	},
	{
		name: "the leader takes a failed host back", kind: KindSetHostPhase, actor: LeaderActor, lock: "host/1",
		setup: func(c *cluster) {
			c.mustApply(KindSetHostPhase, LeaderActor, SetHostPhase{Host: 1, Phase: Failed})
		},
		payload: func(*cluster, uint64) any { return SetHostPhase{Host: 1, Phase: Active} },
	},
}

// unlockedKinds change no stack, volume or host phase, so they have no
// case; each says why.
var unlockedKinds = map[string]string{
	KindBootstrap:            "makes the cluster",
	KindApplyConfig:          "changes cluster.yaml, no stack or volume",
	KindReportHost:           "a host's report, no phase",
	KindSetIngress:           "names the ingress host, whose phase stays",
	KindPutACMEAccount:       "the ACME account",
	KindPutACMEChallenge:     "an ACME challenge",
	KindDeleteACMEChallenge:  "an ACME challenge",
	KindPutCertificate:       "a route's certificate",
	KindDeleteCertificate:    "a route's certificate",
	KindAddUser:              "a user",
	KindRecoverUser:          "a user",
	KindSetRecoveryRecipient: "the recovery key",
	KindPutAlert:             "an alert",
	KindResolveAlert:         "an alert",
	KindForgetCopy:           "a retired copy, which no volume has any more",
	KindStartOperation:       "operations take the locks",
	KindAdvanceOperation:     "operations take the locks",
	KindFinishOperation:      "operations take the locks",
	KindExtendOperation:      "operations take the locks",
	KindRollBackOperation:    "operations take the locks",
	KindAddJoinToken:         "a join token",
	KindAddHost:              "a new host, which no operation can hold yet",
	KindConfirmServer:        "a fact about the host's server, no phase",
	KindSetHostVoter:         "mirrors Raft's configuration, no phase",
	KindReportPeerForgotten:  "a host's report about a removed host",
}

func TestEveryKindOfCommandIsCheckedForLocks(t *testing.T) {
	for _, kind := range slices.Sorted(maps.Keys(commandKinds)) {
		_, unlocked := unlockedKinds[kind]
		cased := slices.ContainsFunc(lockCases, func(test lockCase) bool { return test.kind == kind })
		if unlocked == cased {
			t.Errorf("%s: give it a lock case if it changes a stack, a volume or a host's phase, else a reason "+
				"in unlockedKinds", kind)
		}
	}
}

func TestOnlyTheOperationThatHoldsALockChangesWhatItGuards(t *testing.T) {
	for _, test := range lockCases {
		t.Run(test.name, func(t *testing.T) {
			// the command changes something, so the lock is what refuses it
			c := lockCluster(t)
			if test.setup != nil {
				test.setup(c)
			}
			if err := c.apply(test.kind, test.actor, test.payload(c, 0)); err != nil {
				t.Fatalf("unlocked: %v", err)
			}

			c = lockCluster(t)
			if test.setup != nil {
				test.setup(c)
			}
			c.startOperation(test.lock)
			err := c.apply(test.kind, test.actor, test.payload(c, 0))
			switch {
			case test.free && err != nil:
				t.Errorf("under a lock: %v", err)
			case !test.free && (err == nil || !strings.Contains(err.Error(), "is locked by operation")):
				t.Errorf("under another's lock: want it refused, got %v", err)
			}
			if !test.byOperation {
				return
			}

			c = lockCluster(t)
			if test.setup != nil {
				test.setup(c)
			}
			holder := c.startOperation(test.lock)
			if err := c.apply(test.kind, test.actor, test.payload(c, holder)); err != nil {
				t.Errorf("by the operation that holds the lock: %v", err)
			}
		})
	}
}

func TestOnlyTheLeaderActsForAnOperation(t *testing.T) {
	c := lockCluster(t)
	holder := c.startOperation("host/1")
	c.refuse(KindSetHostPhase, UserActor("alice"), SetHostPhase{Host: 1, Phase: Draining, Operation: holder},
		"only the leader runs operations")
	c.refuse(KindStartOperation, UserActor("alice"), StartOperation{Kind: "test", Locks: []string{"host/0"}},
		"only the leader")
	c.refuse(KindFinishOperation, HostActor(1), FinishOperation{ID: holder}, "only the leader")
}

func TestAnOperationLocksWhatThereIsAndFreesItWhenFinished(t *testing.T) {
	c := lockCluster(t)
	for _, test := range []struct{ lock, wantError string }{
		{"stack/default/blog", "no stack default/blog"},
		{"volume/default/logs", "no volume default/logs"},
		{"host/7", "host 7 is not a member"},
		{"host/one", "no host ID"},
		{"namespace/default", "none of"},
	} {
		c.refuse(KindStartOperation, LeaderActor, StartOperation{Kind: "test", Locks: []string{test.lock}}, test.wantError)
	}
	c.refuse(KindStartOperation, LeaderActor, StartOperation{Kind: "test"}, "locks nothing")

	first := c.startOperation("stack/default/shop", "volume/default/data")
	c.refuse(KindStartOperation, LeaderActor, StartOperation{Kind: "test", Locks: []string{"volume/default/data"}},
		fmt.Sprintf("volume/default/data is locked by operation %d (test)", first))
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: first, Failed: true, Error: "gave up"})
	if operation := c.state.Operations[first]; operation.State != OpFailed || operation.Error != "gave up" ||
		len(c.state.Locks) != 0 {
		t.Errorf("finished: %+v, locks %v", operation, c.state.Locks)
	}
	c.refuse(KindFinishOperation, LeaderActor, FinishOperation{ID: first}, "is finished (failed)")
	c.startOperation("volume/default/data")

	// the state keeps the last keptFinished finished operations
	for range keptFinished {
		c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: c.startOperation("host/1")})
	}
	if _, kept := c.state.Operations[first]; kept || len(c.state.Operations) != keptFinished+1 {
		t.Errorf("%d operations kept, the oldest finished one too: %t", len(c.state.Operations), kept)
	}
}

func TestAnOperationAdvancesOnlyByTheOwnerOfItsStep(t *testing.T) {
	c := lockCluster(t)
	operation := c.startOperation("stack/default/shop")
	one := HostID(1)
	// the leader runs the first step and hands the second to host 1
	c.refuse(KindAdvanceOperation, HostActor(1), AdvanceOperation{ID: operation, FromStep: 0, Step: 1, Owner: &one},
		"leader runs its step 1, not host/1")
	c.refuse(KindAdvanceOperation, UserActor("alice"), AdvanceOperation{ID: operation, FromStep: 0, Step: 1},
		"leader runs its step 1, not user/alice")
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 2, Step: 3},
		"at step 0, no longer 2")
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: 0},
		"step 0 does not follow step 0")
	gone := HostID(7)
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: 1, Owner: &gone},
		"host 7 is not a member")
	c.mustApply(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 0, Step: 1, Owner: &one})

	// now host 1 runs it, and only host 1
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 1, Step: 2},
		"host/1 runs its step 2, not leader")
	c.refuse(KindAdvanceOperation, HostActor(0), AdvanceOperation{ID: operation, FromStep: 1, Step: 2},
		"host/1 runs its step 2, not host/0")
	c.mustApply(KindAdvanceOperation, HostActor(1), AdvanceOperation{ID: operation, FromStep: 1, Step: 2})
	// the same step told again, by a host that took long to tell
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 1, Step: 2},
		"at step 2, no longer 1")
	if got := c.state.Operations[operation]; got.Step != 2 || got.Owner != nil {
		t.Errorf("after step 2: %+v", got)
	}
	c.mustApply(KindFinishOperation, LeaderActor, FinishOperation{ID: operation})
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: operation, FromStep: 2, Step: 3}, "is finished (done)")
	c.refuse(KindAdvanceOperation, LeaderActor, AdvanceOperation{ID: 999, FromStep: 0, Step: 1}, "no operation 999")
}

// refuseStale applies a leader's command that must be refused as planned on
// an older state.
func (c *cluster) refuseStale(kind string, payload any) {
	c.t.Helper()
	if err := c.apply(kind, LeaderActor, payload); !errors.Is(err, ErrStale) {
		c.t.Errorf("%s: want ErrStale, got %v", kind, err)
	}
}

func TestALeaderCommandPlannedOnAnOlderStateIsRefused(t *testing.T) {
	zero := HostID(0)
	place := func(plannedAt uint64) SetStackStatus {
		return SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &zero, PlannedAt: plannedAt}
	}
	pushOut := func(plannedAt uint64) SetPushedOut {
		return SetPushedOut{Stacks: map[string][]string{"default/shop": {"app"}}, PlannedAt: plannedAt}
	}

	// changed between the leader's copy and its commit
	c := newCluster(t)
	c.putShop(shopSpec())
	planned := c.index
	changed := shopSpec()
	changed.Services["cache"] = stackspec.Service{Image: "x"}
	c.putShop(changed)
	c.refuseStale(KindSetStackStatus, place(planned))
	c.refuseStale(KindSetPushedOut, pushOut(planned))
	c.refuse(KindSetStackStatus, LeaderActor, place(0), "names no state it was planned with")
	c.mustApply(KindSetStackStatus, LeaderActor, place(c.index))

	// deleted
	c = newCluster(t)
	c.putShop(shopSpec())
	planned = c.index
	deleteShop := DeleteResource{Kind: resource.KindStack, Namespace: "default", Name: "shop", Owner: OwnerCLI}
	c.mustApply(KindDeleteResource, UserActor("alice"), deleteShop)
	c.refuse(KindSetStackStatus, LeaderActor, place(planned), "no stack default/shop")
	c.refuse(KindSetPushedOut, LeaderActor, pushOut(planned), "no stack default/shop")

	// made anew under the same name: the same spec and generation, but
	// another stack
	c.putShop(shopSpec())
	c.refuseStale(KindSetStackStatus, place(planned))
	c.refuseStale(KindSetPushedOut, pushOut(planned))

	// a volume whose host told it holds data since
	c = lockCluster(t)
	planned = c.index
	c.mustApply(KindSetVolumeInitialized, HostActor(0), SetVolumeInitialized{Volume: "default/data", DataGeneration: 1})
	restored := c.placedData()
	restored.DataGeneration++
	c.refuseStale(KindPutVolume, PutVolume{Volume: restored, PlannedAt: planned})
	c.refuse(KindPutVolume, LeaderActor, PutVolume{Volume: restored}, "names no state it was planned with")
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{Volume: restored, PlannedAt: c.index})
}

func TestTheLeaderGivesAHostOnlyToAStackThatHasNone(t *testing.T) {
	c := lockCluster(t) // shop runs on host 0
	one := HostID(1)
	c.refuse(KindSetStackStatus, LeaderActor,
		SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &one, PlannedAt: c.index},
		"runs on host 0; only an operation that holds its lock moves it")
	// an operation that holds another stack's lock does not move it either
	c.putBlog()
	other := c.startOperation("stack/default/blog")
	c.refuse(KindSetStackStatus, LeaderActor,
		SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &one, PlannedAt: c.index, Operation: other},
		"only an operation that holds its lock moves it")
	// the one that holds its lock does
	holder := c.startOperation("stack/default/shop")
	c.mustApply(KindSetStackStatus, LeaderActor,
		SetStackStatus{ID: "default/shop", Status: StackStarting, Host: &one, PlannedAt: c.index, Operation: holder})
	if host := c.state.Stacks["default/shop"].Host; host == nil || *host != 1 {
		t.Errorf("moved to host %v", host)
	}
}

// putBlog applies a second stack, default/blog.
func (c *cluster) putBlog() {
	c.t.Helper()
	blog := stackspec.Spec{Services: map[string]stackspec.Service{"web": {Image: "x"}}}
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindStack, "default", "blog", blog))
}
