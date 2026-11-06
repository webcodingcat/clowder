package state

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// encryptedOf stands in for a value of size bytes encrypted by datakey: only
// its length counts for the state.
func encryptedOf(size int) Encrypted {
	chunks := max(1, (size+encryptedChunk-1)/encryptedChunk)
	return make(Encrypted, encryptedHeader+size+chunks*encryptedTag)
}

func TestABatchGoesInWholeOrNotAtAll(t *testing.T) {
	accepted := []ResourceChange{
		putChange(resource.KindNamespace, "", "shop", nil),
		putChange(resource.KindSecret, "shop", "token", encryptedValue(encryptedOf(5), "d")),
		putChange(resource.KindStack, "shop", "web", shopSpec()),
	}
	refused := deleteChange(resource.KindNamespace, "", names.Default)
	for index := range len(accepted) + 1 {
		t.Run(fmt.Sprintf("refused at %d", index), func(t *testing.T) {
			c := newCluster(t)
			before := c.encoded()
			changes := slices.Insert(slices.Clone(accepted), index, refused)
			err := c.apply(KindChangeResources, cliActor, batch(changes...))
			var command *CommandError
			var change *ChangeError
			if !errors.As(err, &command) || !errors.As(err, &change) || change.Index != index ||
				!strings.Contains(err.Error(), "namespace default belongs to the cluster") {
				t.Fatalf("want change %d refused, got %v", index, err)
			}
			if after := c.encoded(); after != before {
				t.Errorf("the refused batch changed the state:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestAChangeSeesTheChangesAboveIt(t *testing.T) {
	tests := []struct {
		name          string
		first, second ResourceChange
		wantError     string
	}{
		{
			"a namespace and its stack",
			putChange(resource.KindNamespace, "", "shop", nil),
			putChange(resource.KindStack, "shop", "web", shopSpec()),
			`no namespace "shop"`,
		},
		{
			"a Secret and the configuration that names it",
			hetznerSecret(),
			configChange(testConfig()),
			"clowder-system has no Secret hetzner",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			err := c.apply(KindChangeResources, cliActor, batch(test.second, test.first))
			var change *ChangeError
			if !errors.As(err, &change) || change.Index != 0 || !strings.Contains(err.Error(), test.wantError) {
				t.Errorf("below what it needs: want change 0 refused with %q, got %v", test.wantError, err)
			}
			changes, err := c.changes(KindChangeResources, cliActor, batch(test.first, test.second))
			if err != nil || len(changes) != 2 {
				t.Errorf("below what it needs: changes %v, %v", changes, err)
			}
		})
	}
}

func TestABatchThatChangesNothingLeavesNoAuditEntry(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	c.put(resource.KindNamespace, "", "shop", nil)
	before := c.encoded()
	changes, err := c.changes(KindChangeResources, cliActor, batch(
		hetznerSecret(),
		configChange(testConfig()),
		putChange(resource.KindNamespace, "", "shop", nil),
		putChange(resource.KindNamespace, "", names.Default, nil),
	))
	if err != nil || len(changes) != 0 {
		t.Errorf("changes %v, %v", changes, err)
	}
	if after := c.encoded(); after != before {
		t.Errorf("an unchanged batch changed the state or its audit:\nbefore %s\nafter  %s", before, after)
	}
	c.refuse(KindChangeResources, cliActor, batch(), "no changes")
	c.refuse(KindChangeResources, cliActor, batch(ResourceChange{}), "a change is one put or one delete")
}

func TestAnUnchangedResourceIsNoChange(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	shop := putChange(resource.KindNamespace, "", "shop", nil)
	changes, err := c.changes(KindChangeResources, cliActor, batch(shop))
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{Kind: resource.KindNamespace, Name: "shop", ResourceVersion: c.index}}
	if !slices.Equal(changes, want) {
		t.Errorf("changes %v, want %v", changes, want)
	}
	volume := putChange(resource.KindVolume, "shop", "data", testVolumeSpec())
	c.mustApply(KindChangeResources, cliActor, batch(volume))
	volumeIndex := c.index
	before := c.encoded()
	for _, again := range []ResourceChange{shop, volume, putChange(resource.KindNamespace, "", "default", nil)} {
		if changes, err := c.changes(KindChangeResources, cliActor, batch(again)); err != nil || len(changes) != 0 {
			t.Errorf("%s %s again: changes %v, %v", again.Put.Kind, again.Put.Name, changes, err)
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
	c.put(resource.KindNamespace, "", "shop", nil)
	const gitops = "gitops:infra" // another channel
	// refused even though nothing would change: another channel learns nothing
	ofGitops := putChange(resource.KindNamespace, "", "shop", nil)
	ofGitops.Put.Owner = gitops
	c.refuseChange(ofGitops, "belongs to cli, not to gitops:infra")
	deleteByGitops := deleteChange(resource.KindNamespace, "", "shop")
	deleteByGitops.Delete.Owner = gitops
	c.refuseChange(deleteByGitops, "belongs to cli")
	// the namespace's owner owns only the namespace
	secret := putChange(resource.KindSecret, "shop", "token", encryptedValue(encryptedOf(5), "d1"))
	secret.Put.Owner = gitops
	c.mustApply(KindChangeResources, cliActor, batch(secret))
	// default belongs to no channel
	c.put(resource.KindSecret, "default", "token", encryptedValue(encryptedOf(5), "d2"))
	c.refuseChange(putChange(resource.KindVolume, "nowhere", "data", testVolumeSpec()), `no namespace "nowhere"`)
	c.refuseChange(putChange("Pod", "shop", "web", nil), `no kind of resource "Pod"`)
}

func TestANamespaceGoesOnlyEmptyAndDefaultStays(t *testing.T) {
	c := newCluster(t)
	c.put(resource.KindNamespace, "", "shop", nil)
	c.refuseChange(putChange(resource.KindNamespace, "", "clowder-apps", nil), "do not start with clowder-")
	c.refuseChange(putChange(resource.KindNamespace, "shop", "inner", nil), "lives in no namespace")
	c.put(resource.KindNamespace, "", "blog", map[string]int{})
	c.put(resource.KindSecret, "shop", "token", encryptedValue(encryptedOf(5), "d"))
	shop := deleteChange(resource.KindNamespace, "", "shop")
	c.refuseChange(shop, "still holds Secret shop/token")
	c.del(resource.KindSecret, "shop", "token")
	changes, err := c.changes(KindChangeResources, cliActor, batch(shop))
	if want := []Change{{Kind: resource.KindNamespace, Name: "shop", ResourceVersion: c.index, Deleted: true}}; err != nil ||
		!slices.Equal(changes, want) {
		t.Errorf("changes %v, want %v (%v)", changes, want, err)
	}
	c.refuseChange(deleteChange(resource.KindNamespace, "", "default"), "belongs to the cluster")
}

func TestANamespacesImagePullSecretsRaiseItsGeneration(t *testing.T) {
	c := newCluster(t)
	c.put(resource.KindNamespace, "", "shop", nil)
	pulling := resource.NamespaceSpec{ImagePullSecrets: []resource.LocalObjectReference{{Name: "ghcr"}}}
	c.put(resource.KindNamespace, "", "shop", pulling)
	if shop := c.state.Namespaces["shop"]; shop.Generation != 2 || !sameJSON(shop.Spec, pulling) {
		t.Errorf("namespace %+v", shop)
	}
	if changes, err := c.changes(KindChangeResources, cliActor,
		batch(putChange(resource.KindNamespace, "", "shop", pulling))); err != nil || len(changes) != 0 {
		t.Errorf("the same spec again: changes %v, %v", changes, err)
	}
	twice := resource.NamespaceSpec{ImagePullSecrets: []resource.LocalObjectReference{{Name: "ghcr"}, {Name: "ghcr"}}}
	c.refuseChange(putChange(resource.KindNamespace, "", "shop", twice), "imagePullSecrets: ghcr is named twice")
}

func TestClowderSystemHoldsOnlyTheClustersSecrets(t *testing.T) {
	c := newCluster(t)
	if system := c.state.Namespaces[names.System]; system == nil || system.Generation != 1 {
		t.Fatalf("namespace %s after the bootstrap: %+v", names.System, system)
	}
	tests := []struct {
		name      string
		change    ResourceChange
		wantError string
	}{
		{"its delete", deleteChange(resource.KindNamespace, "", names.System), "namespace clowder-system belongs to the cluster"},
		{"a stack", putChange(resource.KindStack, names.System, "shop", shopSpec()), "holds the cluster's Secrets only, no Stacks"},
		{"a volume", putChange(resource.KindVolume, names.System, "data", testVolumeSpec()), "holds the cluster's Secrets only, no Volumes"},
		{
			"image pull secrets",
			putChange(resource.KindNamespace, "", names.System,
				resource.NamespaceSpec{ImagePullSecrets: []resource.LocalObjectReference{{Name: "ghcr"}}}),
			"takes no image pull secrets",
		},
	}
	for _, test := range tests {
		c.refuseChange(test.change, test.wantError)
	}
	// put as it is, it stays as it is
	if changes, err := c.changes(KindChangeResources, cliActor,
		batch(putChange(resource.KindNamespace, "", names.System, nil))); err != nil || len(changes) != 0 {
		t.Errorf("%s put as it is: changes %v, %v", names.System, changes, err)
	}
}

func TestAVolumeWaitsForItsStackAndCannotChangeYet(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	spec := testVolumeSpec()
	unknown := spec
	unknown.Class = "slow"
	c.refuseChange(putChange(resource.KindVolume, "default", "data", unknown), `no storage class "slow"`)
	c.refuseChange(putChange(resource.KindVolume, "default", "data", resource.VolumeSpec{}), "class is required")
	c.put(resource.KindVolume, "default", "data", spec)
	volume := c.state.Volumes["default/data"]
	if volume.Status != VolumePending || volume.DataGeneration != 1 || len(volume.Copies) != 0 {
		t.Errorf("new volume %+v", volume)
	}
	larger := spec
	larger.Size *= 2
	c.refuseChange(putChange(resource.KindVolume, "default", "data", larger), "cannot change a volume yet")
	// its class stays while it is there
	config := testConfig()
	config.StorageClasses = map[string]clusterspec.StorageClass{"slow": {Pool: "vg-slow/thin"}}
	c.refuseChange(configChange(config), "volume default/data has storage class fast, so the class must stay")
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
	c.del(resource.KindVolume, "default", "data")
	if want := []RetiredCopy{{Volume: "default/data", DataGeneration: 1, Host: 0}}; !slices.Equal(c.state.Retired, want) {
		t.Errorf("retired %v, want %v", c.state.Retired, want)
	}
	c.put(resource.KindVolume, "default", "data", spec)
	if generation := c.state.Volumes["default/data"].DataGeneration; generation != 2 {
		t.Errorf("data generation %d after a retired copy of 1", generation)
	}
}

func TestTheConfigurationKeepsTheClassACopyIsIn(t *testing.T) {
	c := newCluster(t)
	config := testConfig()
	config.StorageClasses["fast"] = clusterspec.StorageClass{Fallback: []string{"bulk"}}
	config.StorageClasses["bulk"] = clusterspec.StorageClass{Pool: "vg-bulk/thin"}
	c.configure(config)
	c.put(resource.KindVolume, "default", "data", testVolumeSpec())
	// fast is only its fallback: the copy is in bulk
	placed := *c.state.Volumes["default/data"]
	minor := 0
	placed.Status, placed.Minor, placed.Copies = VolumeOK, &minor, []VolumeCopy{{Host: 0, Class: "bulk"}}
	c.mustApply(KindPutVolume, LeaderActor, PutVolume{PlannedAt: c.index, Volume: placed})
	c.refuseChange(configChange(testConfig()), "volume default/data has storage class bulk, so the class must stay")
}

func TestSecretsChangeByDigestWithinTheirLimits(t *testing.T) {
	c := newCluster(t)
	secret := func(name string, size int, digest string) ResourceChange {
		return putChange(resource.KindSecret, "default", name, encryptedValue(encryptedOf(size), digest))
	}
	c.mustApply(KindChangeResources, cliActor, batch(secret("token", 10, "d1")))
	// encrypted anew, the same value has the same digest
	if changes, err := c.changes(KindChangeResources, cliActor, batch(secret("token", 10, "d1"))); err != nil || len(changes) != 0 {
		t.Errorf("the same value: changes %v, %v", changes, err)
	}
	c.mustApply(KindChangeResources, cliActor, batch(secret("token", 12, "d2")))
	if stored := c.state.Secrets["default/token"]; stored.Generation != 2 || ClearSize(stored.Spec.Data["value"]) != 12 {
		t.Errorf("changed secret %+v", stored.Meta)
	}
	tests := []struct {
		name      string
		change    ResourceChange
		wantError string
	}{
		{"too large", secret("big", resource.MaxSecretBytes+1, "d"), "larger than 65536 bytes"},
		{"empty", secret("empty", 0, "d"), "the value of value is empty"},
		{"no digest", secret("plain", 10, ""), "no digest"},
		{"not encrypted", putChange(resource.KindSecret, "default", "short", encryptedValue(Encrypted("x"), "d")), "the value of value is not encrypted"},
		{"no keys", putChange(resource.KindSecret, "default", "none", EncryptedSecret{Digest: "d"}), "has no keys"},
		{
			"a key that is no file name",
			putChange(resource.KindSecret, "default", "odd", EncryptedSecret{Data: map[string]Encrypted{"a/b": encryptedOf(5)}, Digest: "d"}),
			`key "a/b"`,
		},
	}
	for _, test := range tests {
		c.refuseChange(test.change, test.wantError)
	}
	for _, name := range []string{"a", "b", "c"} {
		c.mustApply(KindChangeResources, cliActor, batch(secret(name, resource.MaxSecretBytes, "d-"+name)))
	}
	c.refuseChange(secret("d", resource.MaxSecretBytes, "d"),
		"the secrets of namespace default would be larger than 262144 bytes")
}

func TestACreateRefusesAResourceThatExists(t *testing.T) {
	c := newCluster(t)
	create := putChange(resource.KindSecret, "default", "token", encryptedValue(encryptedOf(10), "d1"))
	create.Put.Create = true
	c.mustApply(KindChangeResources, cliActor, batch(create))
	c.refuseChange(create, "Secret default/token already exists")
	// the same value without Create changes nothing, as apply does
	create.Put.Create = false
	if changes, err := c.changes(KindChangeResources, cliActor, batch(create)); err != nil || len(changes) != 0 {
		t.Errorf("an apply of the same value: changes %v, %v", changes, err)
	}
}

func TestTheCountOfSecretsHoldsForClowderSystemToo(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	// the Hetzner token's Secret and 999 more make the 1000 allowed
	var secrets []ResourceChange
	for index := range resource.MaxClusterSecrets - 1 {
		name := fmt.Sprintf("s%d", index)
		secrets = append(secrets, putChange(resource.KindSecret, "default", name, encryptedValue(encryptedOf(1), name)))
	}
	c.mustApply(KindChangeResources, cliActor, batch(secrets...))
	for _, namespace := range []string{names.Default, names.System} {
		c.refuseChange(putChange(resource.KindSecret, namespace, "one-more", encryptedValue(encryptedOf(1), "d")),
			"the cluster would have 1001 secrets, more than 1000")
	}
}

func TestTheSecretsOfTheClusterHaveALimitInAll(t *testing.T) {
	c := newCluster(t)
	// 16 namespaces with 4 Secrets of 64 KiB each are the 4 MiB the
	// cluster may hold; clowder-system counts as any other
	full := []ResourceChange{}
	namespaces := []string{names.Default, names.System}
	for index := range 14 {
		namespace := fmt.Sprintf("n%d", index)
		namespaces = append(namespaces, namespace)
		full = append(full, putChange(resource.KindNamespace, "", namespace, nil))
	}
	for _, namespace := range namespaces {
		for index := range 4 {
			name := fmt.Sprintf("s%d", index)
			full = append(full, putChange(resource.KindSecret, namespace, name,
				encryptedValue(encryptedOf(resource.MaxSecretBytes), namespace+"/"+name)))
		}
	}
	c.mustApply(KindChangeResources, cliActor, batch(full...))
	c.mustApply(KindChangeResources, cliActor, batch(putChange(resource.KindNamespace, "", "more", nil)))
	c.refuseChange(putChange(resource.KindSecret, "more", "token", encryptedValue(encryptedOf(1), "d")),
		"secret more/token: the secrets of the cluster would be larger than 4194304 bytes")
}

func TestClearSizeUndoesWhatEncryptionAdds(t *testing.T) {
	for _, size := range []int{0, 1, encryptedChunk - 1, encryptedChunk, encryptedChunk + 1, 3*encryptedChunk + 7} {
		if got := ClearSize(encryptedOf(size)); got != size {
			t.Errorf("%d bytes in clear: ClearSize %d", size, got)
		}
	}
	if got := ClearSize(Encrypted("short")); got != -1 {
		t.Errorf("too short: %d", got)
	}
}
