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

func TestASecretHasAValueOfLimitedSize(t *testing.T) {
	if err := (SecretSpec{Value: "s3cret"}).Check(); err != nil {
		t.Error(err)
	}
	if err := (SecretSpec{}).Check(); err == nil {
		t.Error("an empty secret passed")
	}
	if err := (SecretSpec{Value: strings.Repeat("x", MaxSecretBytes+1)}).Check(); err == nil {
		t.Error("a secret too large passed")
	}
}

func TestOnlyNamespacesLiveOutsideANamespace(t *testing.T) {
	if Namespaced(KindNamespace) || !Namespaced(KindVolume) || !Namespaced(KindSecret) {
		t.Error("wrong kinds in namespaces")
	}
}
