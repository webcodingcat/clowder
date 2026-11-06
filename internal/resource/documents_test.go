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

func TestASecretsValuesComeFromDataAndStringData(t *testing.T) {
	objects, err := ParseDocuments([]byte(`
apiVersion: clowder/v1
kind: Secret
metadata: {namespace: shop, name: db}
spec:
  data: {password: "no Base64, but stringData wins", tls.key: AP8=}
  stringData: {user: shop, password: plain}
`))
	if err != nil {
		t.Fatal(err)
	}
	values, err := DecodeSecretSpec(objects[0].Spec)
	// stringData wins over data for the same key, as in Kubernetes
	if err != nil || len(values) != 3 || string(values["password"]) != "plain" || string(values["user"]) != "shop" ||
		string(values["tls.key"]) != "\x00\xff" {
		t.Errorf("values %q, %v", values, err)
	}
}

func TestASecretSpecIsRefusedWithoutNamingAValue(t *testing.T) {
	for spec, want := range map[string]string{
		`{}`:                                  "at least one key",
		`{"data":{"password":"s3cret!"}}`:     "key password: the value is not Base64",
		`{"stringData":{"a/b":"s3cret"}}`:     `key "a/b"`,
		`{"data":{},"stringData":{"..":"x"}}`: `key ".."`,
		`{"encrypted":{"password":"s3cret"}}`: "unknown field",
	} {
		_, err := DecodeSecretSpec([]byte(spec))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: want %q, got %v", spec, want, err)
		}
	}
}

func TestASealedSecretNamesItsValuesByValidKeys(t *testing.T) {
	if _, err := DecodeSealedSecretSpec([]byte(`{"encryptedData":{"password":"YWJj","tls.crt":"YWJj"}}`)); err != nil {
		t.Error(err)
	}
	for spec, want := range map[string]string{
		`{}`:                               "at least one key",
		`{"encryptedData":{"a/b":"YWJj"}}`: `key "a/b"`,
		`{"data":{"password":"YWJj"}}`:     "unknown field",
		`{"encryptedData":{"password":"YWJj"},"template":{}}`: "unknown field",
	} {
		if _, err := DecodeSealedSecretSpec([]byte(spec)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", spec, want, err)
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
