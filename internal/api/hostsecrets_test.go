package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/stackspec"
	"github.com/webcodingcat/clowder/internal/state"
)

// withWorkerStack gives the leader's state the worker, host 2, with the
// stack default/shop that names the secret db; the secret other belongs to
// no stack. The worker holds no data key, as on a real host.
func withWorkerStack(t *testing.T) *testCluster {
	t.Helper()
	cluster := newTestCluster(t)
	keys := cluster.apis[0].config.Keys
	cluster.apis[workerHost].config.Keys = nil
	secret := func(value string) *state.Secret {
		encrypted, err := keys.Encrypt([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return &state.Secret{Spec: state.EncryptedSecret{
			Data: map[string]state.Encrypted{"password": encrypted}, Digest: "digest of " + value,
		}}
	}
	worker := workerHost
	cluster.clusters[0].Read(func(current *state.State) {
		current.Hosts[workerHost] = &state.Host{ID: workerHost, Name: "node2", Roles: rolesOf(workerHost), Phase: state.Active}
		current.Secrets[names.ID(names.Default, "db")] = secret("the db's")
		current.Secrets[names.ID(names.Default, "other")] = secret("another's")
		current.Stacks[names.ID(names.Default, "shop")] = &state.Stack{
			Meta: state.Meta{Namespace: names.Default, Name: "shop"}, Host: &worker,
			Spec: stackspec.Spec{Services: map[string]stackspec.Service{"app": {
				Image: "x", SecretEnv: map[string]names.SecretKeyRef{"PASSWORD": {Name: "db", Key: "password"}},
			}}},
		}
	})
	return cluster
}

func TestAWorkerGetsOnlyTheSecretsOfItsStacks(t *testing.T) {
	cluster := withWorkerStack(t)
	ctx := context.Background()
	secrets, err := cluster.apis[workerHost].SecretsOfThisHost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, ok := secrets["default/db"]
	if len(secrets) != 1 || !ok || db.Digest != "digest of the db's" || string(db.Data["password"]) != "the db's" {
		t.Errorf("the worker got %v, want only default/db", slices.Sorted(maps.Keys(secrets)))
	}

	// a control-plane host without the stack gets nothing over the route,
	// and nothing from its own state either
	var asked map[string]HostSecret
	if err := cluster.client(cluster.hostCertificate(1), testVersion, 0).Get(ctx, "/v1/internal/hosts/self/secrets", &asked); err != nil || len(asked) != 0 {
		t.Errorf("host 1 got %v, %v", slices.Sorted(maps.Keys(asked)), err)
	}
	if own, err := cluster.apis[1].SecretsOfThisHost(ctx); err != nil || len(own) != 0 {
		t.Errorf("host 1 opened %v, %v", slices.Sorted(maps.Keys(own)), err)
	}
}

func TestTheLeaderAnswersAWorkerFromAStateAsNewAsItsOwn(t *testing.T) {
	cluster := withWorkerStack(t)
	cluster.clusters[workerHost].setIndex(5)
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if secrets, err := cluster.apis[workerHost].SecretsOfThisHost(short); err == nil {
		t.Errorf("the leader answered from an older state: %v", slices.Sorted(maps.Keys(secrets)))
	}
	cluster.clusters[0].setIndex(5)
	if _, err := cluster.apis[workerHost].SecretsOfThisHost(context.Background()); err != nil {
		t.Errorf("once the leader caught up: %v", err)
	}
}

func TestOnlyTheHostOfTheFloatingIPGetsTheCertificatesKeys(t *testing.T) {
	cluster := withWorkerStack(t)
	key, err := cluster.apis[0].config.Keys.Encrypt([]byte("the key"))
	if err != nil {
		t.Fatal(err)
	}
	cluster.clusters[0].Read(func(current *state.State) {
		current.Certificates["shop.example.com"] = &state.Certificate{Domain: "shop.example.com", Key: key, Chain: []byte("chain")}
	})
	ctx := context.Background()
	if _, err := cluster.apis[workerHost].CertificatesOfThisHost(ctx); statusOf(err) != http.StatusForbidden {
		t.Errorf("a worker without the Floating IP: %v, want HTTP 403", err)
	}
	worker := workerHost
	cluster.clusters[0].Read(func(current *state.State) { current.Ingress = &worker })
	certificates, err := cluster.apis[workerHost].CertificatesOfThisHost(ctx)
	if certificate := certificates["shop.example.com"]; err != nil || string(certificate.Key) != "the key" ||
		string(certificate.Chain) != "chain" {
		t.Errorf("the host of the Floating IP got %v, %v", certificates, err)
	}
}

func TestAHostWithoutTheDataKeyOpensNothing(t *testing.T) {
	worker := withWorkerStack(t).apis[workerHost]
	if _, err := worker.secretsOf(workerHost); statusOf(err) != http.StatusServiceUnavailable {
		t.Errorf("secrets: %v, want HTTP 503", err)
	}
	if _, err := worker.certificatesOf(workerHost); statusOf(err) != http.StatusServiceUnavailable {
		t.Errorf("certificates: %v, want HTTP 503", err)
	}
	if _, err := worker.authority(); err == nil {
		t.Error("it opened the CA's key")
	}
	if _, err := worker.sealingKey(); err == nil {
		t.Error("it opened the sealing key")
	}
}
