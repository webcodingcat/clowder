package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// valuePlace is where a value stands in its file: the document, counted
// from 0 as secretvalue.Rewrite counts, and the path in it.
type valuePlace struct {
	document int
	path     string
}

// fileValue is a value of a file, where it stands, and what it must be for.
type fileValue struct {
	place valuePlace
	api.PlacedValue
}

// fileValues reads the values of a file (path names it in errors), for
// secret rekey and decrypt. In a file named cluster.yaml every value is
// for the cluster. Any other file is read as apply reads it: a value
// stands only in a Secret's spec.value and is for the Secret's ID.
func fileValues(path string, content []byte) ([]fileValue, error) {
	documents, err := secretvalue.Extract(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	inClusterYAML := filepath.Base(path) == clusterdir.ConfigFile
	var secrets []resource.Object
	if !inClusterYAML && slices.ContainsFunc(documents, func(d secretvalue.Document) bool { return len(d.Secrets) > 0 }) {
		objects, err := readFile(path, content, false)
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			if object.Kind == resource.KindSecret {
				secrets = append(secrets, object)
			}
		}
	}
	// apply takes one value from each Secret and none from anything else,
	// so the documents with a value are the Secrets in their order; the
	// value apply took confirms each pair
	next := 0 // the Secret the next value must be
	var values []fileValue
	for index, document := range documents {
		for _, valuePath := range slices.Sorted(maps.Keys(document.Secrets)) {
			value := fileValue{
				place:       valuePlace{index, valuePath},
				PlacedValue: api.PlacedValue{For: secretvalue.Cluster, Encrypted: document.Secrets[valuePath]},
			}
			if !inClusterYAML {
				if next == len(secrets) {
					return nil, fmt.Errorf("%s: document %d: a value tagged !secret stands only in a Secret "+
						"or in a file named %s", path, index+1, clusterdir.ConfigFile)
				}
				secret := secrets[next]
				var spec resource.SecretSpec
				if err := json.Unmarshal(secret.Spec, &spec); err != nil || spec.Encrypted != value.Encrypted {
					return nil, fmt.Errorf("%s: document %d: its value is not the one apply takes for secret %s",
						path, index+1, secret.Metadata.Name)
				}
				value.For = names.ID(secret.Metadata.Namespace, secret.Metadata.Name)
				next++
			}
			values = append(values, value)
		}
	}
	return values, nil
}

// readIdentities reads age identities, such as the recovery key, from a file
// or from stdin ("-"): from a password manager, without a file on disk.
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
