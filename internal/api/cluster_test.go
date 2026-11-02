package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/state"
)

// encrypt encrypts a value for a place, as clowder secret encrypt does.
func encrypt(t *testing.T, recipients []age.Recipient, place, value string) string {
	t.Helper()
	armored, err := secretvalue.Encrypt(secretvalue.Value{For: place, Bytes: []byte(value)}, recipients...)
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

const registryPassword = `registries["ghcr.io"].password`

// testApply is a cluster.yaml with a hoster's token and a registry's login.
func (c *testCluster) testApply(token string) ClusterApply {
	return ClusterApply{
		Spec: clusterspec.Config{
			Name: "test", Seeds: []string{"10.0.0.2"},
			Hoster:         clusterspec.Hoster{Type: "hetzner", Token: "hoster.token"},
			Ingress:        clusterspec.Ingress{FloatingIP: "203.0.113.11"},
			StorageClasses: map[string]clusterspec.StorageClass{"fast": {Pool: "vg-fast/thin"}},
			Registries:     map[string]clusterspec.Registry{"ghcr.io": {Username: "robot", Password: registryPassword}},
		},
		Encrypted: map[string]string{
			"hoster.token":   encrypt(c.t, c.recipients, secretvalue.Cluster, token),
			registryPassword: encrypt(c.t, c.recipients, secretvalue.Cluster, "the password"),
		},
	}
}

func TestTheClusterOpensAndSealsTheValuesOfClusterYAML(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	if _, err := client.Put(context.Background(), "/v1/cluster", cluster.testApply("the token")); err != nil {
		t.Fatal(err)
	}
	var login state.RegistryLogin
	var token state.Sealed
	cluster.clusters[0].Read(func(current *state.State) {
		login = current.RegistryLogins()["ghcr.io"]
		token, _ = current.ClusterSecret("hoster.token")
	})
	keys := cluster.apis[0].config.Keys
	password, err := keys.Open(login.Password)
	if err != nil || login.Username != "robot" || string(password) != "the password" {
		t.Errorf("login %s with %q, %v", login.Username, password, err)
	}
	if opened, err := keys.Open(token); err != nil || string(opened) != "the token" {
		t.Errorf("token %q, %v", opened, err)
	}
	var encryption Encryption
	if err := client.Get(context.Background(), "/v1/encryption", &encryption); err != nil ||
		encryption.Cluster != cluster.recipients[0].(*age.X25519Recipient).String() ||
		encryption.Recovery != cluster.recipients[1].(*age.X25519Recipient).String() {
		t.Errorf("encryption %+v, %v", encryption, err)
	}
}

func TestAValueForAnotherPlaceOrClusterIsRefused(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	elsewhere := cluster.testApply("the token")
	elsewhere.Encrypted["hoster.token"] = encrypt(t, cluster.recipients, "shop/token", "the token")
	_, err := client.Put(context.Background(), "/v1/cluster", elsewhere)
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "hoster.token: the value is for shop/token") {
		t.Errorf("a value for a Secret: %v", err)
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other := cluster.testApply("the token")
	other.Encrypted["hoster.token"] = encrypt(t, []age.Recipient{stranger.Recipient()}, secretvalue.Cluster, "the token")
	_, err = client.Put(context.Background(), "/v1/cluster", other)
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "another cluster") ||
		strings.Contains(err.Error(), "the token") {
		t.Errorf("a value of another cluster: %v", err)
	}
}

func TestADiffNamesWhatAnApplyWouldChange(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	ctx := context.Background()
	// init's join key goes, as this cluster.yaml does not name it
	first, err := client.DiffCluster(ctx, cluster.testApply("the token"))
	if err != nil || first.Revision != 0 || !slices.Contains(first.Fields, "hoster") ||
		!slices.Equal(first.Secrets, []string{"hoster.token", "join_keys[0]", registryPassword}) {
		t.Errorf("before the first apply: %+v, %v", first, err)
	}
	if _, err := client.Put(ctx, "/v1/cluster", cluster.testApply("the token")); err != nil {
		t.Fatal(err)
	}
	// encrypted anew, the same values are no change
	if same, err := client.DiffCluster(ctx, cluster.testApply("the token")); err != nil || same.Revision != 1 ||
		len(same.Fields)+len(same.Files)+len(same.Secrets) != 0 {
		t.Errorf("the same cluster.yaml: %+v, %v", same, err)
	}
	changed := cluster.testApply("a new token")
	changed.Spec.PrivateNetwork = "10.0.0.0/16"
	if diff, err := client.DiffCluster(ctx, changed); err != nil || !slices.Equal(diff.Fields, []string{"private_network"}) ||
		!slices.Equal(diff.Secrets, []string{"hoster.token"}) {
		t.Errorf("a new token and private network: %+v, %v", diff, err)
	}
	// what apply-config refuses, the diff refuses as well
	refused := cluster.testApply("the token")
	delete(refused.Encrypted, registryPassword)
	if _, err := client.DiffCluster(ctx, refused); statusOf(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), registryPassword+": the value is missing") {
		t.Errorf("a missing value: %v", err)
	}
	var info ClusterInfo
	if err := client.Get(ctx, "/v1/cluster", &info); err != nil || info.Revision != 1 {
		t.Errorf("the dry runs changed the cluster: %+v, %v", info, err)
	}
}

func TestRekeyKeepsEachValueAndItsPlace(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.userCertificate("alice"), testVersion, 0)
	values := []PlacedValue{
		{For: "shop/token", Encrypted: encrypt(t, cluster.recipients, "shop/token", "s3cret")},
		{For: secretvalue.Cluster, Encrypted: encrypt(t, cluster.recipients, secretvalue.Cluster, "the hoster's token")},
	}
	rekeyed, err := client.Rekey(context.Background(), values)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := cluster.apis[0].clusterIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"s3cret", "the hoster's token"} {
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
	// the hoster's token copied into a Secret is not encrypted anew for it
	moved := PlacedValue{For: "shop/token", Encrypted: values[1].Encrypted}
	_, err = client.Rekey(context.Background(), []PlacedValue{moved})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "the value is for cluster, not for shop/token") {
		t.Errorf("a value out of place: %v", err)
	}
}

func TestChangedKeysNameWhatCameWentOrChanged(t *testing.T) {
	applied := map[string][]byte{"same.pem": []byte("a"), "changed.pem": []byte("a"), "gone.pem": []byte("a")}
	next := map[string][]byte{"same.pem": []byte("a"), "changed.pem": []byte("b"), "new.pem": []byte("a")}
	changed := changedKeys(applied, next, func(a, b []byte) bool { return string(a) == string(b) })
	if want := []string{"changed.pem", "gone.pem", "new.pem"}; !slices.Equal(changed, want) {
		t.Errorf("changed %v, want %v", changed, want)
	}
}
