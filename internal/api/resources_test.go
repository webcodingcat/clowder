package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// object is a resource as a user writes it.
func object(kind, namespace, name, spec string) resource.Object {
	written := resource.Object{
		APIVersion: resource.APIVersion, Kind: kind,
		Metadata: resource.Metadata{Namespace: namespace, Name: name},
	}
	if spec != "" {
		written.Spec = json.RawMessage(spec)
	}
	return written
}

// secretObject is a Secret as the CLI sends it: each value by its key, in
// clear in stringData.
func (c *testCluster) secretObject(namespace, name string, data map[string]string) resource.Object {
	c.t.Helper()
	encoded, err := json.Marshal(resource.SecretSpec{StringData: data})
	if err != nil {
		c.t.Fatal(err)
	}
	return object(resource.KindSecret, namespace, name, string(encoded))
}

// sealedSecretObject is a SealedSecret as clowder secret seal writes it:
// each value by its key, sealed for the cluster and for this Secret.
func (c *testCluster) sealedSecretObject(namespace, name string, data map[string]string) resource.Object {
	c.t.Helper()
	sealed := map[string]string{}
	for key, value := range data {
		sealed[key] = seal(c.t, c.sealing, names.ID(namespace, name), value)
	}
	return sealedSecretWithData(c.t, namespace, name, sealed)
}

// sealedSecretWithData is a SealedSecret of values sealed by hand, for the
// tests of values sealed for another Secret.
func sealedSecretWithData(t *testing.T, namespace, name string, sealed map[string]string) resource.Object {
	t.Helper()
	encoded, err := json.Marshal(resource.SealedSecretSpec{EncryptedData: sealed})
	if err != nil {
		t.Fatal(err)
	}
	return object(resource.KindSealedSecret, namespace, name, string(encoded))
}

// withStorageClass gives every agent's state the storage class fast.
func withStorageClass(cluster *testCluster) {
	for _, each := range cluster.clusters {
		each.mu.Lock()
		each.state.Config.Spec.StorageClasses = map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}}
		each.mu.Unlock()
	}
}

func outcomes(result ChangeResult) []string {
	var each []string
	for _, one := range result.Results {
		each = append(each, one.Outcome)
	}
	return each
}

func TestResourcesReadBackAsTheyApplyAndSecretsWithoutTheirValue(t *testing.T) {
	cluster := newTestCluster(t)
	withStorageClass(cluster)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	result, err := client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", `{"imagePullSecrets": [{"name": "ghcr"}]}`),
		cluster.secretObject("shop", "ghcr", map[string]string{"server": "ghcr.io", "username": "robot", "password": "s3cret"}),
		object(resource.KindVolume, "shop", "data", `{"class": "fast", "size": "1024M", "replicas": 1}`),
		cluster.secretObject("default", "ghcr", map[string]string{"server": "ghcr.io", "username": "robot", "password": "s3cret"}),
	}}, false)
	if err != nil || !slices.Equal(outcomes(result), []string{Applied, Applied, Applied, Applied}) {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	var volumes ResourceList
	if err := client.Get(ctx, "/v1/resources/volume?namespace=shop", &volumes); err != nil {
		t.Fatal(err)
	}
	if len(volumes.Items) != 1 || volumes.Index == 0 || string(volumes.Items[0].Spec) != `{"class":"fast","size":"1G","replicas":1}` {
		t.Fatalf("volumes %+v", volumes)
	}
	// what a read answers applies again as it is (a secret's value never
	// leaves the cluster, so a secret is the exception)
	result, err = client.ApplyResources(ctx, ApplyRequest{Resources: volumes.Items}, false)
	if err != nil || result.Results[0].Outcome != Unchanged {
		t.Errorf("the answer applied again: %+v, %v", result, err)
	}
	var secrets ResourceList
	if err := client.Get(ctx, "/v1/resources/secret?allNamespaces=true", &secrets); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 2 || secrets.Items[0].Spec != nil || strings.Contains(string(secrets.Items[0].Status), "s3cret") {
		t.Fatalf("secrets %+v", secrets)
	}
	var status SecretStatus
	if err := json.Unmarshal(secrets.Items[0].Status, &status); err != nil ||
		!slices.Equal(status.Keys, []string{"password", "server", "username"}) || status.Digest == "" {
		t.Errorf("secret status %s, %v", secrets.Items[0].Status, err)
	}
	// the same values in another secret have another digest
	if string(secrets.Items[0].Status) == string(secrets.Items[1].Status) {
		t.Error("two secrets of the same values share their digest")
	}
	var namespace resource.Object
	if err := client.Get(ctx, "/v1/resources/namespace/shop", &namespace); err != nil || namespace.Metadata.Owner != state.OwnerCLI ||
		string(namespace.Spec) != `{"imagePullSecrets":[{"name":"ghcr"}]}` {
		t.Errorf("namespace %+v (%s), %v", namespace, namespace.Spec, err)
	}
	var system resource.Object
	if err := client.Get(ctx, "/v1/resources/namespace/clowder-system", &system); err != nil || system.Spec != nil {
		t.Errorf("namespace clowder-system %+v, %v", system, err)
	}
	for path, status := range map[string]int{
		"/v1/resources/volume":                       http.StatusBadRequest, // no namespace named
		"/v1/resources/namespace?namespace=x":        http.StatusBadRequest,
		"/v1/resources/pod?namespace=shop":           http.StatusNotFound,
		"/v1/resources/volume/shop/nothing":          http.StatusNotFound,
		"/v1/resources/namespace/shop/nothing":       http.StatusNotFound,
		"/v1/resources/namespace/nothing":            http.StatusNotFound,
		"/v1/resources/volume/data":                  http.StatusNotFound, // a volume lives in a namespace
		"/v1/resources/clusterconfiguration/cluster": http.StatusNotFound, // none applied yet
	} {
		var answer json.RawMessage
		if err := client.Get(ctx, path, &answer); statusOf(err) != status {
			t.Errorf("%s: want %d, got %v", path, status, err)
		}
	}
}

func TestAStackShowsItsStatusAndIsAskedForOnItsHost(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	result, err := client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", ""),
		object(resource.KindStack, "shop", "web", `{"services": {"app": {"image": "example/app"}}}`),
	}}, false)
	if err != nil || !slices.Equal(outcomes(result), []string{Applied, Applied}) {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	var stack resource.Object
	if err := client.Get(ctx, "/v1/resources/stack/shop/web", &stack); err != nil {
		t.Fatal(err)
	}
	var status StackStatus
	// no priority written: the configuration's default, normal
	if err := json.Unmarshal(stack.Status, &status); err != nil || status.Status != string(state.StackPending) ||
		status.Priority != 500 || !status.Default || status.Host != "" {
		t.Errorf("status %s, %v", stack.Status, err)
	}
	for path, want := range map[string]int{
		"/v1/stacks/shop/web/ps":      http.StatusConflict, // not placed yet
		"/v1/stacks/shop/web/logs":    http.StatusConflict,
		"/v1/stacks/default/web/ps":   http.StatusNotFound, // another namespace's
		"/v1/stacks/shop/nothing/ps":  http.StatusNotFound,
		"/v1/stacks/shop/Not_Web/ps":  http.StatusNotFound,
		"/v1/stacks/shop/web/unknown": http.StatusNotFound,
	} {
		if err := client.Get(ctx, path, &[]ContainerInfo{}); statusOf(err) != want {
			t.Errorf("%s: want HTTP %d, got %v", path, want, err)
		}
	}
	// placed on host 1, the stack is asked for there: these test agents run
	// no containers, which only host 1 itself answers
	host := state.HostID(1)
	leader := cluster.clusters[0]
	leader.mu.Lock()
	leader.state.Stacks["shop/web"].Host = &host
	leader.mu.Unlock()
	if err := client.Get(ctx, "/v1/stacks/shop/web/ps", &[]ContainerInfo{}); statusOf(err) != http.StatusServiceUnavailable ||
		!strings.Contains(err.Error(), "runs no containers") {
		t.Errorf("ps of a placed stack: %v", err)
	}
}

func TestARestartGoesThroughAFollowerAndShowsInTheStatus(t *testing.T) {
	cluster := newTestCluster(t)
	leader := cluster.client(cluster.cliCertificate(), testVersion, 0)
	follower := cluster.client(cluster.cliCertificate(), testVersion, 1)
	ctx := context.Background()
	if _, err := leader.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{
		object(resource.KindStack, "default", "web", `{"services": {"db": {"image": "x"}, "app": {"image": "x", "dependsOn": ["db"]}}}`),
	}}, false); err != nil {
		t.Fatal(err)
	}
	var written Written
	if err := follower.Act(ctx, "/v1/stacks/default/web/restart", RestartRequest{Services: []string{"db"}}, &written); err != nil ||
		written.Index == 0 {
		t.Fatalf("restart: %+v, %v", written, err)
	}
	var stack resource.Object
	var status StackStatus
	if err := leader.Get(ctx, "/v1/resources/stack/default/web", &stack); err != nil || json.Unmarshal(stack.Status, &status) != nil {
		t.Fatalf("stack %+v, %v", stack, err)
	}
	if len(status.Restarted) != 2 || status.Restarted["app"].IsZero() || stack.Metadata.Generation != 1 {
		t.Errorf("restarted %v, generation %d; want db and app, generation 1", status.Restarted, stack.Metadata.Generation)
	}
	for request, want := range map[string]int{
		`{"services": ["mail"]}`: http.StatusConflict,
		`{"servics": ["db"]}`:    http.StatusBadRequest,
	} {
		err := leader.Act(ctx, "/v1/stacks/default/web/restart", json.RawMessage(request), &written)
		if statusOf(err) != want {
			t.Errorf("%s: want HTTP %d, got %v", request, want, err)
		}
	}
	if err := leader.Act(ctx, "/v1/stacks/default/blog/restart", RestartRequest{}, &written); statusOf(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "no stack default/blog") {
		t.Errorf("restart of no stack: %v", err)
	}
}
