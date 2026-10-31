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
		string(objects[1].Spec) != `{"class":"fast","replicas":1,"size":"10G"}` {
		t.Errorf("objects %+v", objects)
	}
}

func TestAFileOfOtherDocumentsHoldsNoResource(t *testing.T) {
	for _, text := range []string{"services: {web: {image: nginx}}", "apiVersion: v1\nkind: ConfigMap", "", "- a list"} {
		if objects, err := ParseDocuments([]byte(text)); err != nil || len(objects) != 0 {
			t.Errorf("%q: %v, %v", text, objects, err)
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
		"apiVersion: clowder/v1\nkind: [":                                                    "document 1",
	} {
		if _, err := ParseDocuments([]byte(text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", text, want, err)
		}
	}
}
