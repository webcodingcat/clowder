package state

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/resource"
)

// putResource is the command clowder apply sends for one resource.
func putResource(kind, namespace, name string, spec any) PutResource {
	encoded, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	return PutResource{Kind: kind, Namespace: namespace, Name: name, Owner: OwnerCLI, Spec: encoded}
}

// sealedOf stands in for a value of size bytes sealed by datakey: only its
// length counts for the state.
func sealedOf(size int) Sealed {
	chunks := max(1, (size+sealedChunk-1)/sealedChunk)
	return make(Sealed, sealedHeader+size+chunks*sealedTag)
}

func TestAnUnchangedResourceIsNoChange(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	shop := putResource(resource.KindNamespace, "", "shop", nil)
	changes, err := c.changes(KindPutResource, UserActor("alice"), shop)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{Kind: resource.KindNamespace, Name: "shop", ResourceVersion: c.index}}
	if !slices.Equal(changes, want) {
		t.Errorf("changes %v, want %v", changes, want)
	}
	volume := putResource(resource.KindVolume, "shop", "data", testVolumeSpec())
	c.mustApply(KindPutResource, UserActor("alice"), volume)
	volumeIndex := c.index
	before := c.encoded()
	for _, again := range []PutResource{shop, volume, putResource(resource.KindNamespace, "", "default", nil)} {
		if changes, err := c.changes(KindPutResource, UserActor("bob"), again); err != nil || len(changes) != 0 {
			t.Errorf("%s %s again: changes %v, %v", again.Kind, again.Name, changes, err)
		}
	}
	if after := c.encoded(); after != before {
		t.Errorf("an unchanged apply changed the state:\nbefore %s\nafter  %s", before, after)
	}
	if meta := c.state.Volumes["shop/data"].Meta; meta.Generation != 1 || meta.ResourceVersion != volumeIndex {
		t.Errorf("volume %+v", meta)
	}
}

func TestAResourceBelongsToTheChannelThatAppliedIt(t *testing.T) {
	c := newCluster(t)
	shop := putResource(resource.KindNamespace, "", "shop", nil)
	c.mustApply(KindPutResource, UserActor("alice"), shop)
	gitops := shop
	gitops.Owner = "gitops:infra" // another channel
	// refused even though nothing would change: another channel learns nothing
	c.refuse(KindPutResource, UserActor("alice"), gitops, "belongs to cli, not to gitops:infra")
	c.refuse(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindNamespace, Name: "shop", Owner: gitops.Owner}, "belongs to cli")
	// the namespace's owner owns only the namespace
	secret := putResource(resource.KindSecret, "shop", "token", SealedSecret{Value: sealedOf(5), Digest: "d1"})
	secret.Owner = gitops.Owner
	c.mustApply(KindPutResource, UserActor("alice"), secret)
	// default belongs to no channel
	inDefault := putResource(resource.KindSecret, "default", "token", SealedSecret{Value: sealedOf(5), Digest: "d2"})
	c.mustApply(KindPutResource, UserActor("alice"), inDefault)
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "nowhere", "data", testVolumeSpec()),
		`no namespace "nowhere"`)
	c.refuse(KindPutResource, UserActor("alice"), putResource("Pod", "shop", "web", nil), `no kind of resource "Pod"`)
}

func TestANamespaceGoesOnlyEmptyAndDefaultStays(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "", "shop", nil))
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "", "clowder-system", nil),
		"do not start with clowder-")
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "shop", "inner", nil),
		"lives in no namespace")
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "", "blog", map[string]int{"size": 1}),
		"a namespace has no spec")
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindNamespace, "", "blog", map[string]int{}))
	c.mustApply(KindPutResource, UserActor("alice"),
		putResource(resource.KindSecret, "shop", "token", SealedSecret{Value: sealedOf(5), Digest: "d"}))
	shop := DeleteResource{Kind: resource.KindNamespace, Name: "shop", Owner: OwnerCLI}
	c.refuse(KindDeleteResource, UserActor("alice"), shop, "still holds Secret shop/token")
	c.mustApply(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindSecret, Namespace: "shop", Name: "token", Owner: OwnerCLI})
	changes, err := c.changes(KindDeleteResource, UserActor("alice"), shop)
	if want := []Change{{Kind: resource.KindNamespace, Name: "shop", ResourceVersion: c.index, Deleted: true}}; err != nil ||
		!slices.Equal(changes, want) {
		t.Errorf("changes %v, want %v (%v)", changes, want, err)
	}
	c.refuse(KindDeleteResource, UserActor("alice"), DeleteResource{Kind: resource.KindNamespace, Name: "default"},
		"belongs to the cluster")
}

func TestAVolumeWaitsForItsStackAndCannotChangeYet(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	spec := testVolumeSpec()
	unknown := spec
	unknown.Class = "slow"
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", unknown),
		`no storage class "slow"`)
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", resource.VolumeSpec{}),
		"class is required")
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", spec))
	volume := c.state.Volumes["default/data"]
	if volume.Status != VolumePending || volume.DataGeneration != 1 || len(volume.Copies) != 0 {
		t.Errorf("new volume %+v", volume)
	}
	larger := spec
	larger.Size *= 2
	c.refuse(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", larger),
		"cannot change a volume yet")
	// its class stays while it is there
	config := testConfig()
	config.StorageClasses = map[string]clusterspec.StorageClass{"slow": {Pool: "vg-slow/thin"}}
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()},
		"volume default/data has storage class fast, so the class must stay")
	// the leader records where it is, and only that
	placed := *volume
	placed.Status = VolumeOK
	placed.Copies = []VolumeCopy{{Host: 0, Class: "fast"}}
	placed.Spec.Replicas = 0
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed}, "its spec changes only with an apply")
	placed.Spec.Replicas = 1
	// the record as it is, with a Meta the sender read long ago: nothing changes
	stale := *volume
	stale.ResourceVersion = 1
	if changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: stale}); err != nil || len(changes) != 0 {
		t.Errorf("a pending volume as it is: changes %v, %v", changes, err)
	}
	pendingWithCopy := *volume
	pendingWithCopy.Copies = placed.Copies
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: pendingWithCopy}, "a pending volume has no copies")
	minor := 3
	placed.Minor = &minor
	changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed})
	if err != nil || len(changes) != 1 || changes[0].Name != "data" {
		t.Errorf("placed: changes %v, %v", changes, err)
	}
	if changes, err := c.changes(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: *c.state.Volumes["default/data"]}); err != nil ||
		len(changes) != 0 {
		t.Errorf("the same record again: changes %v, %v", changes, err)
	}
	// placed, it keeps a copy: only its delete gives up its data
	backToPending := *c.state.Volumes["default/data"]
	backToPending.Status, backToPending.Copies, backToPending.Minor = VolumePending, nil, nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: backToPending}, "holds data")
	noCopy := *c.state.Volumes["default/data"]
	noCopy.Copies = nil
	c.refuse(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: noCopy}, "holds data")
	// a volume made again after its delete starts above its retired copies
	c.mustApply(KindDeleteResource, UserActor("alice"),
		DeleteResource{Kind: resource.KindVolume, Namespace: "default", Name: "data", Owner: OwnerCLI})
	if want := []RetiredCopy{{Volume: "default/data", DataGeneration: 1, Host: 0}}; !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", spec))
	if generation := c.state.Volumes["default/data"].DataGeneration; generation != 2 {
		t.Errorf("data generation %d after a retired copy of 1", generation)
	}
}

func TestClusterYAMLKeepsTheClassACopyIsIn(t *testing.T) {
	c := newCluster(t)
	config := testConfig()
	config.StorageClasses["fast"] = clusterspec.StorageClass{Fallback: []string{"bulk"}}
	config.StorageClasses["bulk"] = clusterspec.StorageClass{Pool: "vg-bulk/thin"}
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: config, Secrets: testSecrets()})
	c.mustApply(KindPutResource, UserActor("alice"), putResource(resource.KindVolume, "default", "data", testVolumeSpec()))
	// fast is only its fallback: the copy is in bulk
	placed := *c.state.Volumes["default/data"]
	minor := 0
	placed.Status, placed.Minor, placed.Copies = VolumeOK, &minor, []VolumeCopy{{Host: 0, Class: "bulk"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed})
	c.refuse(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()},
		"volume default/data has storage class bulk, so the class must stay")
}

func TestSecretsChangeByDigestWithinTheirLimits(t *testing.T) {
	c := newCluster(t)
	put := func(name string, size int, digest string) PutResource {
		return putResource(resource.KindSecret, "default", name, SealedSecret{Value: sealedOf(size), Digest: digest})
	}
	c.mustApply(KindPutResource, UserActor("alice"), put("token", 10, "d1"))
	// sealed anew, the same value has the same digest
	if changes, err := c.changes(KindPutResource, UserActor("alice"), put("token", 10, "d1")); err != nil || len(changes) != 0 {
		t.Errorf("the same value: changes %v, %v", changes, err)
	}
	c.mustApply(KindPutResource, UserActor("alice"), put("token", 12, "d2"))
	if secret := c.state.Secrets["default/token"]; secret.Generation != 2 || ClearSize(secret.Spec.Value) != 12 {
		t.Errorf("changed secret %+v", secret.Meta)
	}
	c.refuse(KindPutResource, UserActor("alice"), put("big", resource.MaxSecretBytes+1, "d"), "larger than 65536 bytes")
	c.refuse(KindPutResource, UserActor("alice"), put("empty", 0, "d"), "is empty")
	c.refuse(KindPutResource, UserActor("alice"), put("plain", 10, ""), "no digest")
	c.refuse(KindPutResource, UserActor("alice"),
		putResource(resource.KindSecret, "default", "short", SealedSecret{Value: Sealed("x"), Digest: "d"}), "not sealed")
	for _, name := range []string{"a", "b", "c"} {
		c.mustApply(KindPutResource, UserActor("alice"), put(name, resource.MaxSecretBytes, "d-"+name))
	}
	c.refuse(KindPutResource, UserActor("alice"), put("d", resource.MaxSecretBytes, "d"),
		"the secrets of namespace default would be larger than 262144 bytes")
}

func TestTheCountOfSecretsHoldsForResourcesAndClusterYAMLAlike(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindApplyConfig, UserActor("alice"), ApplyConfig{Spec: testConfig(), Secrets: testSecrets()})
	// cluster.yaml's one value and 999 resources make the 1000 allowed
	for index := range resource.MaxClusterSecrets - 1 {
		name := fmt.Sprintf("s%d", index)
		c.mustApply(KindPutResource, UserActor("alice"),
			putResource(resource.KindSecret, "default", name, SealedSecret{Value: sealedOf(1), Digest: name}))
	}
	c.refuse(KindPutResource, UserActor("alice"),
		putResource(resource.KindSecret, "default", "one-more", SealedSecret{Value: sealedOf(1), Digest: "d"}),
		"the cluster would have 1001 secrets, more than 1000")
	c.refuse(KindApplyConfig, UserActor("alice"), withRegistries(1, 16), "cluster.yaml: the cluster would have 1001 secrets")
}

// withRegistries is testConfig with registry passwords of a size in clear.
func withRegistries(count, size int) ApplyConfig {
	applied := ApplyConfig{Spec: testConfig(), Secrets: testSecrets()}
	applied.Spec.Registries = map[string]clusterspec.Registry{}
	for index := range count {
		host := fmt.Sprintf("r%d", index)
		name := "registries." + host + ".password"
		applied.Spec.Registries[host] = clusterspec.Registry{Username: "robot", Password: clusterspec.SecretRef(name)}
		applied.Secrets[name] = SealedSecret{Value: sealedOf(size), Digest: name}
	}
	return applied
}

func TestClusterYAMLsValuesCountTowardsTheClustersSize(t *testing.T) {
	c := newCluster(t)
	// 64 values of 64 KiB are the 4 MiB the cluster may hold
	full := withRegistries(63, resource.MaxSecretBytes)
	full.Secrets["hoster.token"] = SealedSecret{Value: sealedOf(resource.MaxSecretBytes), Digest: "d"}
	c.mustApply(KindApplyConfig, UserActor("alice"), full)
	c.refuse(KindPutResource, UserActor("alice"),
		putResource(resource.KindSecret, "default", "token", SealedSecret{Value: sealedOf(1), Digest: "d"}),
		"secret default/token: the secrets of the cluster would be larger than 4194304 bytes")
	over := withRegistries(64, resource.MaxSecretBytes)
	over.Secrets["hoster.token"] = SealedSecret{Value: sealedOf(1), Digest: "d"}
	c.refuse(KindApplyConfig, UserActor("alice"), over, "the secrets of the cluster would be larger than 4194304 bytes")
	big := withRegistries(1, resource.MaxSecretBytes+1)
	c.refuse(KindApplyConfig, UserActor("alice"), big, "registries.r0.password is larger than 65536 bytes")
}

func TestClearSizeUndoesWhatSealingAdds(t *testing.T) {
	for _, size := range []int{0, 1, sealedChunk - 1, sealedChunk, sealedChunk + 1, 3*sealedChunk + 7} {
		if got := ClearSize(sealedOf(size)); got != size {
			t.Errorf("%d bytes in clear: ClearSize %d", size, got)
		}
	}
	if got := ClearSize(Sealed("short")); got != -1 {
		t.Errorf("too short: %d", got)
	}
}
