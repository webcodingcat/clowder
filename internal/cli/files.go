package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/clusterdir"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// readResources reads the resources of a file, of the YAML files of a
// directory, or of stdin ("-"). A file or stdin must hold resources; in a
// directory, hidden files, other files and files of other documents are
// skipped, and each is reported to report.
func readResources(from string, stdin io.Reader, report io.Writer) ([]resource.Object, error) {
	if from == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return readFile("stdin", data, true, false)
	}
	info, err := os.Stat(from)
	if err != nil {
		return nil, fmt.Errorf("read the resources: %w", err)
	}
	if !info.IsDir() {
		data, err := os.ReadFile(from)
		if err != nil {
			return nil, fmt.Errorf("read the resources: %w", err)
		}
		return readFile(from, data, true, true)
	}
	return readDirectory(from, report)
}

// readDirectory reads the YAML files of a directory, not those below it.
// A cluster directory is refused: its files are cluster apply's.
func readDirectory(dir string, report io.Writer) ([]resource.Object, error) {
	if _, err := os.Stat(filepath.Join(dir, clusterdir.ConfigFile)); err == nil {
		return nil, fmt.Errorf("%s holds %s: it is a cluster directory, which cluster apply takes", dir, clusterdir.ConfigFile)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the resources: %w", err)
	}
	var objects []resource.Object
	skip := func(name, why string) {
		_, _ = fmt.Fprintf(report, "skipped %s: %s\n", filepath.Join(dir, name), why) // a report only
	}
	for _, entry := range entries { // sorted by name
		name := entry.Name()
		switch extension := filepath.Ext(name); {
		case strings.HasPrefix(name, "."):
			skip(name, "hidden")
		case entry.IsDir():
			skip(name, "a directory; apply reads no directories below the one named")
		case extension != ".yaml" && extension != ".yml":
			skip(name, "not a YAML file")
		default:
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read the resources: %w", err)
			}
			found, err := readFile(path, data, false, true)
			if err != nil {
				return nil, err
			}
			if len(found) == 0 {
				skip(name, "no resource of clowder's")
			}
			objects = append(objects, found...)
		}
	}
	return objects, nil
}

// readFile reads the resources of one file. A SOPS file of a resource is
// decrypted with the user's age key and must hold one Secret: a file of its
// own per secret keeps sopsage as it is, every value encrypted. A SOPS file
// of anything else (the cluster's secrets) is not decrypted. Unless
// required, a file without resources holds none (a directory skips it);
// else it is an error. A Secret in a file (inFile) in clear is refused: such
// a file ends up in a repository, a backup, an editor's history. On stdin
// the value comes from somewhere else, a password manager or a pipe.
func readFile(name string, data []byte, required, inFile bool) ([]resource.Object, error) {
	encrypted, holdsResource := sopsFile(data)
	switch {
	case encrypted && !holdsResource && required:
		return nil, fmt.Errorf("%s is a SOPS file of no resource of clowder's (no apiVersion)", name)
	case encrypted && !holdsResource:
		return nil, nil
	case encrypted:
		cleartext, err := clusterdir.DecryptSOPS(name, data)
		if err != nil {
			return nil, err
		}
		data = cleartext
	}
	objects, err := resource.ParseDocuments(data)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: %w", name, err)
	case encrypted && (len(objects) != 1 || objects[0].Kind != resource.KindSecret):
		return nil, fmt.Errorf("%s: a SOPS file holds exactly one Secret", name)
	case required && len(objects) == 0:
		return nil, fmt.Errorf("%s holds no resource of clowder's (apiVersion %s)", name, resource.APIVersion)
	}
	// a resource without a namespace is in default, as on the command line
	for index := range objects {
		if resource.Namespaced(objects[index].Kind) && objects[index].Metadata.Namespace == "" {
			objects[index].Metadata.Namespace = names.Default
		}
		if !encrypted && inFile && objects[index].Kind == resource.KindSecret {
			return nil, fmt.Errorf("%s holds Secret %s in clear: keep it in a SOPS file of its own, or pass it on stdin",
				name, objects[index].Metadata.Name)
		}
	}
	return objects, nil
}

// sopsFile tells whether a file is SOPS encrypted (its first document has
// SOPS's own key) and whether it holds a resource: SOPS encrypts the values
// but keeps the keys, so a resource still shows its apiVersion key.
func sopsFile(data []byte) (encrypted, holdsResource bool) {
	var document map[string]any
	_ = yaml.Unmarshal(data, &document) // what does not read is no SOPS file; ParseDocuments says why
	_, encrypted = document["sops"]
	_, holdsResource = document["apiVersion"]
	return encrypted, holdsResource
}
