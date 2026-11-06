// Package names holds the rules for the names that clowder takes from
// users, and builds the names on the host from them, so that the CLI, the
// API, the state and the agent agree.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// MaxLength is the length of the longest name, as for a DNS label. The
// names on the host that join several names (Project) have no tight limit.
// Those that do have one are cut or hashed (VolumeName, BridgeName).
const MaxLength = 63

var (
	// name is a DNS label: no "/", which joins a namespace and a name in an
	// ID; no "_", which joins them on the host (Project); and no "..",
	// because a secret's name is a file name on the host.
	name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// variable is an environment variable's name.
	variable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// domain is a domain name of at least two labels, in any case.
	domain = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// Rule says what Valid accepts, for messages.
var Rule = fmt.Sprintf("use lowercase letters, digits and -, at most %d characters, "+
	"starting and ending with a letter or digit", MaxLength)

// Valid tells whether name may name a stack, a service, a volume or a secret.
func Valid(resourceName string) bool {
	return len(resourceName) <= MaxLength && name.MatchString(resourceName)
}

// Default is the namespace that exists from the start. A name without a
// namespace refers to it.
const Default = "default"

// ReservedPrefix is the prefix of the Compose projects that clowder runs
// itself (its Traefik). Every stack's project starts with its namespace, so
// no namespace may start with this prefix.
const ReservedPrefix = "clowder-"

// NamespaceRule says what ValidNamespace accepts, for messages.
var NamespaceRule = Rule + ", and do not start with " + ReservedPrefix

// ValidNamespace tells whether name may name a namespace that users make.
// System, which the cluster makes, is not such a namespace.
func ValidNamespace(namespace string) bool {
	return Valid(namespace) && !strings.HasPrefix(namespace, ReservedPrefix)
}

// System is the namespace of the cluster's own Secrets (the Hetzner token,
// the alert channels' URLs and passwords), like Kubernetes' kube-system.
// It always exists, cannot be deleted and holds no stacks or volumes. It
// starts with ReservedPrefix, so no namespace that users make can have
// this name, and no stack's Compose project can clash with clowder's own.
const System = ReservedPrefix + "system"

// MaxSecretKeyLength is the length of the longest key of a secret, as in
// Kubernetes.
const MaxSecretKeyLength = 253

// secretKey is a key of a secret, as Kubernetes allows it. It is a file
// name in the container (/run/secrets/<secret>/<key>).
var secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// SecretKeyRule says what ValidSecretKey accepts, for messages.
var SecretKeyRule = fmt.Sprintf("use letters, digits, -, _ and ., at most %d characters, not . or ..",
	MaxSecretKeyLength)

// ValidSecretKey tells whether key may name a value of a secret.
func ValidSecretKey(key string) bool {
	return len(key) <= MaxSecretKeyLength && secretKey.MatchString(key) && key != "." && key != ".."
}

// SecretKeyRef names one value of a secret: the secret, in the namespace
// of the resource that names it, and its key, like Kubernetes'
// secretKeyRef.
type SecretKeyRef struct {
	Name string `yaml:"name" json:"name"`
	Key  string `yaml:"key" json:"key"`
}

// String returns the name of the value as messages use it:
// "hetzner[token]".
func (r SecretKeyRef) String() string {
	return r.Name + "[" + r.Key + "]"
}

// Problem tells what is wrong with the reference on its own, or "".
func (r SecretKeyRef) Problem() string {
	switch {
	case !Valid(r.Name):
		return fmt.Sprintf("secret name %q: %s", r.Name, Rule)
	case !ValidSecretKey(r.Key):
		return fmt.Sprintf("key %q of secret %s: %s", r.Key, r.Name, SecretKeyRule)
	}
	return ""
}

// ID returns the name that the state and the users use for a resource in a
// namespace: "<namespace>/<name>".
func ID(namespace, resourceName string) string {
	return namespace + "/" + resourceName
}

// SplitID is the reverse of ID. ok is false for anything that ID does not
// make from a valid namespace and name.
func SplitID(id string) (namespace, resourceName string, ok bool) {
	namespace, resourceName, ok = strings.Cut(id, "/")
	return namespace, resourceName, ok && ValidNamespace(namespace) && Valid(resourceName)
}

// Project returns a stack's Compose project, which is also the name of
// everything the agent keeps per stack (its directory, its secrets, the
// supervisor's hold, its Traefik services). No name has "_", so "_"
// separates namespace and stack.
func Project(namespace, stack string) string {
	return namespace + "_" + stack
}

// ClowdersOwn tells whether a Compose project is one that clowder runs
// itself (its Traefik), not a stack's. Every place that treats such a
// project differently asks it.
func ClowdersOwn(project string) bool {
	return strings.HasPrefix(project, ReservedPrefix)
}

// SplitProject is the reverse of Project. ok is false for anything that
// Project does not make from a valid namespace and stack (clowder's own
// projects).
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

// FileName returns the file name of a record per ID ("shop/data" ->
// "shop_data"). No name has "_", so the file name is unique.
func FileName(id string) string {
	return strings.ReplaceAll(id, "/", "_")
}

// The parts of a volume's LV name and their limits. LVM and DRBD accept
// names of up to 127 characters, and device-mapper accepts 128 for
// "<vg>-<lv>", in which it writes every "-" twice. Two names of MaxLength
// full of "-" would break that limit. So the readable part is cut, and the
// hash keeps the name unique.
const (
	volumeReadable = 24
	// MaxDataGeneration is the highest generation of a volume's data: at
	// most six digits in the LV name.
	MaxDataGeneration = 999999
	// MaxVolumeGroupLength is the length of the longest volume group of a
	// storage class's pool, the other part of the device-mapper name ("vg-"
	// and a class of 16 characters, as examples/hetzner names them).
	MaxVolumeGroupLength = 19
)

// VolumeName returns the name of the LV, DRBD resource, mount point and
// Docker volume of a volume's data generation: "clw-", the readable ID cut
// to length, the first 8 hex digits of its hash ("a-b/c" and "a/b-c" read
// the same), and the generation. The prefix marks clowder's LVs and keeps
// LVM's own names out.
func VolumeName(id string, generation int64) string {
	readable := strings.ReplaceAll(id, "/", "-")
	if len(readable) > volumeReadable {
		readable = readable[:volumeReadable]
	}
	return fmt.Sprintf("clw-%s-%s-g%d", readable, shortHash(id, 8), generation)
}

// BridgeName returns the Linux bridge of a stack's one network: "cbr-" and
// the first 11 hex digits of a hash of the stack's ID, within the 15
// characters that an interface name may have.
func BridgeName(stackID string) string {
	return "cbr-" + shortHash(stackID, 11)
}

// shortHash returns the first hex digits of the SHA-256 of key.
func shortHash(key string, digits int) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:digits]
}

// ValidDomain tells whether name is a domain name of at least two labels,
// as needed for the host of an HTTP route and of its certificate.
func ValidDomain(domainName string) bool {
	return domain.MatchString(domainName)
}

// ValidVariable tells whether name may name an environment variable.
func ValidVariable(variableName string) bool {
	return variable.MatchString(variableName)
}
