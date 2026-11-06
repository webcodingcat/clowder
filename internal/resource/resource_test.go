package resource

import (
	"strings"
	"testing"
)

func TestAVolumeNeedsAClassASizeAndReplicasFromZeroToThree(t *testing.T) {
	if err := (VolumeSpec{Class: "fast", Size: 1 << 30, Replicas: 0}).Check(); err != nil {
		t.Errorf("a plain volume: %v", err)
	}
	err := (VolumeSpec{Replicas: 4}).Check()
	for _, want := range []string{"class is required", "size is required", "replicas 4"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
}

func TestASealedSecretIsStoredAsASecret(t *testing.T) {
	if StoredKind(KindSealedSecret) != KindSecret || StoredKind(KindVolume) != KindVolume {
		t.Error("wrong stored kinds")
	}
}

func TestANamespaceNamesEachPullSecretOnce(t *testing.T) {
	valid := NamespaceSpec{ImagePullSecrets: []LocalObjectReference{{Name: "ghcr"}, {Name: "docker-hub"}}}
	if err := valid.Check(); err != nil {
		t.Error(err)
	}
	twice := NamespaceSpec{ImagePullSecrets: []LocalObjectReference{{Name: "ghcr"}, {Name: "ghcr"}, {Name: "Bad"}}}
	err := twice.Check()
	for _, want := range []string{"ghcr is named twice", `"Bad" is not a secret name`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
}

func TestOnlyNamespacesAndTheConfigurationLiveOutsideANamespace(t *testing.T) {
	if Namespaced(KindNamespace) || Namespaced(KindClusterConfiguration) || !Namespaced(KindVolume) ||
		!Namespaced(KindSecret) || !Namespaced(KindStack) {
		t.Error("wrong kinds in namespaces")
	}
}
