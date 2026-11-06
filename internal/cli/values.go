package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// valuePlace is where a value is in its file: the document, counted from 0
// as secretvalue.Rewrite counts, and the path in that document.
type valuePlace struct {
	document int
	path     string
}

// fileValue is a value of a file: where it is, the key in its Secret, and
// the Secret it must be for.
type fileValue struct {
	place valuePlace
	key   string
	api.PlacedValue
}

// label names the value for people: "shop/db[password]".
func (v fileValue) label() string {
	return v.For + "[" + v.key + "]"
}

// fileValues reads the values of a file (path names it in errors) for
// secret rekey and decrypt, the same way apply reads them: a value may only
// be in a Secret's spec.data, and it is for the Secret's ID.
func fileValues(path string, content []byte) ([]fileValue, error) {
	documents, err := secretvalue.Extract(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var secrets []resource.Object
	if slices.ContainsFunc(documents, func(d secretvalue.Document) bool { return len(d.Secrets) > 0 }) {
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
	// apply takes values only from the Secrets (readFile refused any other
	// place), so the documents with values are the Secrets, in their order;
	// the values that apply took confirm each pair
	next := 0 // the Secret that the next document with values must be
	var values []fileValue
	for index, document := range documents {
		if len(document.Secrets) == 0 {
			continue
		}
		if next == len(secrets) {
			return nil, fmt.Errorf("%s: document %d: a value tagged !secret stands only in a Secret's %s",
				path, index+1, resource.SecretDataPath)
		}
		secret := secrets[next]
		next++
		var spec resource.SecretSpec
		if err := json.Unmarshal(secret.Spec, &spec); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, index+1, err)
		}
		keyOf := map[string]string{}
		for key := range spec.Encrypted {
			keyOf[resource.SecretValuePath(key)] = key
		}
		for _, valuePath := range slices.Sorted(maps.Keys(document.Secrets)) {
			key, ok := keyOf[valuePath]
			if !ok || spec.Encrypted[key] != document.Secrets[valuePath] {
				return nil, fmt.Errorf("%s: document %d: its value at %s is not one apply takes for secret %s",
					path, index+1, valuePath, secret.Metadata.Name)
			}
			values = append(values, fileValue{
				place: valuePlace{index, valuePath}, key: key,
				PlacedValue: api.PlacedValue{
					For: names.ID(secret.Metadata.Namespace, secret.Metadata.Name), Encrypted: document.Secrets[valuePath],
				},
			})
		}
	}
	return values, nil
}

// readIdentities reads age identities, such as the recovery key, from a
// file or from stdin ("-"). Stdin lets them come from a password manager,
// without a file on disk.
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
