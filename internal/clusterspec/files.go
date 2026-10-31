package clusterspec

import (
	"crypto/x509"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// The limits of the files next to cluster.yaml: they travel in the state,
// which every agent keeps in memory.
const (
	maxFiles    = 8
	maxFileSize = 64 << 10
)

// FileNames lists the files next to cluster.yaml that it names, sorted,
// each once: so far all of them CAs, acme.ca and each email channel's
// smtp.ca. cluster apply sends their contents along, so the cluster keeps
// them with the config.
func FileNames(config Config) []string {
	var files []string
	if config.ACME.CA != "" {
		files = append(files, config.ACME.CA)
	}
	if config.Alerts != nil {
		for _, channel := range config.Alerts.Channels {
			if channel.SMTP != nil && channel.SMTP.CA != "" {
				files = append(files, channel.SMTP.CA)
			}
		}
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
// cluster.yaml: one it names is missing or too large, or a CA file holds no
// certificate.
func CheckFiles(config Config, files map[string][]byte) Problems {
	var problems Problems
	for _, name := range FileNames(config) {
		content, ok := files[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("file %s is named in %s but missing", name, FileName))
		case len(content) > maxFileSize:
			problems = append(problems, fmt.Sprintf("file %s is larger than %d KiB", name, maxFileSize>>10))
		}
	}
	for _, name := range FileNames(config) { // every one a CA's so far
		if content, ok := files[name]; ok && !x509.NewCertPool().AppendCertsFromPEM(content) {
			problems = append(problems, fmt.Sprintf("the CA file %s holds no PEM certificate", name))
		}
	}
	return problems
}
