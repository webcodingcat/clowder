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
	paths, err := yamlFiles(dir, report)
	if err != nil {
		return nil, fmt.Errorf("read the resources: %w", err)
	}
	var objects []resource.Object
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the resources: %w", err)
		}
		found, err := readFile(path, data, false)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			skipped(report, path, "no resource of clowder's")
		}
		objects = append(objects, found...)
	}
	return objects, nil
}

// yamlFiles lists the YAML files of a directory, sorted by name, not those
// below it; hidden files and other files are skipped and reported.
func yamlFiles(dir string, report io.Writer) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries { // sorted by name
		path := filepath.Join(dir, entry.Name())
		switch extension := filepath.Ext(entry.Name()); {
		case strings.HasPrefix(entry.Name(), "."):
			skipped(report, path, "hidden")
		case entry.IsDir():
			skipped(report, path, "a directory; only the files of the one named are read")
		case extension != ".yaml" && extension != ".yml":
			skipped(report, path, "not a YAML file")
		default:
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// skipped reports a file a command leaves alone.
func skipped(report io.Writer, path, why string) {
	_, _ = fmt.Fprintf(report, "skipped %s: %s\n", path, why) // a report only
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
