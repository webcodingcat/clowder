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

func TestASecretTravelsEncrypted(t *testing.T) {
	if err := (SecretSpec{Encrypted: testArmor}).Check(); err != nil {
		t.Error(err)
	}
	for _, refused := range []string{"", "s3cret"} {
		if err := (SecretSpec{Encrypted: refused}).Check(); err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%q: %v", refused, err)
		}
	}
}

func TestOnlyNamespacesLiveOutsideANamespace(t *testing.T) {
	if Namespaced(KindNamespace) || !Namespaced(KindVolume) || !Namespaced(KindSecret) {
		t.Error("wrong kinds in namespaces")
	}
}
