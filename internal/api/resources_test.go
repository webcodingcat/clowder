package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

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

func TestAnApplyGoesInWholeOrNotAtAll(t *testing.T) {
	cluster := newTestCluster(t)
	withStorageClass(cluster)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	shop := ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", ""),
		object(resource.KindVolume, "shop", "data", `{"class": "fast", "size": "1024M", "replicas": 1}`),
		cluster.secretObject("shop", "token", map[string]string{"value": "s3cret"}),
	}}
	refused := shop
	refused.Resources = append(slices.Clone(shop.Resources),
		object(resource.KindVolume, "shop", "logs", `{"class": "slow", "size": "1G", "replicas": 1}`),
		cluster.secretObject("shop", "later", map[string]string{"value": "x"}))
	var before uint64
	cluster.clusters[0].Read(func(current *state.State) { before = current.Index })
	result, err := client.ApplyResources(ctx, refused, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{NotApplied, NotApplied, NotApplied, Refused, NotApplied}
	if !slices.Equal(outcomes(result), want) || result.Index != 0 ||
		!strings.Contains(result.Results[3].Message, `no storage class "slow"`) {
		t.Fatalf("results %+v, want %v", result, want)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if _, ok := current.Namespaces["shop"]; ok || current.Index != before {
			t.Error("a refused apply committed")
		}
	})
	applied, err := client.ApplyResources(ctx, shop, false)
	if err != nil || !slices.Equal(outcomes(applied), []string{Applied, Applied, Applied}) || applied.Index == 0 {
		t.Fatalf("apply: %+v, %v", applied, err)
	}
	// the same files again change nothing and commit nothing
	again, err := client.ApplyResources(ctx, shop, false)
	if err != nil || !slices.Equal(outcomes(again), []string{Unchanged, Unchanged, Unchanged}) || again.Index != 0 {
		t.Errorf("again: %+v, %v", again, err)
	}
}

func TestADryRunChecksEverythingAndWritesNothing(t *testing.T) {
	cluster := newTestCluster(t)
	withStorageClass(cluster)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	var before uint64
	cluster.clusters[0].Read(func(current *state.State) { before = current.Index })
	// the volume's namespace comes with the same request
	result, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "blog", ""),
		object(resource.KindVolume, "blog", "data", `{"class": "fast", "size": "1G", "replicas": 0}`),
	}}, true)
	if err != nil || !slices.Equal(outcomes(result), []string{Applied, Applied}) || result.Index != 0 {
		t.Errorf("dry run: %+v, %v", result, err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if _, ok := current.Namespaces["blog"]; ok || current.Index != before {
			t.Error("the dry run wrote to the state")
		}
	})
}

func TestASpecIsReadStrictly(t *testing.T) {
	cluster := newTestCluster(t)
	withStorageClass(cluster)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	for spec, want := range map[string]string{
		`{"class": "fast", "size": "1G"}`:                              "replicas is required",
		`{"class": "fast", "size": "1G", "replicas": 1, "fast": true}`: "unknown field",
		`{"class": "fast", "size": "1.5G", "replicas": 1}`:             "not a positive whole number",
	} {
		result, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{
			object(resource.KindVolume, "default", "data", spec),
		}}, false)
		if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, want) {
			t.Errorf("%s: %+v, %v", spec, result, err)
		}
	}
	wrongVersion := object(resource.KindNamespace, "", "shop", "")
	wrongVersion.APIVersion = "v1"
	result, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{wrongVersion}}, false)
	if err != nil || !strings.Contains(result.Results[0].Message, `apiVersion "v1"`) {
		t.Errorf("another apiVersion: %+v, %v", result, err)
	}
	// a typo in the request is no empty request
	err = client.Act(context.Background(), "/v1/apply", map[string]any{"resorces": []any{}}, &result)
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("a request with a typo: %v", err)
	}
}

func TestASealedSecretTakesOnlyAValueSealedForIt(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, value, want string
	}{
		// the Hetzner token, copied
		{
			"the Hetzner token", seal(t, cluster.sealing, "clowder-system/hetzner", "s3cret"),
			"the value is sealed for clowder-system/hetzner, not for default/token",
		},
		{
			"another Secret", seal(t, cluster.sealing, "shop/token", "s3cret"),
			"the value is sealed for shop/token, not for default/token",
		},
		{
			"another cluster", seal(t, stranger.Recipient(), "default/token", "s3cret"),
			"another cluster",
		},
	} {
		secret := sealedSecretWithData(t, "default", "token", map[string]string{"value": test.value})
		result, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{secret}}, false)
		if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, test.want) ||
			strings.Contains(result.Results[0].Message, "s3cret") {
			t.Errorf("a value of %s: %+v, %v", test.name, result, err)
		}
	}
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

func TestTheLeaderDecidesNotAFollowerThatIsBehind(t *testing.T) {
	cluster := newTestCluster(t)
	// host 1 still has a namespace shop that the leader has not: it would
	// answer unchanged and propose nothing
	cluster.clusters[1].mu.Lock()
	cluster.clusters[1].state.Namespaces["shop"] = &state.Namespace{Meta: state.Meta{Name: "shop", Owner: state.OwnerCLI}}
	cluster.clusters[1].mu.Unlock()
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	ctx := context.Background()
	shop := ApplyRequest{Resources: []resource.Object{object(resource.KindNamespace, "", "shop", "")}}
	result, err := client.ApplyResources(ctx, shop, false)
	if err != nil || result.Results[0].Outcome != Applied || result.Index == 0 {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if _, ok := current.Namespaces["shop"]; !ok {
			t.Error("the leader has no namespace shop")
		}
	})
	// a volume goes only with its data said so, and then the whole request
	// is refused before anything goes
	err = client.Act(ctx, "/v1/delete", DeleteRequest{Resources: []resource.Ref{
		{Kind: resource.KindNamespace, Name: "shop"}, {Kind: resource.KindVolume, Namespace: "default", Name: "data"},
	}}, &result)
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "deleting volume default/data deletes its data") {
		t.Errorf("a volume without with_data: %v", err)
	}
	result, err = client.DeleteResources(ctx, DeleteRequest{Resources: []resource.Ref{
		{Kind: resource.KindNamespace, Name: "shop"},
		{Kind: resource.KindNamespace, Name: "default"},
		{Kind: resource.KindNamespace, Name: "blog"},
	}}, false)
	if want := []string{NotApplied, Refused, NotApplied}; err != nil || !slices.Equal(outcomes(result), want) ||
		!strings.Contains(result.Results[1].Message, "namespace default belongs to the cluster") {
		t.Errorf("delete: %+v, %v; want %v", result, err, want)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if _, ok := current.Namespaces["shop"]; !ok {
			t.Error("a refused delete took namespace shop")
		}
	})
}

func TestABatchCarriesOnlyTheCLIsChanges(t *testing.T) {
	cluster := newTestCluster(t)
	host1 := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	for _, actor := range []string{state.LeaderActor, "host/1", ""} {
		var result ChangeResult
		err := host1.Act(context.Background(), "/v1/internal/changes", changeBatch{Actor: actor, Changes: []batchChange{
			{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "shop"}, Change: namespaceChange("shop")},
		}}, &result)
		if statusOf(err) != http.StatusForbidden {
			t.Errorf("a batch of actor %q: %v", actor, err)
		}
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if _, ok := current.Namespaces["shop"]; ok {
			t.Error("a forbidden batch went in")
		}
	})
}

func TestTheLeaderTakesABatchWithFieldsOfTheNextRelease(t *testing.T) {
	cluster := newTestCluster(t)
	host1 := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	encoded, err := json.Marshal(changeBatch{Actor: state.CLIActor("admin"), Changes: []batchChange{
		{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "shop"}, Change: namespaceChange("shop")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var batch map[string]any
	if err := json.Unmarshal(encoded, &batch); err != nil {
		t.Fatal(err)
	}
	batch["from_the_next_release"] = true
	var result ChangeResult
	if err := host1.Act(context.Background(), "/v1/internal/changes", batch, &result); err != nil ||
		!slices.Equal(outcomes(result), []string{Applied}) {
		t.Errorf("a batch with an unknown field: %+v, %v", result, err)
	}
}

func TestAChangeThatDidNotCommitIsAFailureNotARefusal(t *testing.T) {
	cluster := newTestCluster(t)
	// the server checked that it leads, then Raft lost the lead: nothing
	// commits, though the state would have taken the command
	cluster.clusters[0].leader = false
	server := &Server{config: ServerConfig{Cluster: alwaysLeader{cluster.clusters[0]}}}
	result := server.runChanges(context.Background(), changeBatch{Actor: state.CLIActor("admin"), Changes: []batchChange{
		{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "shop"}, Change: namespaceChange("shop")},
		{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "blog"}, Change: namespaceChange("blog")},
	}})
	if want := []string{Failed, Failed}; !slices.Equal(outcomes(result), want) {
		t.Errorf("results %+v, want %v", result, want)
	}
}

// alwaysLeader says it leads whatever its Raft member does.
type alwaysLeader struct{ *fakeCluster }

func (alwaysLeader) IsLeader() bool { return true }

// namespaceChange puts a namespace of the CLI's.
func namespaceChange(namespace string) *state.ResourceChange {
	return &state.ResourceChange{Put: &state.PutResource{Kind: resource.KindNamespace, Name: namespace, Owner: state.OwnerCLI}}
}
