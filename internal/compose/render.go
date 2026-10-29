package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"
)

// Target is what the agent of a host knows when it renders a stack.
type Target struct {
	// Generation of the stack; it goes into the labels.
	Generation int64
	// FloatingIP and NodeAddress: published ports bind to both, so the
	// Floating IP reaches the container when this host is the ingress host
	// and the node address when another host forwards to it.
	FloatingIP  netip.Addr
	NodeAddress netip.Addr
	// Resolver is the agent's DNS resolver for the containers.
	Resolver netip.Addr
	// Volumes per volume name: the directory where its file system is
	// mounted, and its generation.
	Volumes map[string]MountedVolume
}

// MountedVolume is a volume as it is mounted on the host.
type MountedVolume struct {
	Dir        string
	Generation int64
}

// Labels that the agent puts on every container.
const (
	LabelStack      = "clowder.stack"
	LabelGeneration = "clowder.generation"
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
	project := *stack.Project
	project.Services = types.Services{}
	for name, service := range stack.Project.Services {
		project.Services[name] = renderService(stack.Name, service, target)
	}
	project.Networks = types.Networks{}
	for name, network := range stack.Project.Networks {
		project.Networks[name] = renderNetwork(stack.Name, name, network)
	}
	if _, ok := project.Networks["default"]; !ok {
		project.Networks["default"] = renderNetwork(stack.Name, "default", types.NetworkConfig{})
	}
	project.Volumes = types.Volumes{}
	for name, volume := range stack.Project.Volumes {
		mounted, ok := target.Volumes[name]
		if !ok {
			return nil, fmt.Errorf("render stack %s: volume %s is not mounted", stack.Name, name)
		}
		project.Volumes[name] = renderVolume(stack.Name, name, volume, mounted)
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

func renderService(stackName string, service types.ServiceConfig, target Target) types.ServiceConfig {
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
	labels[LabelStack] = stackName
	labels[LabelGeneration] = strconv.FormatInt(target.Generation, 10)
	service.Labels = labels
	return service
}

// renderNetwork gives a stack network a fixed bridge name, so nft can tell
// which stack a packet comes from.
func renderNetwork(stackName, name string, network types.NetworkConfig) types.NetworkConfig {
	options := types.Options{}
	for key, value := range network.DriverOpts {
		options[key] = value
	}
	options["com.docker.network.bridge.name"] = BridgeName(stackName, name)
	network.DriverOpts = options
	return network
}

// renderVolume binds a named volume to the data/ directory inside the
// mounted file system: data/ exists only while the file system is mounted,
// so a container cannot start on the host's root file system by mistake.
// The generation is in the name because Docker cannot change the options of
// an existing volume (a restore or class move makes a new one).
func renderVolume(stackName, name string, volume types.VolumeConfig, mounted MountedVolume) types.VolumeConfig {
	volume.Name = VolumeName(stackName, name, mounted.Generation)
	volume.Driver = "local"
	volume.DriverOpts = types.Options{
		"type":   "none",
		"o":      "bind",
		"device": path.Join(mounted.Dir, "data"),
	}
	return volume
}

// VolumeName is the Docker volume of a stack volume. Stack and volume names
// may contain "-", so a hash of both keeps "a-b" + "c" apart from "a" + "b-c";
// the names stay in front for people reading docker volume ls.
func VolumeName(stackName, volume string, generation int64) string {
	return fmt.Sprintf("clw-%s-%s-%s-g%d", stackName, volume, shortHash(stackName+"/"+volume, 8), generation)
}

// BridgeName is the Linux bridge of a stack network: "cbr-" and the first 11
// hex digits of a hash, within the 15 characters an interface name may have.
// The default network hashes the stack name alone.
func BridgeName(stackName, network string) string {
	key := stackName
	if network != "default" {
		key += "/" + network
	}
	return "cbr-" + shortHash(key, 11)
}

// shortHash is the first hex digits of the SHA-256 of key.
func shortHash(key string, digits int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:digits]
}
