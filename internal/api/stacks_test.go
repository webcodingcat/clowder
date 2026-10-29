package api

import (
	"context"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/state"
)

const privilegedSource = "services: {exporter: {image: x, privileged: true}}\n"

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

func TestClusterYAMLThatWouldBreakAStackIsRefused(t *testing.T) {
	cluster := &fakeCluster{state: bootstrappedState(t), leader: true}
	cluster.state.Stacks = map[string]*state.Stack{"monitoring": {Name: "monitoring", Source: privilegedSource}}
	server := &Server{config: ServerConfig{Cluster: cluster}}
	err := server.checkStacksAgainst(context.Background(), clusterdir.Config{}.WithDefaults())
	if err == nil || !strings.Contains(err.Error(), "stack monitoring: service exporter: privileged") {
		t.Errorf("taking a running stack out of stacks.privileged: %v", err)
	}
}
