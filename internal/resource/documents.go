package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// clowderVersions starts the apiVersion of every clowder document, of this
// version or any other.
const clowderVersions = "clowder/"

// ParseDocuments reads the YAML documents of a file as resources (JSON is
// YAML too). A file holds either only clowder's resources or none of them,
// like a Compose file or a Kubernetes manifest in the same directory. For
// a file without clowder's resources it returns none, so that an apply of
// a directory can skip the file. A file that mixes both is an error, and
// so is a resource of another clowder apiVersion. Resources are read
// strictly: a field that the resource does not have is an error.
func ParseDocuments(data []byte) ([]Object, error) {
	documents, err := yamlDocuments(data)
	if err != nil {
		return nil, err
	}
	var objects []Object
	others := 0
	for position, document := range documents {
		index := position + 1
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
		objects = append(objects, object)
	}
	if len(objects) > 0 && others > 0 {
		return nil, errors.New("the file mixes clowder's resources with other documents; keep them apart")
	}
	return objects, nil
}

// yamlDocuments reads every YAML document of data as a node tree.
func yamlDocuments(data []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []*yaml.Node
	for {
		document := &yaml.Node{}
		err := decoder.Decode(document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read YAML: %w", err)
		}
		documents = append(documents, document)
	}
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

// jsonOf writes a YAML node as JSON. It keeps each number as written, so
// that a field that takes text (a stack's env) gets "1.0", not "1".
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
