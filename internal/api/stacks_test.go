package api

import (
	"context"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

const privilegedSource = "name: monitoring\nservices: {exporter: {image: x, privileged: true}}\n"

func TestOnlyStacksTheClusterNamesMayBePrivileged(t *testing.T) {
	stack, err := compose.Parse(context.Background(), "monitoring", []byte(privilegedSource))
	if err != nil {
		t.Fatal(err)
	}
	config := clusterdir.Config{}.WithDefaults()
	if _, err := checkAgainstCluster("default", stack, config); err == nil ||
		!strings.Contains(err.Error(), "service exporter: privileged reaches beyond the container") {
		t.Errorf("a privileged stack the cluster does not name: %v", err)
	}
	config.Stacks.Privileged = []string{"other/monitoring"}
	if _, err := checkAgainstCluster("default", stack, config); err == nil {
		t.Error("a privileged stack of another namespace passed")
	}
	config.Stacks.Privileged = []string{"default/monitoring"}
	if _, err := checkAgainstCluster("default", stack, config); err != nil {
		t.Errorf("a privileged stack the cluster names: %v", err)
	}
}

func TestAnExistingVolumeCannotChange(t *testing.T) {
	stack, err := compose.Parse(context.Background(), "shop", []byte(
		"name: shop\nservices: {db: {image: x, volumes: [\"data:/data\"]}}\n"+
			"volumes: {data: {x-clowder: {class: fast, size: 2G, replicas: 1}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkVolumesUnchanged("default", stack, map[string]*state.Volume{}); err != nil {
		t.Errorf("a new volume: %v", err)
	}
	volumes := map[string]*state.Volume{
		"default/shop/data": {ID: "default/shop/data", Spec: resource.VolumeSpec{Class: "fast", Size: 2 << 30, Replicas: 1}},
	}
	if err := checkVolumesUnchanged("default", stack, volumes); err != nil {
		t.Errorf("the same volume: %v", err)
	}
	volumes["default/shop/data"].Spec.Size = 1 << 30
	if err := checkVolumesUnchanged("other", stack, volumes); err != nil {
		t.Errorf("the volume of another namespace's stack counted: %v", err)
	}
	if err := checkVolumesUnchanged("default", stack, volumes); err == nil ||
		!strings.Contains(err.Error(), "volume data exists with class fast, size 1G and 1 replicas") {
		t.Errorf("a grown volume: %v", err)
	}
}

// clearKeys "seals" a value as it is.
type clearKeys struct{}

func (clearKeys) Seal(plaintext []byte) (state.Sealed, error) { return state.Sealed(plaintext), nil }
func (clearKeys) Open(sealed state.Sealed) ([]byte, error)    { return []byte(sealed), nil }

func TestExactlyTheSecretsTheStackAsksForAreSealed(t *testing.T) {
	stack, err := compose.Parse(context.Background(), "shop", []byte(
		"name: shop\nservices: {app: {image: x, x-clowder: {secrets: [db-password]}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{config: ServerConfig{Keys: clearKeys{}}}
	if sealed, err := server.sealStackSecrets(stack, map[string]string{"db-password": "pw"}); err != nil ||
		string(sealed["db-password"]) != "pw" {
		t.Errorf("sealed %v, %v", sealed, err)
	}
	for want, values := range map[string]map[string]string{
		"asks for the secrets":    {},
		"but [db-password other]": {"db-password": "pw", "other": "x"},
		"is empty":                {"db-password": ""},
		"larger than 65536 bytes": {"db-password": strings.Repeat("x", resource.MaxSecretBytes+1)},
	} {
		if _, err := server.sealStackSecrets(stack, values); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}

func TestClusterYAMLThatWouldBreakAStackIsRefused(t *testing.T) {
	cluster := &fakeCluster{state: bootstrappedState(t), leader: true}
	cluster.state.Stacks = map[string]*state.Stack{
		"default/monitoring": {Namespace: "default", Name: "monitoring", Source: privilegedSource},
	}
	server := &Server{config: ServerConfig{Cluster: cluster}}
	_, err := server.checkStacksAgainst(context.Background(), clusterdir.Config{}.WithDefaults())
	if err == nil || !strings.Contains(err.Error(), "stack default/monitoring: service exporter: privileged") {
		t.Errorf("taking a running stack out of stacks.privileged: %v", err)
	}
}
