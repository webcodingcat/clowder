package api

import (
	"context"
	"net/http"
	"testing"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/secretvalue"
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

func TestTheCLIGetsThePublicSealingKey(t *testing.T) {
	cluster := newTestCluster(t)
	client := cluster.client(cluster.cliCertificate(), testVersion, 0)
	var public PublicSealingKey
	if err := client.Get(context.Background(), "/v1/sealing-key", &public); err != nil ||
		public.Recipient != cluster.sealing.(*age.X25519Recipient).String() {
		t.Errorf("public sealing key %+v, %v", public, err)
	}
}

func TestOnlyTheHostItselfGetsTheSealingKey(t *testing.T) {
	cluster := newTestCluster(t)
	var answer SealingKeyAnswer
	host := cluster.client(cluster.hostCertificate(1), testVersion, 1)
	if err := host.Get(context.Background(), "/v1/internal/sealing-key", &answer); err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(answer.Identity)
	if err != nil || identity.Recipient().String() != cluster.sealing.(*age.X25519Recipient).String() {
		t.Errorf("the sealing key %v does not belong to the cluster's public key", err)
	}
	cli := cluster.client(cluster.cliCertificate(), testVersion, 1)
	if err := cli.Get(context.Background(), "/v1/internal/sealing-key", &answer); statusOf(err) != http.StatusForbidden {
		t.Errorf("the CLI asked for the sealing key: %v", err)
	}
}
