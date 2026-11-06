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
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/state"
)

// encrypt encrypts a value for a Secret, as clowder secret encrypt does.
func encrypt(t *testing.T, recipients []age.Recipient, secretID, value string) string {
	t.Helper()
	armored, err := secretvalue.Encrypt(secretvalue.Value{For: secretID, Bytes: []byte(value)}, recipients...)
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

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
		put, err := server.putChange(object)
		batch.add(object.Ref(), state.ResourceChange{Put: &put}, err)
	}
	return server.runChanges(context.Background(), batch)
}

func TestTheEncryptionNamesTheClustersKeyAndTheRecoveryKey(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	var encryption Encryption
	if err := client.Get(context.Background(), "/v1/encryption", &encryption); err != nil ||
		encryption.Cluster != cluster.recipients[0].(*age.X25519Recipient).String() ||
		encryption.Recovery != cluster.recipients[1].(*age.X25519Recipient).String() {
		t.Errorf("encryption %+v, %v", encryption, err)
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
	var token state.Sealed
	cluster.clusters[0].Read(func(current *state.State) { token, err = current.HetznerToken() })
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := cluster.apis[0].config.Keys.Open(token); err != nil || string(opened) != "the token" {
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

func TestRekeyKeepsEachValueAndItsPlace(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	values := []PlacedValue{
		{For: "shop/token", Encrypted: encrypt(t, cluster.recipients, "shop/token", "s3cret")},
		{For: "clowder-system/hetzner", Encrypted: encrypt(t, cluster.recipients, "clowder-system/hetzner", "the Hetzner token")},
	}
	rekeyed, err := client.Rekey(context.Background(), values)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := cluster.apis[0].clusterIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"s3cret", "the Hetzner token"} {
		if rekeyed[index] == values[index].Encrypted {
			t.Errorf("value %d is not encrypted anew", index+1)
		}
		for _, key := range []age.Identity{identity, cluster.recovery} {
			opened, err := secretvalue.OpenFor(rekeyed[index], values[index].For, key)
			if err != nil {
				t.Errorf("value %d: %v", index+1, err)
			} else if string(opened.Bytes) != want {
				t.Errorf("value %d is %q, want %q", index+1, opened.Bytes, want)
			}
		}
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other := PlacedValue{For: "shop/token", Encrypted: encrypt(t, []age.Recipient{stranger.Recipient()}, "shop/token", "s3cret")}
	_, err = client.Rekey(context.Background(), []PlacedValue{values[0], other})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "value 2") {
		t.Errorf("a value of another cluster: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error shows the value: %v", err)
	}
	// the Hetzner token copied into another Secret is not encrypted anew for it
	moved := PlacedValue{For: "shop/token", Encrypted: values[1].Encrypted}
	_, err = client.Rekey(context.Background(), []PlacedValue{moved})
	if statusOf(err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "the value is for clowder-system/hetzner, not for shop/token") {
		t.Errorf("a value out of place: %v", err)
	}
}
