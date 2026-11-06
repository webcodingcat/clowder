// Package clientconfig reads and writes the CLI's credentials file, which
// is like kubectl's kubeconfig (docs/developing/specs.md). It is portable
// (Linux, macOS, Windows).
package clientconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/atomicfile"
)

// EnvVar names the file when --config is not given.
const EnvVar = "CLOWDER_CONFIG"

// The errors from picking and adding a context. They are separate values,
// so that the CLI can tell the user what to do about each.
var (
	ErrNoCurrentContext = errors.New("no current context")
	ErrNoContext        = errors.New("no such context")
	ErrContextTaken     = errors.New("a context of that name exists already")
)

// Config is the whole file.
type Config struct {
	// CurrentContext is the context that a command uses without --context.
	CurrentContext string    `yaml:"current_context"`
	Contexts       []Context `yaml:"contexts"`
}

// Context is the way into one cluster: the agents to ask, the CA that
// their certificates must come from, and the CLI's key and certificate.
// Whoever holds a context controls the cluster.
type Context struct {
	// Name may be any text that is not empty, because it names no file and
	// is sent nowhere.
	Name string `yaml:"name"`
	// Endpoints are asked in turn. Each uses the API's port unless it names
	// its own.
	Endpoints   []string `yaml:"endpoints"`
	CA          string   `yaml:"ca"`
	Certificate string   `yaml:"certificate"`
	Key         string   `yaml:"key"`
}

// Path returns the file that the CLI reads: the flag, or else
// $CLOWDER_CONFIG, or else ~/.clowder/config.
func Path(flag string) (string, error) {
	for _, candidate := range []string{flag, os.Getenv(EnvVar)} {
		if candidate != "" {
			return candidate, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the credentials file: %w", err)
	}
	return filepath.Join(home, ".clowder", "config"), nil
}

// Read reads the file at path. A missing file reads as an empty config, so
// the first import creates it.
func Read(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read the credentials: %w", err)
	}
	config, err := Parse(content)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// Parse reads a config strictly and checks it, so that a misspelled key or
// a context without its key shows up at once. An empty file reads as an
// empty config, like a missing one.
func Parse(content []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, err
	}
	if err := config.check(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) check() error {
	var seen []string
	for _, cliContext := range c.Contexts {
		if err := cliContext.check(); err != nil {
			return err
		}
		if slices.Contains(seen, cliContext.Name) {
			return fmt.Errorf("context %s is there twice", cliContext.Name)
		}
		seen = append(seen, cliContext.Name)
	}
	if c.CurrentContext != "" && !slices.Contains(seen, c.CurrentContext) {
		return fmt.Errorf("current_context %s names no context", c.CurrentContext)
	}
	return nil
}

func (c Context) check() error {
	switch {
	case strings.TrimSpace(c.Name) == "":
		return errors.New("a context has no name")
	case len(c.Endpoints) == 0:
		return fmt.Errorf("context %s: endpoints: at least one address is required", c.Name)
	case c.CA == "" || c.Certificate == "" || c.Key == "":
		return fmt.Errorf("context %s: ca, certificate and key are required", c.Name)
	}
	for _, endpoint := range c.Endpoints {
		if !ValidEndpoint(endpoint) {
			return fmt.Errorf("context %s: endpoint %q is neither an address nor a host name", c.Name, endpoint)
		}
	}
	return nil
}

// ValidEndpoint accepts an IP address or a host name, each with an
// optional port.
func ValidEndpoint(address string) bool {
	host := address
	if splitHost, port, err := net.SplitHostPort(address); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		host = splitHost
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && !strings.ContainsAny(host, " /:")
}

// Context returns the context of that name, or the current one for "".
func (c Config) Context(name string) (Context, error) {
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return Context{}, ErrNoCurrentContext
	}
	index := c.index(name)
	if index < 0 {
		return Context{}, fmt.Errorf("%w: %s", ErrNoContext, name)
	}
	return c.Contexts[index], nil
}

func (c Config) index(name string) int {
	return slices.IndexFunc(c.Contexts, func(cliContext Context) bool { return cliContext.Name == name })
}

// Add adds a context whose name is not taken yet. The first context
// becomes the current one.
func (c *Config) Add(cliContext Context) error {
	if c.index(cliContext.Name) >= 0 {
		return fmt.Errorf("%w: %s", ErrContextTaken, cliContext.Name)
	}
	return c.Put(cliContext)
}

// Put adds a context, or replaces the one of the same name in its position.
// The first context becomes the current one.
func (c *Config) Put(cliContext Context) error {
	if err := cliContext.check(); err != nil {
		return err
	}
	if index := c.index(cliContext.Name); index >= 0 {
		c.Contexts[index] = cliContext
	} else {
		c.Contexts = append(c.Contexts, cliContext)
	}
	if c.CurrentContext == "" {
		c.CurrentContext = cliContext.Name
	}
	return nil
}

// Delete removes the context of that name. If it was the current one, no
// context is current afterwards.
func (c *Config) Delete(name string) error {
	index := c.index(name)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNoContext, name)
	}
	c.Contexts = slices.Delete(c.Contexts, index, index+1)
	if c.CurrentContext == name {
		c.CurrentContext = ""
	}
	return nil
}

// Use makes the context of that name the current one.
func (c *Config) Use(name string) error {
	if _, err := c.Context(name); err != nil {
		return err
	}
	c.CurrentContext = name
	return nil
}

// Write writes the config to path, whole or not at all. Only the user can
// read it: mode 0600 in a directory of 0700, the way SSH keeps its keys.
// On Windows, the user's profile protects it.
func Write(path string, config Config) error {
	if err := config.check(); err != nil {
		return err
	}
	content, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode the credentials: %w", err)
	}
	return atomicfile.Write(path, content, 0o600)
}
