package resource

import (
	"strings"
	"testing"
)

func TestAFileHoldsResourcesInOrder(t *testing.T) {
	objects, err := ParseDocuments([]byte(`
apiVersion: clowder/v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: clowder/v1
kind: Volume
metadata: {namespace: shop, name: data}
spec: {class: fast, size: 10G, replicas: 1}
---
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[1].Ref() != (Ref{Kind: KindVolume, Namespace: "shop", Name: "data"}) ||
		string(objects[1].Spec) != `{"class":"fast","size":"10G","replicas":1}` {
		t.Errorf("objects %+v", objects)
	}
}

func TestNumbersKeepTheTextTheyWereWrittenIn(t *testing.T) {
	objects, err := ParseDocuments([]byte(`
apiVersion: clowder/v1
kind: Stack
metadata: {name: shop}
spec: {services: {app: {image: x, env: {RATIO: 1.0, PORT: 8080, DEBUG: true, HEX: 0x10, NAME: shop}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"services":{"app":{"image":"x","env":{"RATIO":1.0,"PORT":8080,"DEBUG":true,"HEX":16,"NAME":"shop"}}}}`
	if string(objects[0].Spec) != want {
		t.Errorf("spec %s, want %s", objects[0].Spec, want)
	}
}

func TestAFileOfOtherDocumentsHoldsNoResource(t *testing.T) {
	for _, text := range []string{"services: {web: {image: nginx}}", "apiVersion: v1\nkind: ConfigMap", "", "- a list"} {
		if objects, err := ParseDocuments([]byte(text)); err != nil || len(objects) != 0 {
			t.Errorf("%q: %v, %v", text, objects, err)
		}
	}
}

const testArmor = "-----BEGIN AGE ENCRYPTED FILE-----\nYWJj\n-----END AGE ENCRYPTED FILE-----\n"

func TestASecretsValuesComeEncryptedFromSpecData(t *testing.T) {
	objects, err := ParseDocuments([]byte(`
apiVersion: clowder/v1
kind: Secret
metadata: {namespace: shop, name: db}
spec:
  data:
    password: !secret |
      -----BEGIN AGE ENCRYPTED FILE-----
      YWJj
      -----END AGE ENCRYPTED FILE-----
    tls.key: !secret |
      -----BEGIN AGE ENCRYPTED FILE-----
      YWJj
      -----END AGE ENCRYPTED FILE-----
`))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := DecodeSecretSpec(objects[0].Spec)
	if err != nil || len(spec.Encrypted) != 2 || spec.Encrypted["password"] != testArmor ||
		spec.Encrypted["tls.key"] != testArmor {
		t.Errorf("spec %+v, %v", spec, err)
	}
}

func TestASecretValueStandsOnlyEncryptedAndOnlyInASecret(t *testing.T) {
	block := func(indent string) string {
		return "!secret |\n" + indent + strings.ReplaceAll(strings.TrimSpace(testArmor), "\n", "\n"+indent) + "\n"
	}
	for text, want := range map[string]string{
		"apiVersion: clowder/v1\nkind: Secret\nmetadata: {name: token}\nspec: {data: {token: s3cret}}":   "write the value of token encrypted",
		"apiVersion: clowder/v1\nkind: Volume\nmetadata: {name: data}\nspec:\n  class: " + block("    "): "only in a Secret's spec.data",
		"apiVersion: clowder/v1\nkind: Secret\nmetadata:\n  name: " + block("    ") +
			"spec:\n  data:\n    token: " + block("      "): "only in spec.data",
	} {
		_, err := ParseDocuments([]byte(text))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%q: want %q, got %v", text, want, err)
		}
	}
}

func TestAResourceIsReadStrictly(t *testing.T) {
	for text, want := range map[string]string{
		"apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop}\n---\nservices: {}": "mixes",
		"apiVersion: clowder/v2\nkind: Namespace\nmetadata: {name: shop}":                    "not clowder/v1",
		"apiVersion: clowder/v1\nkind: Namespace\nmetadata: {name: shop, labels: {}}":        "unknown field",
		"apiVersion: clowder/v1\nkind: Namespace\nmetadata: {}":                              "metadata.name is required",
		"apiVersion: clowder/v1\nmetadata: {name: shop}":                                     "kind is required",
		"apiVersion: clowder/v1\nkind: [":                                                    "line 2",
	} {
		if _, err := ParseDocuments([]byte(text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", text, want, err)
		}
	}
}
