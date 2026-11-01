// Package clusterdir reads a cluster directory (cluster.yaml, whose types
// and checks are clusterspec's, with its values encrypted in it) and the
// user's credentials, which live apart from it in the user's config
// directory. It is portable (Linux, macOS, Windows). The SOPS files of
// Secret resources are decrypted here too, with the user's age key as SOPS
// finds it (SOPS_AGE_KEY_FILE, SOPS_AGE_KEY or SOPS's default path).
package clusterdir

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterspec"
	"github.com/webcodingcat/clowder/internal/secretvalue"
	"github.com/webcodingcat/clowder/internal/sopsage"
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
	dir, err := Target(flag)
	if err != nil || flag != "" || os.Getenv(EnvVar) != "" {
		return dir, err
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
// run clusterspec.Check: a cluster.yaml that is not complete yet, as the
// bootstrap writes it, still names the cluster for the commands that need
// only its name and seeds. cluster apply and diff check it.
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

// DecryptSOPS decrypts a SOPS file of a Secret resource with the user's age
// key, as sops finds it; file names it in messages. It refuses a file with
// values left in clear (a rule like unencrypted_suffix): such a value would
// sit in the repository in clear.
func DecryptSOPS(file string, sealed []byte) ([]byte, error) {
	identities, err := sopsage.LoadIdentities()
	if err != nil {
		return nil, fmt.Errorf("load the age key for %s: %w", file, err)
	}
	cleartext, err := sopsage.Decrypt(sealed, identities)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s (is your age key set up for SOPS?): %w", file, err)
	}
	inClear, err := sopsage.LeftInClear(sealed)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if len(inClear) > 0 {
		return nil, fmt.Errorf("%s keeps %s in clear; encrypt every value (no unencrypted_suffix)",
			file, strings.Join(inClear, ", "))
	}
	return cleartext, nil
}

// decodeStrict decodes YAML and refuses keys the target does not know, so a
// misspelled key is reported instead of silently ignored.
func decodeStrict(content []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
