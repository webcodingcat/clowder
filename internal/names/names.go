// Package names holds the rules for the names that users give, and builds
// the names on the host from them, so that the CLI, the API, the state and
// the agent agree.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// MaxLength is the length of the longest name, as for a DNS label. Names
// on the host with a tight limit are cut or hashed (VolumeName,
// BridgeName).
const MaxLength = 63

var (
	// name is a DNS label. It has no "/", which joins a namespace and a
	// name in an ID, no "_", which joins them on the host (Project), and
	// no ".", because a secret's name is a file name on the host.
	name     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	variable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// domain is a domain name of at least two labels, in any case.
	domain = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// Rule says in messages what Valid accepts.
var Rule = fmt.Sprintf("use lowercase letters, digits and -, at most %d characters, "+
	"starting and ending with a letter or digit", MaxLength)

// Valid tells whether resourceName may name a stack, a service, a volume
// or a secret.
func Valid(resourceName string) bool {
	return len(resourceName) <= MaxLength && name.MatchString(resourceName)
}

// Default is the namespace that exists from the start. A name without a
// namespace refers to it.
const Default = "default"

// ReservedPrefix starts the names of the Compose projects that clowder
// runs itself (its Traefik). Every stack's project starts with its
// namespace, so no namespace that users make may start with it.
const ReservedPrefix = "clowder-"

// NamespaceRule says in messages what ValidNamespace accepts.
var NamespaceRule = Rule + ", and do not start with " + ReservedPrefix

// ValidNamespace tells whether namespace may name a namespace that users
// make. It refuses System, which the cluster makes.
func ValidNamespace(namespace string) bool {
	return Valid(namespace) && !strings.HasPrefix(namespace, ReservedPrefix)
}

// System is the namespace of the cluster's own Secrets, like Kubernetes'
// kube-system. It starts with ReservedPrefix, so that users cannot make a
// namespace of this name.
const System = ReservedPrefix + "system"

// MaxSecretKeyLength is the length of the longest key of a secret, as in
// Kubernetes.
const MaxSecretKeyLength = 253

// secretKey is a key of a secret, as Kubernetes allows it. It is a file
// name in the container, so ValidSecretKey also refuses "." and "..".
var secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// SecretKeyRule says in messages what ValidSecretKey accepts.
var SecretKeyRule = fmt.Sprintf("use letters, digits, -, _ and ., at most %d characters, not . or ..",
	MaxSecretKeyLength)

// ValidSecretKey tells whether key may name a value of a secret.
func ValidSecretKey(key string) bool {
	return len(key) <= MaxSecretKeyLength && secretKey.MatchString(key) && key != "." && key != ".."
}

// SecretKeyRef names one value of a secret, like Kubernetes' secretKeyRef.
// The secret is in the namespace of the resource that holds the reference.
type SecretKeyRef struct {
	Name string `yaml:"name" json:"name"`
	Key  string `yaml:"key" json:"key"`
}

// String names the value as messages show it: "hetzner[token]".
func (r SecretKeyRef) String() string {
	return r.Name + "[" + r.Key + "]"
}

// Problem tells what is wrong with the reference itself, or returns "".
func (r SecretKeyRef) Problem() string {
	switch {
	case !Valid(r.Name):
		return fmt.Sprintf("secret name %q: %s", r.Name, Rule)
	case !ValidSecretKey(r.Key):
		return fmt.Sprintf("key %q of secret %s: %s", r.Key, r.Name, SecretKeyRule)
	}
	return ""
}

// ID returns the name by which the state and the users know a resource in
// a namespace: "<namespace>/<name>".
func ID(namespace, resourceName string) string {
	return namespace + "/" + resourceName
}

// SplitID is the reverse of ID. ok is false unless ID could have made id
// from a valid namespace and name.
func SplitID(id string) (namespace, resourceName string, ok bool) {
	namespace, resourceName, ok = strings.Cut(id, "/")
	return namespace, resourceName, ok && ValidNamespace(namespace) && Valid(resourceName)
}

// Project returns a stack's Compose project, which also names everything
// the agent keeps per stack. No name has "_", so "_" separates the
// namespace from the stack unambiguously.
func Project(namespace, stack string) string {
	return namespace + "_" + stack
}

// ClowdersOwn tells whether clowder runs a Compose project itself (its
// Traefik). Every place that treats such a project differently calls it.
func ClowdersOwn(project string) bool {
	return strings.HasPrefix(project, ReservedPrefix)
}

// SplitProject is the reverse of Project. ok is false unless Project could
// have made project from a valid namespace and stack, so it is false for
// clowder's own projects.
func SplitProject(project string) (namespace, stack string, ok bool) {
	namespace, stack, ok = strings.Cut(project, "_")
	return namespace, stack, ok && ValidNamespace(namespace) && Valid(stack)
}

// StackNetwork returns the one Docker network of a stack: "<project>_net".
func StackNetwork(namespace, stack string) string {
	return Project(namespace, stack) + "_net"
}

// Container returns the Docker container of a service:
// "<project>_<service>", without Compose's replica number, because a
// service has one container.
func Container(namespace, stack, service string) string {
	return Project(namespace, stack) + "_" + service
}

// FileName returns the file name of a record that is kept per ID
// ("shop/data" -> "shop_data"). No name has "_", so the file name is
// unique.
func FileName(id string) string {
	return strings.ReplaceAll(id, "/", "_")
}

// The parts of a volume's LV name and their limits. device-mapper accepts
// 128 characters for "<vg>-<lv>" and writes every "-" in them twice, so
// VolumeName cuts the readable part (specs.md has the arithmetic).
const (
	volumeReadable = 24
	// MaxDataGeneration keeps the generation to six digits in the LV name.
	MaxDataGeneration = 999999
	// MaxVolumeGroupLength is the length of the longest volume group of a
	// storage class's pool. examples/hetzner names it "vg-" and the class,
	// so a class may have 16 characters.
	MaxVolumeGroupLength = 19
)

// VolumeName returns the name of the LV, the DRBD resource, the mount
// point and the Docker volume of a volume's data generation. The hash
// keeps "a-b/c" and "a/b-c" apart, and the prefix "clw-" keeps clowder's
// LVs apart from LVM's own names.
func VolumeName(id string, generation int64) string {
	readable := strings.ReplaceAll(id, "/", "-")
	if len(readable) > volumeReadable {
		readable = readable[:volumeReadable]
	}
	return fmt.Sprintf("clw-%s-%s-g%d", readable, shortHash(id, 8), generation)
}

// BridgeName returns the Linux bridge of a stack's one network, within the
// 15 characters that an interface name may have.
func BridgeName(stackID string) string {
	return "cbr-" + shortHash(stackID, 11)
}

func shortHash(key string, digits int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:digits]
}

// ValidDomain tells whether domainName is a domain name of at least two
// labels, as an HTTP route's host and its certificate need.
func ValidDomain(domainName string) bool {
	return domain.MatchString(domainName)
}

// ValidVariable tells whether variableName may name an environment
// variable.
func ValidVariable(variableName string) bool {
	return variable.MatchString(variableName)
}
