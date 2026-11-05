// Package clusterdir reads a cluster directory: cluster.yaml, whose types
// and checks are clusterspec's, with its values encrypted in it. The
// user's credentials live apart from it (clientconfig). It is portable
// (Linux, macOS, Windows).
package clusterdir

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/secretvalue"
)

// File names inside a cluster directory.
const (
	ConfigFile = clusterspec.FileName
	// EnvVar names the cluster directory when --cluster is not given.
	EnvVar = "CLOWDER_CLUSTER"
)

// Directory is a loaded cluster directory.
type Directory struct {
	Path string
	// Config is cluster.yaml as written, without defaults, with the names of
	// its secrets where it has their values.
	Config clusterspec.Config
	// Encrypted are the values of cluster.yaml by name, still encrypted:
	// only the cluster opens them.
	Encrypted map[string]string
}

// Find returns the cluster directory: the flag if set, else $CLOWDER_CLUSTER,
// else the working directory if it holds a cluster.yaml.
func Find(flag string) (string, error) {
	for _, candidate := range []string{flag, os.Getenv(EnvVar)} {
		if candidate != "" {
			return candidate, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the cluster directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFile)); err != nil {
		return "", fmt.Errorf("no cluster directory: use --cluster, set %s or run where %s is",
			EnvVar, ConfigFile)
	}
	return dir, nil
}

// Load reads cluster.yaml in dir: it takes the values tagged !secret out,
// checks that they stand exactly where cluster.yaml may hold a secret
// (clusterspec.SecretPlaces), and decodes the rest strictly. It does not
// run clusterspec.Check: a cluster.yaml is written bit by bit, the values
// that secret encrypt --cluster-yaml prints before the rest. cluster apply
// and diff check it.
func Load(dir string) (*Directory, error) {
	content, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("read the cluster directory: %w", err)
	}
	documents, err := secretvalue.Extract(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if len(documents) != 1 {
		return nil, fmt.Errorf("%s holds %d YAML documents, not one", ConfigFile, len(documents))
	}
	// yaml.v3 decodes a node without refusing unknown keys, so the node goes
	// back to text first
	extracted, err := yaml.Marshal(documents[0].Node)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if err := checkReplacedKeys(documents[0].Node); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	var config clusterspec.Config
	if err := decodeStrict(extracted, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if err := checkSecretPlaces(config, documents[0].Secrets); err != nil {
		return nil, err
	}
	return &Directory{Path: dir, Config: config, Encrypted: documents[0].Secrets}, nil
}

// checkSecretPlaces holds the values taken out of cluster.yaml to the
// places that may hold one: no value elsewhere, and at each such place a
// value, also where its path happens to stand as text.
func checkSecretPlaces(config clusterspec.Config, encrypted map[string]string) error {
	places := map[string]bool{}
	var problems clusterspec.Problems
	for _, place := range clusterspec.SecretPlaces(config) {
		places[place.Path] = true
		if _, ok := encrypted[place.Path]; !ok {
			problems = append(problems, fmt.Sprintf("%s: write the value encrypted, tagged !secret "+
				"(clowder secret encrypt --cluster-yaml)", place.Path))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(encrypted)) {
		if !places[path] {
			problems = append(problems, fmt.Sprintf("%s: !secret is not allowed here", path))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// replacedKeys are top-level keys of cluster.yaml that earlier releases
// read, with what took their place: a cluster.yaml written for one of them
// fails with the way forward, not only with an unknown field.
var replacedKeys = map[string]string{
	"hoster":    "write hetzner, with its token and api (there is no type any more)",
	"network":   "write private_network",
	"name":      "clowderd init --cluster-name names the cluster",
	"endpoints": "your credentials name the endpoints (clowder config get-contexts)",
}

// checkReplacedKeys refuses a document that has one of replacedKeys.
func checkReplacedKeys(document *yaml.Node) error {
	mapping := document
	if mapping.Kind == yaml.DocumentNode && len(mapping.Content) == 1 {
		mapping = mapping.Content[0]
	}
	if mapping.Kind != yaml.MappingNode {
		return nil // decodeStrict says what is wrong
	}
	var problems clusterspec.Problems
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key := mapping.Content[index].Value
		if replacement, ok := replacedKeys[key]; ok {
			problems = append(problems, fmt.Sprintf("line %d: %s is gone; %s",
				mapping.Content[index].Line, key, replacement))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// decodeStrict decodes YAML and refuses keys the target does not know, so a
// misspelled key is reported instead of silently ignored.
func decodeStrict(content []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
