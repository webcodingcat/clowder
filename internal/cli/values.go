package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// fileValue is a sealed value in a file, for secret rekey and decrypt.
type fileValue struct {
	node   *yaml.Node
	key    string
	secret string // the Secret's ID
	sealed string
}

// label names the value for people: "shop/db[password]".
func (v fileValue) label() string {
	return v.secret + "[" + v.key + "]"
}

// fileValues reads the values from the file's YAML nodes, because rekey
// needs the line of each value. It first refuses a file that apply would
// refuse, so the nodes it reads belong to valid SealedSecrets.
func fileValues(path string, content []byte) ([]fileValue, error) {
	if _, err := readFile(path, content, false); err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var values []fileValue
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return values, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(document.Content) == 0 {
			continue
		}
		top := document.Content[0]
		if text(field(top, "apiVersion")) != resource.APIVersion || text(field(top, "kind")) != resource.KindSealedSecret {
			continue
		}
		metadata := field(top, "metadata")
		id := names.ID(namespaceOrDefault(text(field(metadata, "namespace"))), text(field(metadata, "name")))
		data := field(field(top, "spec"), "encryptedData")
		for index := 0; data != nil && index+1 < len(data.Content); index += 2 { // key, value, key, value, ...
			key, value := data.Content[index], data.Content[index+1]
			values = append(values, fileValue{node: value, key: key.Value, secret: id, sealed: value.Value})
		}
	}
}

// field returns nil for a node that is not a mapping or a missing key, so a
// chain of calls ends in nil where a field is missing.
func field(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func text(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	return node.Value
}

// replaceValues leaves the rest of the file unchanged (comments, quotes,
// line breaks). It refuses a value that is not on one line, which is how
// clowder secret seal writes it.
func replaceValues(content []byte, values []fileValue, replacements []string) ([]byte, error) {
	lines := strings.SplitAfter(string(content), "\n")
	for index, value := range values {
		line := value.node.Line - 1
		if line < 0 || line >= len(lines) || strings.Count(lines[line], value.sealed) != 1 {
			return nil, fmt.Errorf("%s: write the value on one line, as clowder secret seal does", value.label())
		}
		lines[line] = strings.Replace(lines[line], value.sealed, replacements[index], 1)
	}
	return []byte(strings.Join(lines, "")), nil
}

// readIdentities reads stdin for "-", so the key can come from a password
// manager without a file on disk.
func readIdentities(from string, stdin io.Reader) ([]age.Identity, error) {
	source := stdin
	if from != "-" {
		file, err := os.Open(from)
		if err != nil {
			return nil, fmt.Errorf("read the key: %w", err)
		}
		defer func() { _ = file.Close() }() // read only
		source = file
	}
	identities, err := age.ParseIdentities(source)
	if err != nil {
		return nil, fmt.Errorf("read the key: %w", err)
	}
	return identities, nil
}
