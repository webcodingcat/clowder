package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		return readFile("stdin", data, true)
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
		return readFile(from, data, true)
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
			found, err := readFile(path, data, false)
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

// readFile reads the resources of one file, a Secret's value encrypted in
// it (resource.ParseDocuments): the CLI never sees a value in clear, on
// stdin neither. Unless required, a file without resources holds none (a
// directory skips it); else it is an error.
func readFile(name string, data []byte, required bool) ([]resource.Object, error) {
	objects, err := resource.ParseDocuments(data)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: %w", name, err)
	case required && len(objects) == 0:
		return nil, fmt.Errorf("%s holds no resource of clowder's (apiVersion %s)", name, resource.APIVersion)
	}
	// a resource without a namespace is in default, as on the command line
	for index := range objects {
		if resource.Namespaced(objects[index].Kind) && objects[index].Metadata.Namespace == "" {
			objects[index].Metadata.Namespace = names.Default
		}
	}
	return objects, nil
}
