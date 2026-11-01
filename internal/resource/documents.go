package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// clowderVersions starts the apiVersion of every document of clowder's, in
// this version and any other.
const clowderVersions = "clowder/"

// ParseDocuments reads the YAML documents of a file (JSON is YAML too) as
// resources. A file holds clowder's resources only, or none of them (a
// Compose file, a Kubernetes manifest next to them): for such a file it
// returns none, so an apply of a directory can skip it. A file that mixes
// both is an error, and so is a resource of another apiVersion of
// clowder's. A resource is read strictly: a field it does not have is an
// error.
//
// A Secret's value stands encrypted, tagged !secret, in spec.value
// (secretvalue); it is the one place for it. The object's spec carries it
// as SecretSpec.Encrypted, still encrypted: only the cluster opens it.
func ParseDocuments(data []byte) ([]Object, error) {
	// the values come out first: turning YAML into JSON would drop their tag
	documents, err := secretvalue.Extract(data)
	if err != nil {
		return nil, err
	}
	var objects []Object
	others := 0
	for position, extracted := range documents {
		index, document := position+1, extracted.Node
		if len(document.Content) == 0 || document.Content[0].Tag == "!!null" {
			continue // an empty document, as after a trailing ---
		}
		encoded, err := jsonOf(document)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		var head struct {
			APIVersion any `json:"apiVersion"`
		}
		version := ""
		if json.Unmarshal(encoded, &head) == nil {
			version, _ = head.APIVersion.(string)
		}
		switch {
		case !strings.HasPrefix(version, clowderVersions):
			others++
			continue
		case version != APIVersion:
			return nil, fmt.Errorf("document %d: apiVersion %s is not %s, the one this clowder reads",
				index, version, APIVersion)
		}
		object, err := decodeObject(encoded)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		if err := takeValue(&object, extracted.Secrets); err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		objects = append(objects, object)
	}
	if len(objects) > 0 && others > 0 {
		return nil, errors.New("the file mixes clowder's resources with other documents; keep them apart")
	}
	return objects, nil
}

// secretValuePath is the one place a resource may hold a value tagged
// !secret: a Secret's spec.value.
const secretValuePath = "spec.value"

// takeValue puts a Secret's encrypted value into its spec, as the API takes
// it, and refuses a value tagged !secret anywhere else, or a Secret's value
// in clear: such a file would end up in a repository, a backup, an
// editor's history.
func takeValue(object *Object, encrypted map[string]string) error {
	armored, ok := encrypted[secretValuePath]
	switch {
	case object.Kind != KindSecret && len(encrypted) > 0:
		return fmt.Errorf("%s %s: !secret is allowed only in a Secret's %s", object.Kind, object.Metadata.Name,
			secretValuePath)
	case object.Kind != KindSecret:
		return nil
	case !ok:
		return fmt.Errorf("secret %s: write its value encrypted, tagged !secret (clowder secret encrypt)",
			object.Metadata.Name)
	case len(encrypted) > 1:
		return fmt.Errorf("secret %s: !secret is allowed only in %s", object.Metadata.Name, secretValuePath)
	}
	spec, err := json.Marshal(SecretSpec{Encrypted: armored})
	if err != nil {
		return err
	}
	object.Spec = spec
	return nil
}

// decodeObject reads a resource from JSON, strictly.
func decodeObject(encoded []byte) (Object, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var object Object
	if err := decoder.Decode(&object); err != nil {
		return Object{}, fmt.Errorf("read the resource: %w", err)
	}
	switch {
	case object.Kind == "":
		return Object{}, errors.New("kind is required")
	case object.Metadata.Name == "":
		return Object{}, fmt.Errorf("%s: metadata.name is required", object.Kind)
	}
	return object, nil
}

// jsonOf writes a YAML node as JSON and keeps each number as written: a
// field that takes text (a stack's env) gets "1.0", not "1".
func jsonOf(node *yaml.Node) ([]byte, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		return jsonOf(node.Content[0])
	case yaml.AliasNode:
		return jsonOf(node.Alias)
	case yaml.MappingNode:
		var object bytes.Buffer
		object.WriteByte('{')
		for index := 0; index+1 < len(node.Content); index += 2 { // key, value, key, value, ...
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be text", key.Line)
			}
			value, err := jsonOf(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			if index > 0 {
				object.WriteByte(',')
			}
			name, _ := json.Marshal(key.Value) // a string always encodes
			object.Write(name)
			object.WriteByte(':')
			object.Write(value)
		}
		object.WriteByte('}')
		return object.Bytes(), nil
	case yaml.SequenceNode:
		var array bytes.Buffer
		array.WriteByte('[')
		for index, item := range node.Content {
			value, err := jsonOf(item)
			if err != nil {
				return nil, err
			}
			if index > 0 {
				array.WriteByte(',')
			}
			array.Write(value)
		}
		array.WriteByte(']')
		return array.Bytes(), nil
	}
	switch node.ShortTag() {
	case "!!null":
		return []byte("null"), nil
	case "!!bool", "!!int", "!!float":
		if node.ShortTag() != "!!bool" && json.Valid([]byte(node.Value)) {
			return []byte(node.Value), nil // as written: 1.0, 8080
		}
		var value any // true, 0x10, 1e3 as YAML reads them
		if err := node.Decode(&value); err != nil {
			return nil, fmt.Errorf("line %d: %w", node.Line, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("line %d: %q is no number JSON can hold", node.Line, node.Value)
		}
		return encoded, nil
	default:
		encoded, _ := json.Marshal(node.Value) // a string always encodes
		return encoded, nil
	}
}
