package api

import (
	"context"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

const privilegedSource = "name: monitoring\nservices: {exporter: {image: x, privileged: true}}\n"

func TestOnlyStacksTheClusterNamesMayBePrivileged(t *testing.T) {
	stack, err := compose.Parse(context.Background(), "monitoring", []byte(privilegedSource))
	if err != nil {
		t.Fatal(err)
	}
	config := clusterdir.Config{}.WithDefaults()
	if _, err := checkAgainstCluster(stack, config); err == nil ||
		!strings.Contains(err.Error(), "service exporter: privileged reaches beyond the container") {
		t.Errorf("a privileged stack the cluster does not name: %v", err)
	}
	config.Stacks.Privileged = []string{"monitoring"}
	if _, err := checkAgainstCluster(stack, config); err != nil {
		t.Errorf("a privileged stack the cluster names: %v", err)
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
		"larger than 65536 bytes": {"db-password": strings.Repeat("x", state.MaxSecretBytes+1)},
	} {
		if _, err := server.sealStackSecrets(stack, values); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
}

func TestClusterYAMLThatWouldBreakAStackIsRefused(t *testing.T) {
	cluster := &fakeCluster{state: bootstrappedState(t), leader: true}
	cluster.state.Stacks = map[string]*state.Stack{"monitoring": {Name: "monitoring", Source: privilegedSource}}
	server := &Server{config: ServerConfig{Cluster: cluster}}
	_, err := server.checkStacksAgainst(context.Background(), clusterdir.Config{}.WithDefaults())
	if err == nil || !strings.Contains(err.Error(), "stack monitoring: service exporter: privileged") {
		t.Errorf("taking a running stack out of stacks.privileged: %v", err)
	}
}
