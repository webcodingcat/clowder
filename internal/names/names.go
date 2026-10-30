// Package names holds the rules for the names clowder takes from users, and
// builds the names on the host from them, so the CLI, the API, cluster.yaml,
// the state and the agent agree.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// MaxLength is the longest name, as a DNS label. The names on the host that
// join several (Project) have no tight limit; those that have one are cut
// or hashed (VolumeName, BridgeName).
const MaxLength = 63

var (
	// name is a DNS label: no "/", which joins a namespace and a name in an
	// ID, no "_", which joins them on the host (Project), and no "..": a
	// secret's name is a file name on the host.
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

// Default is the namespace that exists from the start; a name without a
// namespace means it.
const Default = "default"

// ReservedPrefix starts the Compose projects clowder runs itself (its
// Traefik). Every stack's project starts with its namespace, so no namespace
// may start with it.
const ReservedPrefix = "clowder-"

// NamespaceRule says what ValidNamespace accepts, for messages.
var NamespaceRule = Rule + ", and do not start with " + ReservedPrefix

// ValidNamespace tells whether name may name a namespace.
func ValidNamespace(namespace string) bool {
	return Valid(namespace) && !strings.HasPrefix(namespace, ReservedPrefix)
}

// ID is how the state and the users name a resource in a namespace:
// "<namespace>/<name>".
func ID(namespace, resourceName string) string {
	return namespace + "/" + resourceName
}

// SplitID is the reverse of ID; ok is false for anything ID does not make of
// a valid namespace and name.
func SplitID(id string) (namespace, resourceName string, ok bool) {
	namespace, resourceName, ok = strings.Cut(id, "/")
	return namespace, resourceName, ok && ValidNamespace(namespace) && Valid(resourceName)
}

// Project is a stack's Compose project, and the name of everything the agent
// keeps per stack (its directory, its secrets, the supervisor's hold, its
// Traefik services): no name has "_", so it separates namespace and stack.
func Project(namespace, stack string) string {
	return namespace + "_" + stack
}

// SplitProject is the reverse of Project; ok is false for anything Project
// does not make of a valid namespace and stack (clowder's own projects).
func SplitProject(project string) (namespace, stack string, ok bool) {
	namespace, stack, ok = strings.Cut(project, "_")
	return namespace, stack, ok && ValidNamespace(namespace) && Valid(stack)
}

// Network is the Docker network of a stack: "<project>_<network>", as Compose
// would name it, but with the namespace in the project.
func Network(namespace, stack, network string) string {
	return Project(namespace, stack) + "_" + network
}

// VolumeID is the ID of a stack's volume: "<namespace>/<stack>/<volume>".
func VolumeID(namespace, stack, volume string) string {
	return ID(namespace, stack) + "/" + volume
}

// StackOfVolume is the ID of the stack a volume ID belongs to; ok is false
// for anything VolumeID does not make of valid names.
func StackOfVolume(volumeID string) (stackID string, ok bool) {
	index := strings.LastIndex(volumeID, "/")
	if index < 0 {
		return "", false
	}
	stackID = volumeID[:index]
	_, _, ok = SplitID(stackID)
	return stackID, ok && Valid(volumeID[index+1:])
}

// FileName is the file name of a record per ID ("shop/web/data" ->
// "shop_web_data"): no name has "_", so it is unique.
func FileName(id string) string {
	return strings.ReplaceAll(id, "/", "_")
}

// The parts of a volume's LV name and their limits. LVM and DRBD take
// names of 127 characters, device-mapper 128 for "<vg>-<lv>", in which it
// writes every "-" twice: two names of MaxLength full of "-" would break that.
// So the readable part is cut, and the hash keeps the name unique.
const (
	volumeReadable = 24
	// MaxDataGeneration is the highest generation of a volume's data: six
	// digits at most in the LV name.
	MaxDataGeneration = 999999
	// MaxVolumeGroupLength is the longest volume group of a storage class's
	// pool: the other part of the device-mapper name ("vg-" and a class of 16
	// characters, as examples/hetzner names them).
	MaxVolumeGroupLength = 19
)

// VolumeName is the LV, DRBD resource, mount point and Docker volume of a
// volume's data generation: "clw-", the ID readable and cut, the first 8 hex
// digits of its hash ("a-b/c" and "a/b-c" read alike), and the generation.
// The prefix marks clowder's LVs and keeps LVM's own names out.
func VolumeName(id string, generation int64) string {
	readable := strings.ReplaceAll(id, "/", "-")
	if len(readable) > volumeReadable {
		readable = readable[:volumeReadable]
	}
	return fmt.Sprintf("clw-%s-%s-g%d", readable, shortHash(id, 8), generation)
}

// BridgeName is the Linux bridge of a stack's network: "cbr-" and the first
// 11 hex digits of a hash of the stack's ID and the network, within the 15
// characters an interface name may have. The default network hashes the ID
// alone; it cannot meet another network's key, which has one "/" more.
func BridgeName(stackID, network string) string {
	key := stackID
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

// ValidDomain tells whether name is a domain name of at least two labels:
// the host of an HTTP route, and of its certificate.
func ValidDomain(domainName string) bool {
	return domain.MatchString(domainName)
}

// ValidVariable tells whether name may name an environment variable.
func ValidVariable(variableName string) bool {
	return variable.MatchString(variableName)
}
