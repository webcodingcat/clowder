package clusterdir

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/webcodingcat/clowder/internal/clusterspec"
)

// Files reads the files cluster.yaml names, by their names in it.
func (d *Directory) Files() (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, name := range clusterspec.FileNames(d.Config) {
		content, err := os.ReadFile(filepath.Join(d.Path, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("read the file %s named in %s: %w", name, ConfigFile, err)
		}
		files[name] = content
	}
	if problems := clusterspec.CheckFiles(d.Config, files); len(problems) > 0 {
		return nil, problems
	}
	return files, nil
}
