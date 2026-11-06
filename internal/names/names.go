// Package names holds the rules for the names that users give. It also
// builds the names on the host from them. Keeping both in one place makes
// the CLI, the API, the state and the agent agree.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// MaxLength is the length of the longest name, as for a DNS label. Some
// names on the host join several names. Most of them have no tight limit
// (Project). Those that do are cut or hashed (VolumeName, BridgeName).
const MaxLength = 63

var (
	// name is a DNS label. It has no "/", which joins a namespace and a
	// name in an ID, and no "_", which joins them on the host (Project).
	// It has no "..", because a secret's name is a file name on the host.
	name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// variable is an environment variable's name.
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
// namespace, so no namespace may start with this prefix.
const ReservedPrefix = "clowder-"

// NamespaceRule says in messages what ValidNamespace accepts.
var NamespaceRule = Rule + ", and do not start with " + ReservedPrefix

// ValidNamespace tells whether namespace may name a namespace that users
// make. It refuses System, which the cluster makes.
func ValidNamespace(namespace string) bool {
	return Valid(namespace) && !strings.HasPrefix(namespace, ReservedPrefix)
}

// System is the namespace of the cluster's own Secrets (the Hetzner token,
// the alert channels' URLs and passwords), like Kubernetes' kube-system.
// It always exists, cannot be deleted and holds no stacks or volumes. Its
// name starts with ReservedPrefix. So users cannot make a namespace of
// this name, and no stack's Compose project can clash with clowder's own.
const System = ReservedPrefix + "system"

// MaxSecretKeyLength is the length of the longest key of a secret, as in
// Kubernetes.
const MaxSecretKeyLength = 253

// secretKey is a key of a secret, as Kubernetes allows it. It is a file
// name in the container (/run/secrets/<secret>/<key>).
var secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// SecretKeyRule says in messages what ValidSecretKey accepts.
var SecretKeyRule = fmt.Sprintf("use letters, digits, -, _ and ., at most %d characters, not . or ..",
	MaxSecretKeyLength)

// ValidSecretKey tells whether key may name a value of a secret.
func ValidSecretKey(key string) bool {
	return len(key) <= MaxSecretKeyLength && secretKey.MatchString(key) && key != "." && key != ".."
}

// SecretKeyRef names one value of a secret, like Kubernetes' secretKeyRef:
// the secret's name and the value's key. The secret is in the namespace of
// the resource that holds the reference.
type SecretKeyRef struct {
	Name string `yaml:"name" json:"name"`
	Key  string `yaml:"key" json:"key"`
}

// String names the value as messages show it: "hetzner[token]".
func (r SecretKeyRef) String() string {
	return r.Name + "[" + r.Key + "]"
}

// Problem tells what is wrong with the reference itself, or returns "".
// It does not check that the secret exists.
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

// Project returns a stack's Compose project. It also names everything the
// agent keeps per stack: its directory, its secrets, the supervisor's hold
// and its Traefik services. No name has "_", so "_" can separate the
// namespace from the stack.
func Project(namespace, stack string) string {
	return namespace + "_" + stack
}

// ClowdersOwn tells whether clowder runs a Compose project itself (its
// Traefik), so that it is not a stack's. Every place that treats such a
// project differently calls it.
func ClowdersOwn(project string) bool {
	return strings.HasPrefix(project, ReservedPrefix)
}

// SplitProject is the reverse of Project. ok is false unless Project could
// have made project from a valid namespace and stack. So it is false for
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

// The parts of a volume's LV name and their limits. LVM and DRBD accept
// names of up to 127 characters. device-mapper accepts 128 for
// "<vg>-<lv>", and it writes every "-" in them twice. Two names of
// MaxLength full of "-" would break that limit. So VolumeName cuts the
// readable part, and a hash keeps the name unique.
const (
	volumeReadable = 24
	// MaxDataGeneration is the highest generation of a volume's data, so
	// that it takes at most six digits in the LV name.
	MaxDataGeneration = 999999
	// MaxVolumeGroupLength is the length of the longest volume group of a
	// storage class's pool. The volume group is the other part of the
	// device-mapper name. examples/hetzner names it "vg-" and the class,
	// so a class may have 16 characters.
	MaxVolumeGroupLength = 19
)

// VolumeName returns the name of the LV, the DRBD resource, the mount
// point and the Docker volume of a volume's data generation. It joins
// "clw-", the readable ID cut to length, the first 8 hex digits of the
// ID's hash, and the generation. The hash is needed because "a-b/c" and
// "a/b-c" have the same readable part. The prefix marks clowder's LVs and
// keeps them apart from LVM's own names.
func VolumeName(id string, generation int64) string {
	readable := strings.ReplaceAll(id, "/", "-")
	if len(readable) > volumeReadable {
		readable = readable[:volumeReadable]
	}
	return fmt.Sprintf("clw-%s-%s-g%d", readable, shortHash(id, 8), generation)
}

// BridgeName returns the Linux bridge of a stack's one network: "cbr-" and
// the first 11 hex digits of a hash of the stack's ID. This fits the 15
// characters that an interface name may have.
func BridgeName(stackID string) string {
	return "cbr-" + shortHash(stackID, 11)
}

// shortHash returns the first hex digits of the SHA-256 of key.
func shortHash(key string, digits int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:digits]
}

// ValidDomain tells whether domainName is a domain name of at least two
// labels. An HTTP route's host and its certificate need such a name.
func ValidDomain(domainName string) bool {
	return domain.MatchString(domainName)
}

// ValidVariable tells whether variableName may name an environment
// variable.
func ValidVariable(variableName string) bool {
	return variable.MatchString(variableName)
}
