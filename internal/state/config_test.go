package state

import (
	"errors"
	"slices"
	"testing"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// withToken is testConfig with the Hetzner token at another place.
func withToken(name, key string) clusterspec.Config {
	config := testConfig()
	config.Hetzner.TokenSecretRef = names.SecretKeyRef{Name: name, Key: key}
	return config
}

func TestTheConfigurationCountsGenerations(t *testing.T) {
	c := newCluster(t)
	if c.state.Configured() {
		t.Errorf("a new cluster is configured: %+v", c.state.Config)
	}
	if _, err := c.state.HetznerToken(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("the token of a new cluster: want ErrNotConfigured, got %v", err)
	}
	c.configure(testConfig())
	config := c.state.Config
	if !c.state.Configured() || config.Generation != 1 || config.Name != resource.ClusterConfigurationName || config.Owner != OwnerCLI {
		t.Errorf("configured %+v", config.Meta)
	}
	if token, err := c.state.HetznerToken(); err != nil || ClearSize(token) != 10 {
		t.Errorf("token of %d bytes, %v", ClearSize(token), err)
	}
	moved := testConfig()
	moved.Ingress.FloatingIP = "203.0.113.12"
	c.mustApply(KindChangeResources, cliActor, batch(configChange(moved)))
	if c.state.Config.Generation != 2 || c.state.Config.Spec.Ingress.FloatingIP != "203.0.113.12" {
		t.Errorf("changed %+v", c.state.Config)
	}
	c.refuseChange(configChange(clusterspec.Config{}), "is not an IPv4 network")
}

func TestTheConfigurationIsNamedClusterAndNamesSecretsOfClowderSystem(t *testing.T) {
	tests := []struct {
		name      string
		change    ResourceChange
		wantError string
	}{
		{
			"another name",
			putChange(resource.KindClusterConfiguration, "", "prod", testConfig()),
			"the ClusterConfiguration is named cluster, not prod",
		},
		{
			"a Secret that is in default only",
			configChange(withToken("hcloud", "token")),
			"hetzner.tokenSecretRef: clowder-system has no Secret hcloud with the key token",
		},
		{
			"a key the Secret lacks",
			configChange(withToken("hetzner", "api")),
			"hetzner.tokenSecretRef: clowder-system has no Secret hetzner with the key api",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			c.mustApply(KindChangeResources, cliActor, batch(
				hetznerSecret(),
				putChange(resource.KindSecret, names.Default, "hcloud",
					SealedSecret{Data: map[string]Sealed{"token": sealedOf(5)}, Digest: "d"}),
			))
			c.refuseChange(test.change, test.wantError)
			if c.state.Configured() {
				t.Error("the refused configuration is there")
			}
		})
	}
}

func TestTheConfigurationCannotBeDeleted(t *testing.T) {
	c := newCluster(t)
	c.refuseChange(deleteChange(resource.KindClusterConfiguration, "", resource.ClusterConfigurationName),
		"no ClusterConfiguration cluster")
	c.configure(testConfig())
	c.refuseChange(deleteChange(resource.KindClusterConfiguration, "", resource.ClusterConfigurationName),
		"the ClusterConfiguration cannot be deleted")
}

func TestASecretTheConfigurationNamesKeepsTheKey(t *testing.T) {
	c := newCluster(t)
	c.configure(testConfig())
	hetzner := func(digest string, keys ...string) ResourceChange {
		data := map[string]Sealed{}
		for _, key := range keys {
			data[key] = sealedOf(5)
		}
		return putChange(resource.KindSecret, names.System, "hetzner", SealedSecret{Data: data, Digest: digest})
	}
	c.refuseChange(deleteChange(resource.KindSecret, names.System, "hetzner"),
		"secret clowder-system/hetzner is named by hetzner.tokenSecretRef of the configuration")
	c.refuseChange(hetzner("d2", "api"), "secret clowder-system/hetzner must keep the key token, which hetzner.tokenSecretRef")
	// a key the configuration does not name comes and goes
	c.mustApply(KindChangeResources, cliActor, batch(hetzner("d3", "token", "api")))
	c.mustApply(KindChangeResources, cliActor, batch(hetzner("d4", "token")))
	// once the configuration names another Secret, the old one may go
	c.mustApply(KindChangeResources, cliActor, batch(
		putChange(resource.KindSecret, names.System, "hcloud",
			SealedSecret{Data: map[string]Sealed{"token": sealedOf(5)}, Digest: "d5"}),
		configChange(withToken("hcloud", "token")),
		deleteChange(resource.KindSecret, names.System, "hetzner"),
	))
	if _, ok := c.state.SystemSecret(hetznerToken); ok {
		t.Error("the old Secret is still there")
	}
	if _, err := c.state.HetznerToken(); err != nil {
		t.Errorf("the token of the new Secret: %v", err)
	}
}

// registryLogin puts a Secret of default with the keys of a registry login.
func registryLogin(name string, keys ...string) ResourceChange {
	data := map[string]Sealed{}
	for _, key := range keys {
		data[key] = sealedOf(len(name + key))
	}
	return putChange(resource.KindSecret, names.Default, name, SealedSecret{Data: data, Digest: "d-" + name})
}

// pullingWith puts namespace default with the image pull secrets.
func pullingWith(secrets ...string) ResourceChange {
	var spec resource.NamespaceSpec
	for _, secret := range secrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, resource.LocalObjectReference{Name: secret})
	}
	return putChange(resource.KindNamespace, "", names.Default, spec)
}

func TestRegistryLoginsComeInTheOrderOfImagePullSecrets(t *testing.T) {
	c := newCluster(t)
	c.mustApply(KindChangeResources, cliActor, batch(
		pullingWith("quay", "ghcr"),
		registryLogin("ghcr", resource.RegistryLoginKeys...),
		registryLogin("quay", resource.RegistryLoginKeys...),
	))
	logins, err := c.state.RegistryLogins(names.Default)
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, login := range logins {
		secrets = append(secrets, login.Secret)
	}
	if !slices.Equal(secrets, []string{"quay", "ghcr"}) {
		t.Errorf("logins of %v, want quay, then ghcr", secrets)
	}
	if login := logins[1]; ClearSize(login.Server) != len("ghcr"+resource.RegistryServerKey) ||
		ClearSize(login.Password) != len("ghcr"+resource.RegistryPasswordKey) {
		t.Errorf("login %s: server and password of %d and %d bytes", login.Secret, ClearSize(login.Server), ClearSize(login.Password))
	}
	if logins, err := c.state.RegistryLogins("shop"); err == nil {
		t.Errorf("logins of a namespace the cluster does not have: %v", logins)
	}
}

func TestRegistryLoginsNeedEverySecretAndKey(t *testing.T) {
	tests := []struct {
		name      string
		secrets   []ResourceChange
		wantError string
	}{
		{"a missing Secret", nil, "namespace default names the image pull secret ghcr, which it does not have"},
		{
			"a missing key",
			[]ResourceChange{registryLogin("ghcr", resource.RegistryServerKey, resource.RegistryUsernameKey)},
			"the image pull secret ghcr of namespace default has no key password",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCluster(t)
			c.mustApply(KindChangeResources, cliActor, batch(append([]ResourceChange{pullingWith("ghcr")}, test.secrets...)...))
			if _, err := c.state.RegistryLogins(names.Default); err == nil || err.Error() != test.wantError {
				t.Errorf("want %q, got %v", test.wantError, err)
			}
		})
	}
}
