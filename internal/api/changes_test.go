package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/state"
)

// testConfig is a configuration that names the Hetzner token in the Secret
// hetzner of names.System, key token.
func testConfig() clusterspec.Config {
	return clusterspec.Config{
		Hetzner:        clusterspec.Hetzner{TokenSecretRef: names.SecretKeyRef{Name: "hetzner", Key: "token"}},
		PrivateNetwork: "10.0.0.0/16",
		Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.11"},
		StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
	}
}

// configObject is the ClusterConfiguration of a config, as a user writes it.
func configObject(t *testing.T, config clusterspec.Config) resource.Object {
	t.Helper()
	spec, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return object(resource.KindClusterConfiguration, "", resource.ClusterConfigurationName, string(spec))
}

// configuration is a config with the Secret of its Hetzner token above it,
// as a file has them.
func (c *testCluster) configuration(token string, config clusterspec.Config) []resource.Object {
	c.t.Helper()
	return []resource.Object{
		c.secretObject(names.System, "hetzner", map[string]string{"token": token}),
		configObject(c.t, config),
	}
}

// dryRunWith runs objects as a dry run on the leader, whose agent asks
// Hetzner with servers. It calls the server directly, as the servers of
// the test cluster run already and their config is not to change.
func (c *testCluster) dryRunWith(servers func(ctx context.Context, api, token string) ([]int64, error),
	objects []resource.Object,
) ChangeResult {
	c.t.Helper()
	server := &Server{config: c.apis[0].config}
	server.config.HetznerServers = servers
	batch := changeBatch{DryRun: true, Actor: state.CLIActor("admin")}
	for _, object := range objects {
		put, err := server.putChange(object, false)
		batch.add(object.Ref(), state.ResourceChange{Put: &put}, err)
	}
	return server.runChanges(context.Background(), batch)
}

// alwaysLeader says it leads whatever its Raft member does.
type alwaysLeader struct{ *fakeCluster }

func (alwaysLeader) IsLeader() bool { return true }

// namespaceChange puts a namespace of the CLI's.
func namespaceChange(namespace string) *state.ResourceChange {
	return &state.ResourceChange{Put: &state.PutResource{Kind: resource.KindNamespace, Name: namespace, Owner: state.OwnerCLI}}
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

func TestASecretAndASealedSecretOfTheSameValuesAreTheSame(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	values := map[string]string{"password": "s3cret", "user": "shop"}
	for _, test := range []struct {
		secret resource.Object
		want   string
	}{
		{cluster.secretObject("default", "db", values), Applied},
		{cluster.sealedSecretObject("default", "db", values), Unchanged},
		{cluster.sealedSecretObject("default", "db", map[string]string{"password": "other", "user": "shop"}), Applied},
	} {
		result, err := client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{test.secret}}, false)
		if err != nil || result.Results[0].Outcome != test.want || result.Results[0].Kind != test.secret.Kind {
			t.Errorf("%s: %+v, %v", test.secret.Kind, result, err)
		}
	}
	// delete -f of the SealedSecret's file deletes the Secret
	sealed := cluster.sealedSecretObject("default", "db", values)
	result, err := client.DeleteResources(ctx, DeleteRequest{Resources: []resource.Ref{sealed.Ref()}}, false)
	if err != nil || result.Results[0].Outcome != Deleted {
		t.Errorf("delete of the SealedSecret: %+v, %v", result, err)
	}
}

func TestAnApplyRefusesASecretNamedTwice(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	values := map[string]string{"password": "s3cret"}
	request := ApplyRequest{Resources: []resource.Object{
		cluster.secretObject(names.System, "alerts", values), cluster.sealedSecretObject(names.System, "alerts", values),
	}}
	result, err := client.ApplyResources(context.Background(), request, false)
	if err != nil || result.Results[0].Outcome != NotApplied || result.Results[1].Outcome != Refused ||
		!strings.Contains(result.Results[1].Message, "the request names Secret clowder-system/alerts twice (first as a Secret)") {
		t.Errorf("a Secret and a SealedSecret of one name: %+v, %v", result, err)
	}
}

func TestACreateRefusesASecretThatExists(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	request := ApplyRequest{Resources: []resource.Object{cluster.secretObject("default", "db", map[string]string{"a": "b"})}, Create: true}
	if result, err := client.ApplyResources(context.Background(), request, false); err != nil || result.Results[0].Outcome != Applied {
		t.Fatalf("the first create: %+v, %v", result, err)
	}
	result, err := client.ApplyResources(context.Background(), request, false)
	if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, "already exists") {
		t.Errorf("the second create: %+v, %v", result, err)
	}
}

func TestTheConfigurationGoesInWithTheSecretItNames(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	var shown resource.Object
	path := "/v1/resources/clusterconfiguration/cluster"
	if err := client.Get(ctx, path, &shown); statusOf(err) != http.StatusNotFound {
		t.Errorf("the configuration before the first apply: %v", err)
	}
	result, err := client.ApplyResources(ctx, ApplyRequest{Resources: cluster.configuration("the token", testConfig())}, false)
	if err != nil || !slices.Equal(outcomes(result), []string{Applied, Applied}) || result.Index == 0 {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	var token state.Encrypted
	cluster.clusters[0].Read(func(current *state.State) { token, err = current.HetznerToken() })
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := cluster.apis[0].config.Keys.Decrypt(token); err != nil || string(opened) != "the token" {
		t.Errorf("token %q, %v", opened, err)
	}
	if err := client.Get(ctx, path, &shown); err != nil {
		t.Fatal(err)
	}
	spec, err := resource.DecodeClusterConfiguration(shown.Spec)
	if err != nil || shown.Metadata.Generation != 1 || shown.Metadata.Owner != state.OwnerCLI ||
		spec.Hetzner.TokenSecretRef.Name != "hetzner" || spec.Ingress.FloatingIP != "203.0.113.11" {
		t.Errorf("configuration %+v (%s), %v", shown, shown.Spec, err)
	}
	var status Status
	if err := client.Get(ctx, "/v1/status", &status); err != nil || status.Generation != 1 {
		t.Errorf("status %+v, %v", status, err)
	}
	// the answer of a read applies again as it is
	if again, err := client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{shown}}, false); err != nil ||
		!slices.Equal(outcomes(again), []string{Unchanged}) {
		t.Errorf("the shown configuration applied again: %+v, %v", again, err)
	}
}

func TestAConfigurationWithoutItsSecretIsRefusedAndNoneGoes(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	alone := ApplyRequest{Resources: []resource.Object{configObject(t, testConfig())}}
	result, err := client.ApplyResources(ctx, alone, false)
	if err != nil || result.Results[0].Outcome != Refused ||
		!strings.Contains(result.Results[0].Message, "hetzner.tokenSecretRef: clowder-system has no Secret hetzner") {
		t.Errorf("a configuration without its Secret: %+v, %v", result, err)
	}
	otherName := configObject(t, testConfig())
	otherName.Metadata.Name = "prod"
	result, err = client.ApplyResources(ctx, ApplyRequest{Resources: []resource.Object{otherName}}, false)
	if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, "named cluster") {
		t.Errorf("a configuration named prod: %+v, %v", result, err)
	}
	if _, err := client.ApplyResources(ctx, ApplyRequest{Resources: cluster.configuration("the token", testConfig())}, false); err != nil {
		t.Fatal(err)
	}
	result, err = client.DeleteResources(ctx, DeleteRequest{Resources: []resource.Ref{
		{Kind: resource.KindClusterConfiguration, Name: resource.ClusterConfigurationName},
	}}, false)
	if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, "cannot be deleted") {
		t.Errorf("delete of the configuration: %+v, %v", result, err)
	}
	// nor does the Secret it names go
	result, err = client.DeleteResources(ctx, DeleteRequest{Resources: []resource.Ref{
		{Kind: resource.KindSecret, Namespace: names.System, Name: "hetzner"},
	}}, false)
	if err != nil || result.Results[0].Outcome != Refused || !strings.Contains(result.Results[0].Message, "named by") {
		t.Errorf("delete of the Hetzner token's Secret: %+v, %v", result, err)
	}
}

func TestADryRunTellsWhatIsNewAndWhatDiffersButNoValue(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	ctx := context.Background()
	files := func(config clusterspec.Config, db map[string]string, services string) []resource.Object {
		return append(cluster.configuration("the token", config),
			object(resource.KindNamespace, "", "shop", ""),
			cluster.secretObject("shop", "db", db),
			object(resource.KindStack, "shop", "web", `{"services": {`+services+`}}`))
	}
	app := `"app": {"image": "example/app"}`
	first := files(testConfig(), map[string]string{"password": "s3cret"}, app)
	result, err := client.ApplyResources(ctx, ApplyRequest{Resources: first}, true)
	if err != nil || result.Index != 0 || !slices.Equal(outcomes(result), []string{Applied, Applied, Applied, Applied, Applied}) {
		t.Fatalf("dry run before the first apply: %+v, %v", result, err)
	}
	for _, each := range result.Results {
		if !each.Created || len(each.Differs) > 0 {
			t.Errorf("%s %s is not new: %+v", each.Kind, each.Name, each)
		}
	}
	if _, err := client.ApplyResources(ctx, ApplyRequest{Resources: first}, false); err != nil {
		t.Fatal(err)
	}
	// encrypted anew, the same values are no change
	again, err := client.ApplyResources(ctx, ApplyRequest{Resources: files(testConfig(),
		map[string]string{"password": "s3cret"}, app)}, true)
	if err != nil || !slices.Equal(outcomes(again), []string{Unchanged, Unchanged, Unchanged, Unchanged, Unchanged}) {
		t.Errorf("the same files: %+v, %v", again, err)
	}
	moved := testConfig()
	moved.Ingress.FloatingIP = "203.0.113.12"
	for _, test := range []struct {
		name     string
		db       map[string]string
		services string
		want     map[string][]string // by the resource's name
	}{
		{
			"a changed value",
			map[string]string{"password": "other"},
			app,
			map[string][]string{"db": {"data.password"}, "cluster": {"spec.ingress"}},
		},
		{
			"a key new and one gone",
			map[string]string{"user": "s3cret"},
			app,
			map[string][]string{"db": {"data.password (gone)", "data.user (new)"}, "cluster": {"spec.ingress"}},
		},
		{
			"a service more",
			map[string]string{"password": "s3cret"},
			app + `, "db": {"image": "example/db"}`,
			map[string][]string{"web": {"spec.services"}, "cluster": {"spec.ingress"}},
		},
	} {
		result, err := client.ApplyResources(ctx, ApplyRequest{Resources: files(moved, test.db, test.services)}, true)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		for _, each := range result.Results {
			want, changed := test.want[each.Name]
			switch {
			case changed && (each.Outcome != Applied || each.Created || !slices.Equal(each.Differs, want)):
				t.Errorf("%s: %s %s: %+v, want differs %v", test.name, each.Kind, each.Name, each, want)
			case !changed && each.Outcome != Unchanged:
				t.Errorf("%s: %s %s: %+v, want unchanged", test.name, each.Kind, each.Name, each)
			}
		}
		if encoded, _ := json.Marshal(result); strings.Contains(string(encoded), "s3cret") ||
			strings.Contains(string(encoded), "other") {
			t.Errorf("%s: the answer shows a value: %s", test.name, encoded)
		}
	}
	var index uint64
	cluster.clusters[0].Read(func(current *state.State) {
		index = current.Index
		if current.Config.Spec.Ingress.FloatingIP != "203.0.113.11" {
			t.Error("a dry run changed the configuration")
		}
	})
	if _, err := client.ApplyResources(ctx, ApplyRequest{Resources: files(moved, map[string]string{"password": "x"}, app)}, true); err != nil {
		t.Fatal(err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		if current.Index != index {
			t.Error("a dry run committed")
		}
	})
}

func TestTheConfigurationWarnsOfItsServiceURLs(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	config := testConfig()
	config.Hetzner.API = "http://hetzner.lab/v1"
	for _, dryRun := range []bool{true, false} {
		result, err := client.ApplyResources(context.Background(),
			ApplyRequest{Resources: cluster.configuration("the token", config)}, dryRun)
		if err != nil || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "hetzner.api is set") ||
			!strings.Contains(result.Warnings[0], "the Hetzner token crosses the network unencrypted") {
			t.Errorf("dry run %t: %+v, %v", dryRun, result, err)
		}
	}
	// a request without the configuration warns of none
	result, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", ""),
	}}, false)
	if err != nil || len(result.Warnings) > 0 {
		t.Errorf("an apply of a namespace: %+v, %v", result, err)
	}
}

func TestADryRunTellsWhatANewHetznerTokenSees(t *testing.T) {
	cluster := newTestCluster(t)
	// a third host; hosts 0, 1 and 2 have the servers 40, 41 and 42
	cluster.clusters[0].mu.Lock()
	cluster.clusters[0].state.Hosts[2] = &state.Host{
		ID: 2, Name: "node2", ServerID: 42, Underlay: netip.MustParseAddr("10.0.0.4"), Phase: state.Active,
	}
	cluster.clusters[0].mu.Unlock()
	config := testConfig()
	config.Hetzner.API = "https://hetzner.lab/v1"
	for _, test := range []struct {
		name    string
		servers func(ctx context.Context, api, token string) ([]int64, error)
		want    TokenCheck
	}{
		{"two of three", func(_ context.Context, api, token string) ([]int64, error) {
			if api != "https://hetzner.lab/v1" || token != "the token" {
				return nil, errors.New("asked " + api + " with another token")
			}
			return []int64{40, 42, 99}, nil
		}, TokenCheck{Hosts: 3, Seen: 2}},
		{"none", func(context.Context, string, string) ([]int64, error) {
			return []int64{99}, nil
		}, TokenCheck{Hosts: 3}},
		{"an error", func(context.Context, string, string) ([]int64, error) {
			return nil, errors.New("unauthorized")
		}, TokenCheck{Hosts: 3, Error: "unauthorized"}},
		{"no client", nil, TokenCheck{Hosts: 3, Error: "this agent has no client of Hetzner's API"}},
	} {
		result := cluster.dryRunWith(test.servers, cluster.configuration("the token", config))
		if result.HetznerToken == nil || *result.HetznerToken != test.want {
			t.Errorf("%s: %+v, want %+v", test.name, result.HetznerToken, test.want)
		}
	}
}

func TestADryRunThatKeepsTheHetznerTokenAsksNothing(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	if _, err := client.ApplyResources(context.Background(),
		ApplyRequest{Resources: cluster.configuration("the token", testConfig())}, false); err != nil {
		t.Fatal(err)
	}
	asked := 0
	servers := func(context.Context, string, string) ([]int64, error) {
		asked++
		return []int64{40, 41}, nil
	}
	moved := testConfig()
	moved.Ingress.FloatingIP = "203.0.113.12"
	// another API is no new token: a test lab's cluster never moves to Hetzner's
	moved.Hetzner.API = "https://hetzner.lab/v1"
	// the same token, encrypted anew, in a changed configuration
	if result := cluster.dryRunWith(servers, cluster.configuration("the token", moved)); result.HetznerToken != nil || asked != 0 {
		t.Errorf("the same token: %+v, asked %d times", result.HetznerToken, asked)
	}
	// a new token in the Secret alone
	result := cluster.dryRunWith(servers, cluster.configuration("a new token", testConfig())[:1])
	if result.HetznerToken == nil || *result.HetznerToken != (TokenCheck{Hosts: 2, Seen: 2}) || asked != 1 {
		t.Errorf("a new token: %+v, asked %d times", result.HetznerToken, asked)
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

func TestABatchRunsOnlyOnTheLeader(t *testing.T) {
	cluster := newTestCluster(t)
	// host 1 still takes host 0 for the leader, which lost its lead
	cluster.clusters[0].leader = false
	client := cluster.client(cluster.cliCertificate(), testVersion, 1)
	_, err := client.ApplyResources(context.Background(), ApplyRequest{Resources: []resource.Object{
		object(resource.KindNamespace, "", "shop", ""),
	}}, false)
	if statusOf(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "not the leader; try again") {
		t.Errorf("a batch forwarded to a former leader: %v", err)
	}
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
	server := &Server{config: ServerConfig{HostConfig: HostConfig{Cluster: alwaysLeader{cluster.clusters[0]}}}}
	result := server.runChanges(context.Background(), changeBatch{Actor: state.CLIActor("admin"), Changes: []batchChange{
		{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "shop"}, Change: namespaceChange("shop")},
		{Ref: resource.Ref{Kind: resource.KindNamespace, Name: "blog"}, Change: namespaceChange("blog")},
	}})
	if want := []string{Failed, Failed}; !slices.Equal(outcomes(result), want) {
		t.Errorf("results %+v, want %v", result, want)
	}
}
