package sopsage

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// walkLeaves calls visit for every scalar below node in document order, with
// the path of mapping keys that SOPS uses (sequence items share the path of
// their sequence). Comments are dropped: SOPS encrypts them, and clowder's
// files need none.
func walkLeaves(node *yaml.Node, path []string, visit func(leaf *yaml.Node, path []string) error) error {
	node.HeadComment, node.LineComment, node.FootComment = "", "", ""
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := walkLeaves(child, path, visit); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			key.HeadComment, key.LineComment, key.FootComment = "", "", ""
			if err := walkLeaves(value, append(path[:len(path):len(path)], key.Value), visit); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		return visit(node, path)
	case yaml.AliasNode:
		return fmt.Errorf("%s: YAML aliases are not supported", strings.Join(path, "."))
	}
	return nil
}

// plainValue returns a scalar's bytes and SOPS type as SOPS sees them:
// booleans as True/False, numbers in Go's shortest form. A null has no value
// (skip): SOPS neither encrypts nor hashes it.
func plainValue(node *yaml.Node) (plaintext []byte, valueType string, skip bool, err error) {
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, "", false, fmt.Errorf("read %q: %w", node.Value, err)
	}
	switch typed := value.(type) {
	case nil:
		return nil, "", true, nil
	case string:
		return []byte(typed), "str", false, nil
	case int:
		return []byte(strconv.Itoa(typed)), "int", false, nil
	case int64:
		return []byte(strconv.FormatInt(typed, 10)), "int", false, nil
	case uint64:
		return []byte(strconv.FormatUint(typed, 10)), "int", false, nil
	case float64:
		return []byte(strconv.FormatFloat(typed, 'f', -1, 64)), "float", false, nil
	case bool:
		if typed {
			return []byte("True"), "bool", false, nil
		}
		return []byte("False"), "bool", false, nil
	case time.Time:
		text, err := typed.MarshalText()
		return text, "time", false, err
	default:
		return nil, "", false, fmt.Errorf("value %q has an unsupported type %T", node.Value, value)
	}
}

// setPlain writes a decrypted value back into its node with the YAML tag of
// its SOPS type.
func setPlain(node *yaml.Node, plaintext []byte, valueType string) error {
	node.Style = 0
	switch valueType {
	case "str", "bytes":
		node.Tag, node.Value = "!!str", string(plaintext)
	case "int":
		node.Tag, node.Value = "!!int", string(plaintext)
	case "float":
		node.Tag, node.Value = "!!float", string(plaintext)
	case "bool":
		value, err := strconv.ParseBool(string(plaintext))
		if err != nil {
			return fmt.Errorf("bool %q: %w", plaintext, err)
		}
		node.Tag, node.Value = "!!bool", strconv.FormatBool(value)
	case "time":
		node.Tag, node.Value = "!!timestamp", string(plaintext)
	default:
		return fmt.Errorf("unknown SOPS type %q", valueType)
	}
	return nil
}
