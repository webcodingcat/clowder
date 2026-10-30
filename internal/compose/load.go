package compose

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/units"
)

// LoadOptions says where a stack's Compose files are.
type LoadOptions struct {
	// Name is the stack the files are for: the first file must say so in
	// its name: (the Compose project name), and no other file may set one.
	Name string
	// WorkingDir is where relative paths (env_file, extends) start.
	WorkingDir string
	// Files are the Compose files, later ones override earlier ones.
	Files []string
	// Environment is used for ${VARIABLE} interpolation.
	Environment map[string]string
}

// Problems lists everything clowder cannot run in a stack, so the user sees
// all of it at once.
type Problems []string

func (p Problems) Error() string {
	return "the stack cannot run on clowder:\n  - " + strings.Join(p, "\n  - ")
}

// Load reads the Compose files, checks them and returns the stack. A stack
// that clowder cannot run gives Problems.
func Load(ctx context.Context, options LoadOptions) (*Stack, error) {
	if err := CheckNames(options.Name, options.Files); err != nil {
		return nil, err
	}
	details := types.ConfigDetails{
		WorkingDir:  options.WorkingDir,
		Environment: options.Environment,
	}
	for _, file := range options.Files {
		details.ConfigFiles = append(details.ConfigFiles, types.ConfigFile{Filename: file})
	}
	project, err := loader.LoadWithContext(ctx, details)
	if err != nil {
		return nil, fmt.Errorf("read the compose files: %w", err)
	}
	if project.Name != options.Name {
		return nil, fmt.Errorf("the compose files are for stack %q, not %q", project.Name, options.Name)
	}
	stack, err := check(project)
	if err != nil {
		return nil, err
	}
	if len(stack.SecretNames()) > 0 && stack.SecretsFile == "" {
		return nil, Problems{"stack: its services ask for secrets, but x-clowder.secrets_file names no file"}
	}
	return stack, nil
}

// CheckNames binds the files to their stack: the first says its name as
// written (before compose-go fills in or normalizes anything), so a file
// for one stack is never applied to another; a later file may not change it.
func CheckNames(stack string, files []string) error {
	for index, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		var header struct {
			Name string `yaml:"name"`
		}
		if err := yaml.Unmarshal(content, &header); err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		switch {
		case index == 0 && header.Name == "":
			return fmt.Errorf("%s has no name:; the stack's first file says which stack it is (name: %s)", file, stack)
		case index == 0 && header.Name != stack:
			return fmt.Errorf("%s is the file of stack %q, not of %q", file, header.Name, stack)
		case index > 0 && header.Name != "":
			return fmt.Errorf("%s sets name:; only the stack's first file does", file)
		}
	}
	return nil
}

// check turns a loaded project into a Stack, or lists its problems.
func check(project *types.Project) (*Stack, error) {
	stack := &Stack{
		Name:     project.Name,
		Services: map[string]Service{},
		Volumes:  map[string]Volume{},
	}
	problems := readStackExtension(project.Extensions, stack)
	for _, name := range slices.Sorted(maps.Keys(project.Volumes)) {
		volume, volumeProblems := checkVolume(name, project.Volumes[name])
		problems = append(problems, volumeProblems...)
		stack.Volumes[name] = volume
	}
	problems = append(problems, checkNetworks(project.Networks)...)
	problems = append(problems, checkSecretsAndConfigs(project)...)
	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service, serviceProblems := checkService(project.Services[name], project.Volumes)
		problems = append(problems, serviceProblems...)
		stack.Services[name] = service
	}
	problems = append(problems, checkUnique(stack.Services)...)
	problems = append(problems, checkReferences(stack)...)
	if len(problems) > 0 {
		return nil, problems
	}
	stack.Project = withoutExtensions(project)
	stack.source = project
	return stack, nil
}

// readStackExtension reads the stack's own x-clowder block.
func readStackExtension(extensions types.Extensions, stack *Stack) Problems {
	var problems Problems
	var raw rawStackExtension
	if err := decodeExtension(extensions, &raw); err != nil {
		problems = append(problems, fmt.Sprintf("stack: %v", err))
	}
	priority, err := parsePriority(raw.Priority)
	if err != nil {
		problems = append(problems, fmt.Sprintf("stack: %v", err))
	}
	stack.Priority = priority
	stack.Exports = raw.Exports
	stack.Imports = raw.Imports
	stack.SecretsFile = raw.SecretsFile
	return problems
}

// checkNetworks refuses what clowder sets itself: Docker's bridges with
// their address pools, one per stack network.
func checkNetworks(networks types.Networks) Problems {
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, name := range slices.Sorted(maps.Keys(networks)) {
		network := networks[name]
		if !names.Valid(name) {
			// it ends up in the network's name on the host
			report("network %s: the name is not a network name (%s)", name, names.Rule)
		}
		if network.External {
			report("network %s: external networks are not supported", name)
		}
		if network.Driver != "" && network.Driver != "bridge" {
			report("network %s: driver %q is not supported (clowder uses bridges)", name, network.Driver)
		}
		if len(network.Ipam.Config) > 0 {
			report("network %s: ipam subnets are not supported (Docker's address pools assign them)", name)
		}
		if _, ok := network.Extensions[extensionKey]; ok {
			report("network %s: x-clowder is not used on networks", name)
		}
	}
	return problems
}

// checkSecretsAndConfigs refuses Compose's secrets and configs that come
// from a file or a variable: a file named here lives on this machine, not on
// the host that runs the stack, and a variable would be read from the
// agent's environment there. Secrets come with x-clowder.secrets instead.
func checkSecretsAndConfigs(project *types.Project) Problems {
	var problems Problems
	for _, name := range slices.Sorted(maps.Keys(project.Secrets)) {
		problems = append(problems, fmt.Sprintf(
			"secret %s: Compose's secrets are not supported; a service asks with x-clowder.secrets", name))
	}
	for _, name := range slices.Sorted(maps.Keys(project.Configs)) {
		config := project.Configs[name]
		if config.File != "" || config.Environment != "" {
			problems = append(problems, fmt.Sprintf("config %s: file and environment are not supported; use content", name))
		}
	}
	return problems
}

// checkReferences checks the names the stack's x-clowder fields refer to:
// exported services, imports, the services of backup hooks.
func checkReferences(stack *Stack) Problems {
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	for _, export := range stack.Exports {
		if _, ok := stack.Services[export]; !ok {
			report("stack: exports %q, which is not a service of this stack", export)
		}
	}
	for _, imported := range stack.Imports {
		if stackName, service, ok := strings.Cut(imported, "/"); !ok || stackName == "" || service == "" {
			report("stack: import %q is not of the form stack/service", imported)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(stack.Volumes)) {
		volume := stack.Volumes[name]
		if volume.Backup != nil && volume.Backup.Pre != nil {
			if _, ok := stack.Services[volume.Backup.Pre.Service]; !ok {
				report("volume %s: the backup hook runs in %q, which is not a service of this stack",
					volume.Name, volume.Backup.Pre.Service)
			}
		}
	}
	return problems
}

// Source is the stack as one self-contained Compose file: variables filled
// in, env files merged into the environment, x-clowder kept. The CLI sends
// it, the cluster stores it, and Parse reads it again on the leader and on
// the stack's host, where the user's files are not.
func (s *Stack) Source() ([]byte, error) {
	project := *s.source
	// the secrets file is the user's path; its values travel apart
	if stackExtension, ok := project.Extensions[extensionKey].(map[string]any); ok {
		project.Extensions = maps.Clone(project.Extensions)
		stackExtension = maps.Clone(stackExtension)
		delete(stackExtension, "secrets_file")
		project.Extensions[extensionKey] = stackExtension
		if len(stackExtension) == 0 {
			delete(project.Extensions, extensionKey)
		}
	}
	project.Services = types.Services{}
	for name, service := range s.source.Services {
		// the environment holds them already; their paths are the user's
		service.EnvFiles = nil
		project.Services[name] = service
	}
	encoded, err := project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("write the stack %s: %w", s.Name, err)
	}
	return encoded, nil
}

// Parse reads a stack from its Source and checks it again. It fills in no
// variables: Source did, and a "$" in a value is meant as it stands.
func Parse(ctx context.Context, name string, source []byte) (*Stack, error) {
	details := types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: source}},
	}
	project, err := loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SkipInterpolation = true
	})
	if err != nil {
		return nil, fmt.Errorf("read the stack %s: %w", name, err)
	}
	if project.Name != name {
		return nil, fmt.Errorf("the Compose file is for stack %q, not %q", project.Name, name)
	}
	return check(project)
}

// checkVolume reads the clowder fields of a named volume.
func checkVolume(name string, config types.VolumeConfig) (Volume, Problems) {
	volume := Volume{Name: name}
	var problems Problems
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("volume %s: ", name)+fmt.Sprintf(format, args...))
	}
	if !names.Valid(name) {
		report("the name is not a volume name (%s)", names.Rule)
	}
	if config.External {
		report("external volumes are not supported")
	}
	if config.Driver != "" || len(config.DriverOpts) > 0 {
		report("driver and driver_opts are set by clowder")
	}
	var raw rawVolumeExtension
	if err := decodeExtension(config.Extensions, &raw); err != nil {
		report("%v", err)
		return volume, problems
	}
	if raw.Class == "" {
		report("x-clowder.class is required (a storage class of the cluster, for example fast)")
	}
	volume.Class = raw.Class
	if raw.Size == "" {
		report("x-clowder.size is required (for example 10G)")
	} else if size, err := units.ParseSize(raw.Size); err != nil {
		report("%v", err)
	} else {
		volume.Size = size
	}
	switch {
	case raw.Replicas == nil:
		report("x-clowder.replicas is required (0 to 3)")
	case *raw.Replicas < 0 || *raw.Replicas > 3:
		report("x-clowder.replicas %d is not from 0 to 3", *raw.Replicas)
	default:
		volume.Replicas = *raw.Replicas
	}
	if raw.Backup != nil {
		if raw.Backup.Schedule == "" {
			report("x-clowder.backup.schedule is required when backup is set")
		}
		if raw.Replicas != nil && *raw.Replicas == 0 {
			report("a volume with replicas 0 is never backed up; remove x-clowder.backup")
		}
		if raw.Backup.Keep < 0 {
			report("x-clowder.backup.keep %d is negative", raw.Backup.Keep)
		}
		if hook := raw.Backup.Pre; hook != nil && (hook.Service == "" || len(hook.Command) == 0) {
			report("x-clowder.backup.pre needs service and command")
		}
		volume.Backup = &Backup{Schedule: raw.Backup.Schedule, Keep: raw.Backup.Keep}
		if raw.Backup.Pre != nil {
			volume.Backup.Pre = &BackupHook{Service: raw.Backup.Pre.Service, Command: raw.Backup.Pre.Command}
		}
	}
	return volume, problems
}

// withoutExtensions returns a copy of the project without x-clowder fields.
func withoutExtensions(project *types.Project) *types.Project {
	clean := *project
	clean.Extensions = without(project.Extensions)
	clean.Services = types.Services{}
	for name, service := range project.Services {
		service.Extensions = without(service.Extensions)
		clean.Services[name] = service
	}
	clean.Volumes = types.Volumes{}
	for name, volume := range project.Volumes {
		volume.Extensions = without(volume.Extensions)
		clean.Volumes[name] = volume
	}
	return &clean
}

// without returns extensions without x-clowder.
func without(extensions types.Extensions) types.Extensions {
	if _, ok := extensions[extensionKey]; !ok {
		return extensions
	}
	clean := maps.Clone(extensions)
	delete(clean, extensionKey)
	return clean
}
