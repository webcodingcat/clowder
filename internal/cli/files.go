package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/resource"
)

// readResourceFiles reads a file, a directory, or stdin ("-"). A file or
// stdin must hold resources; a directory's other files are skipped and
// named on report.
func readResourceFiles(from string, stdin io.Reader, report io.Writer) ([]resource.Object, error) {
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

func readDirectory(dir string, report io.Writer) ([]resource.Object, error) {
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

func skipped(report io.Writer, path, why string) {
	_, _ = fmt.Fprintf(report, "skipped %s: %s\n", path, why) // a report only
}

// readFile allows a file without resources unless required is set, so that
// a directory can skip such a file.
func readFile(name string, data []byte, required bool) ([]resource.Object, error) {
	objects, err := resource.ParseDocuments(data)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: %w", name, err)
	case required && len(objects) == 0:
		return nil, fmt.Errorf("%s holds no resource of clowder's (apiVersion %s)", name, resource.APIVersion)
	}
	for index := range objects {
		if resource.Namespaced(objects[index].Kind) {
			objects[index].Metadata.Namespace = namespaceOrDefault(objects[index].Metadata.Namespace)
		}
	}
	return objects, nil
}

// namespaceOrDefault defaults a file's namespace as the command line does.
func namespaceOrDefault(namespace string) string {
	if namespace == "" {
		return names.Default
	}
	return namespace
}
