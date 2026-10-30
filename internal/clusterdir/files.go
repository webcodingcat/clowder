package clusterdir

import (
	"crypto/x509"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The limits of the files next to cluster.yaml: they travel in the state,
// which every agent keeps in memory.
const (
	maxFiles    = 8
	maxFileSize = 64 << 10
)

// FileNames lists the files next to cluster.yaml that it names (acme.ca),
// sorted. cluster apply sends their contents along, so the cluster keeps
// them with the config.
func FileNames(config Config) []string {
	var files []string
	if config.ACME.CA != "" {
		files = append(files, config.ACME.CA)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// validFileName accepts a path relative to cluster.yaml, written with "/"
// (the CLI runs on Windows too) and without "..": the key of a file in the
// state, the same from every machine.
func validFileName(name string) bool {
	return fs.ValidPath(name) && name != "." && !strings.ContainsAny(name, `\:`)
}

// CheckFiles returns what is wrong with the files that come with
// cluster.yaml: one it names is missing or too large, or acme.ca holds no
// certificate.
func CheckFiles(config Config, files map[string][]byte) Problems {
	var problems Problems
	for _, name := range FileNames(config) {
		content, ok := files[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("file %s is named in %s but missing", name, ConfigFile))
		case len(content) > maxFileSize:
			problems = append(problems, fmt.Sprintf("file %s is larger than %d KiB", name, maxFileSize>>10))
		}
	}
	if name := config.ACME.CA; name != "" {
		if content, ok := files[name]; ok && !x509.NewCertPool().AppendCertsFromPEM(content) {
			problems = append(problems, fmt.Sprintf("acme.ca: %s holds no PEM certificate", name))
		}
	}
	return problems
}

// Files reads the files cluster.yaml names, by their names in it.
func (d *Directory) Files() (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, name := range FileNames(d.Config) {
		content, err := os.ReadFile(filepath.Join(d.Path, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("read the file %s named in %s: %w", name, ConfigFile, err)
		}
		files[name] = content
	}
	if problems := CheckFiles(d.Config, files); len(problems) > 0 {
		return nil, problems
	}
	return files, nil
}
