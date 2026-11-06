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

// seal seals a value for a Secret, as clowder secret seal does.
func seal(t *testing.T, recipient age.Recipient, secretID, value string) string {
	t.Helper()
	sealed, err := secretvalue.Seal(secretvalue.Value{For: secretID, Bytes: []byte(value)}, recipient)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
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
		put, err := server.putChange(object, false)
		batch.add(object.Ref(), state.ResourceChange{Put: &put}, err)
	}
	return server.runChanges(context.Background(), batch)
}

func TestTheCLIGetsThePublicSealingKey(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	var public PublicSealingKey
	if err := client.Get(context.Background(), "/v1/sealing-key", &public); err != nil ||
		public.Recipient != cluster.sealing.(*age.X25519Recipient).String() {
		t.Errorf("public sealing key %+v, %v", public, err)
	}
}

func TestOnlyAHostGetsTheSealingKey(t *testing.T) {
	cluster := newTestCluster(t)
	var answer SealingKeyAnswer
	host := cluster.client(cluster.hostCertificate(1), testVersion, 0)
	if err := host.Get(context.Background(), "/v1/internal/sealing-key", &answer); err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(answer.Identity)
	if err != nil || identity.Recipient().String() != cluster.sealing.(*age.X25519Recipient).String() {
		t.Errorf("the sealing key %v does not belong to the cluster's public key", err)
	}
	cli := cluster.client(cluster.cliCertificate(), testVersion, 0)
	if err := cli.Get(context.Background(), "/v1/internal/sealing-key", &answer); statusOf(err) != http.StatusForbidden {
		t.Errorf("the CLI asked for the sealing key: %v", err)
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
