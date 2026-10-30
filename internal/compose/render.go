package compose

import (
	"bytes"
	"fmt"
	"net/netip"
	"path"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/webcodingcat/clowder/internal/names"
)

// Target is what the agent of a host knows when it renders a stack.
type Target struct {
	// Namespace is the stack's namespace: with the stack's name it makes the
	// names on the host (names.Project, names.BridgeName).
	Namespace string
	// FloatingIP and NodeAddress: published ports bind to both, so the
	// Floating IP reaches the container when this host is the ingress host
	// and the node address when another host forwards to it.
	FloatingIP  netip.Addr
	NodeAddress netip.Addr
	// Resolver is the agent's DNS resolver for the containers.
	Resolver netip.Addr
	// Volumes per volume name: the directory where its file system is
	// mounted, and its name on the host.
	Volumes map[string]MountedVolume
	// SecretsDir holds a file per secret the services ask for as a file.
	SecretsDir string
	// SecretDigests per service that asks for secret files: a keyed digest
	// of their values (LabelSecrets). A changed file keeps its path, so
	// without it Compose would not recreate the container; with a key, the
	// label tells nothing about the values to whoever reads it.
	SecretDigests map[string]string
}

// MountedVolume is a volume as it is mounted on the host.
type MountedVolume struct {
	Dir string
	// Name is the Docker volume's, the copy's LV (names.VolumeName): it has
	// the generation, as Docker cannot change the options of an existing
	// volume and a restore or class move makes a new one.
	Name string
}

// Labels that the agent puts on containers: the stack's Compose project
// (names.Project) on every one, the digest of its secret files on those that
// ask for some. Nothing else, so an apply recreates only the containers
// whose configuration or secret files changed.
const (
	LabelStack   = "clowder.stack"
	LabelSecrets = "clowder.secrets"
)

// Render returns the Compose file that docker compose gets on the host.
// Docker never restarts a container itself (restart "no"): after a reboot
// nothing may start before the agent has looked at the cluster; the agent
// applies the stack's restart rules instead.
func Render(stack *Stack, target Target) ([]byte, error) {
	for _, required := range []struct {
		what    string
		address netip.Addr
	}{{"floating IP", target.FloatingIP}, {"node address", target.NodeAddress}, {"resolver", target.Resolver}} {
		if !required.address.IsValid() {
			return nil, fmt.Errorf("render stack %s: the target has no %s", stack.Name, required.what)
		}
	}
	if !names.ValidNamespace(target.Namespace) {
		return nil, fmt.Errorf("render stack %s: the target's namespace %q is not valid", stack.Name, target.Namespace)
	}
	project := *stack.Project
	project.Name = names.Project(target.Namespace, stack.Name)
	project.Services = types.Services{}
	project.Secrets = types.Secrets{}
	for name, service := range stack.Project.Services {
		rendered := renderService(project.Name, service, target)
		if len(stack.Services[name].Secrets) > 0 {
			if target.SecretsDir == "" {
				return nil, fmt.Errorf("render stack %s: the target has no directory for secrets", stack.Name)
			}
			if target.SecretDigests[name] == "" {
				return nil, fmt.Errorf("render stack %s: no digest of the secrets of service %s", stack.Name, name)
			}
		}
		for _, secret := range stack.Services[name].Secrets {
			// Compose's own secrets are refused, so no name is taken
			project.Secrets[secret] = types.SecretConfig{File: path.Join(target.SecretsDir, secret)}
			rendered.Secrets = append(rendered.Secrets, types.ServiceSecretConfig{Source: secret})
		}
		project.Services[name] = rendered
	}
	project.Networks = types.Networks{}
	for name, network := range stack.Project.Networks {
		project.Networks[name] = renderNetwork(target.Namespace, stack.Name, name, network)
	}
	if _, ok := project.Networks["default"]; !ok {
		project.Networks["default"] = renderNetwork(target.Namespace, stack.Name, "default", types.NetworkConfig{})
	}
	project.Volumes = types.Volumes{}
	for name, volume := range stack.Project.Volumes {
		mounted, ok := target.Volumes[name]
		if !ok || mounted.Name == "" {
			return nil, fmt.Errorf("render stack %s: volume %s is not mounted", stack.Name, name)
		}
		project.Volumes[name] = renderVolume(volume, mounted)
	}
	marshaled, err := project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("render stack %s: %w", stack.Name, err)
	}
	rendered, err := EscapeDollars(marshaled)
	if err != nil {
		return nil, fmt.Errorf("render stack %s: %w", stack.Name, err)
	}
	return rendered, nil
}

// RenderSecretEnv returns the second Compose file the stack comes up with:
// only the variables its services set to secrets (x-clowder.secret_env),
// from values (secret name -> value). The agent keeps it on tmpfs, apart
// from the rendered file on disk. Nil if the stack sets none.
func RenderSecretEnv(stack *Stack, values map[string]string) ([]byte, error) {
	services := map[string]any{}
	for name, service := range stack.Services {
		if len(service.SecretEnv) == 0 {
			continue
		}
		environment := map[string]string{}
		for variable, secret := range service.SecretEnv {
			value, ok := values[secret]
			if !ok {
				return nil, fmt.Errorf("render the secrets of stack %s: no value for %s", stack.Name, secret)
			}
			environment[variable] = value
		}
		services[name] = map[string]any{"environment": environment}
	}
	if len(services) == 0 {
		return nil, nil
	}
	marshaled, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return nil, fmt.Errorf("render the secrets of stack %s: %w", stack.Name, err)
	}
	return EscapeDollars(marshaled)
}

// EscapeDollars writes every $ in a value of a Compose file as $$. The
// values arrive interpolated already (Load, on the user's machine), but
// docker compose on the host interpolates the file it gets once more: a $
// left in a value is meant as it stands. Keys are never interpolated.
func EscapeDollars(file []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(file, &document); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	escapeValues(&document)
	var escaped bytes.Buffer
	encoder := yaml.NewEncoder(&escaped)
	encoder.SetIndent(2) // as compose-go writes it
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("escape the $ signs: %w", err)
	}
	return escaped.Bytes(), nil
}

func escapeValues(node *yaml.Node) {
	switch node.Kind {
	case yaml.ScalarNode:
		node.Value = strings.ReplaceAll(node.Value, "$", "$$")
	case yaml.MappingNode:
		for i := 1; i < len(node.Content); i += 2 { // keys at even places
			escapeValues(node.Content[i])
		}
	default:
		for _, child := range node.Content {
			escapeValues(child)
		}
	}
}

func renderService(project string, service types.ServiceConfig, target Target) types.ServiceConfig {
	service.Restart = types.RestartPolicyNo
	service.DNS = types.StringList{target.Resolver.String()}
	ports := []types.ServicePortConfig{}
	for _, port := range service.Ports {
		for _, address := range []netip.Addr{target.FloatingIP, target.NodeAddress} {
			bound := port
			bound.HostIP = address.String()
			ports = append(ports, bound)
		}
	}
	service.Ports = ports
	labels := types.Labels{}
	for key, value := range service.Labels {
		labels[key] = value
	}
	labels[LabelStack] = project
	if digest := target.SecretDigests[service.Name]; digest != "" {
		labels[LabelSecrets] = digest
	}
	service.Labels = labels
	return service
}

// renderNetwork names a stack network after the project, as Compose would
// (the loaded file has it after the stack's name alone, which two
// namespaces share), and gives it a fixed bridge name, so nft can tell which
// stack a packet comes from.
func renderNetwork(namespace, stack, name string, network types.NetworkConfig) types.NetworkConfig {
	network.Name = names.Network(namespace, stack, name)
	options := types.Options{}
	for key, value := range network.DriverOpts {
		options[key] = value
	}
	options["com.docker.network.bridge.name"] = names.BridgeName(names.ID(namespace, stack), name)
	network.DriverOpts = options
	return network
}

// renderVolume binds a named volume to the data/ directory inside the
// mounted file system: data/ exists only while the file system is mounted,
// so a container cannot start on the host's root file system by mistake.
func renderVolume(volume types.VolumeConfig, mounted MountedVolume) types.VolumeConfig {
	volume.Name = mounted.Name
	volume.Driver = "local"
	volume.DriverOpts = types.Options{
		"type":   "none",
		"o":      "bind",
		"device": path.Join(mounted.Dir, "data"),
	}
	return volume
}
